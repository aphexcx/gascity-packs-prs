package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

var arrivalKeyStatus atomic.Value

func configureArrival(cfg *config, getenv func(string) string) error {
	choice, err := ack.New(ack.Options{Mode: getenv("ACK_REACTION"), Scope: getenv("ACK_SCOPE"), Base: getenv("JEV_API_BASE_URL"), Model: getenv("JEV_MODEL"), Key: getenv("JEV_API_KEY"), Catalog: ack.Catalog, Redact: redactInboundText, Log: func(s string) { log.Print(s) }})
	if err != nil {
		return err
	}
	if choice.Enabled() && cfg.companyVisibleAcks {
		return errors.New("ACK_REACTION and SLACK_COMPANY_VISIBLE_ACKS cannot both be set")
	}
	for _, name := range choice.Names() {
		if name == cfg.busyReaction {
			return errors.New("ACK_REACTION conflicts with BUSY_REACTION")
		}
	}
	cfg.arrivalChoice = choice
	cfg.arrivalState = getenv("GC_SERVICE_STATE_ROOT")
	if cfg.arrivalState == "" {
		cfg.arrivalState = companyStateDirDefault(cfg.cityPath, "")
	}
	return nil
}

// arrivalExecutable names the running binary for the release id. Tests name
// a file that cannot be read.
var arrivalExecutable = os.Executable

// startArrival never refuses the start: a release that cannot be hashed is
// "unknown" with one warning (item 19). It prunes old attempt markers, starts
// the daily pass, and leaves cfg.stopArrival for the shutdown.
func startArrival(cfg *config) error {
	arrivalKeyStatus.Store(cfg.arrivalChoice.KeyStatus())
	logf := func(s string) { log.Print(s) }
	watch := stopwatch.New(cfg.arrivalState, "slack", stopwatch.ReleaseOf(arrivalExecutable, logf), logf)
	ack.PruneAttempts(cfg.arrivalState, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	go ack.KeepPruned(ctx, cfg.arrivalState)
	cfg.stopArrival = func() { cancel(); watch.Close() }
	cfg.inboundJournal.watch = watch
	cfg.arrivalStarted = time.Now().UTC()
	cfg.arrival = &ack.Service{Choice: cfg.arrivalChoice, Dir: cfg.arrivalState, Log: func(s string) { log.Print(s) },
		Inbound: func(in ack.Input) {
			watch.Record(stopwatch.Event{Event: "inbound", Conversation: in.Conversation, Message: in.Message, Thread: in.Thread, MessageAt: in.Time, At: in.Seen})
		},
		Reaction: func(in ack.Input, how string, at time.Time) {
			watch.Record(stopwatch.Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at, Actor: "adapter", Choice: how})
		},
		Send: slackArrivalSender(cfg.slackBotToken),
	}
	return nil
}

func slackArrivalSender(token string) func(context.Context, ack.Input, ack.Row) error {
	return func(ctx context.Context, in ack.Input, row ack.Row) error {
		if token == "" {
			return errors.New("sender unavailable")
		}
		response, err := postReactionMethodContext(ctx, slackAPIClient, token, "reactions.add", slackReactionsAddReq{Channel: in.Conversation, Timestamp: in.Message, Name: row.Name})
		if err != nil {
			return errors.New("reaction request failed")
		}
		if !response.OK && response.Error != "already_reacted" {
			return errors.New("reaction refused")
		}
		return nil
	}
}

func observeSlackArrival(cfg config, env slackEventEnvelope, msg slackMessageEvent, addressed bool, send func(context.Context, ack.Input, ack.Row) error) {
	if cfg.arrival == nil {
		return
	}
	at, ok := ingestionPublishTime(msg.TS)
	if !ok {
		return
	}
	seen := env.receivedAt
	if seen.IsZero() {
		seen = time.Now()
	}
	thread := msg.ThreadTS
	in := ack.Input{Conversation: msg.Channel, Message: msg.TS, Thread: thread, Text: msg.Text, Time: at, Seen: seen, Addressed: addressed,
		Human: msg.User != "" && msg.BotID == "", Own: msg.User == env.botUserID(), Edit: (msg.Type != "message" && msg.Type != "app_mention") || (msg.Subtype != "" && msg.Subtype != "file_share" && msg.Subtype != "thread_broadcast"), Replay: env.replayed || cfg.recoveryReplay || strings.HasPrefix(env.EventID, "backfill:")}
	if env.timingOnly {
		// No sender: the service records the inbound line and stops there.
		cfg.arrival.ObserveUsing(in, nil)
	} else if send == nil {
		cfg.arrival.Observe(in)
	} else {
		cfg.arrival.ObserveUsing(in, send)
	}
}

// observeAdmittedArrival starts the arrival observation for a receipt the
// delivery worker admitted through the roster-drift overlay, from the
// receipt alone: first sight is its ReceivedAt, and a receipt admitted before
// this process's arrival service started is a replay, never a new arrival.
// A wake admitted past the reaction budget (a sweep's redrive) is timed for
// the stopwatch but gets no reaction.
func (g *companyGateway) observeAdmittedArrival(r *IngressReceipt, body json.RawMessage) {
	if g == nil || g.cfg.arrival == nil || r == nil {
		return
	}
	var ev slackMessageEvent
	if json.Unmarshal(body, &ev) != nil {
		return
	}
	env := slackEventEnvelope{Type: "event_callback", TeamID: r.Origin.TeamID, APIAppID: r.APIAppID, EventID: r.EventID, Event: body,
		receivedAt: r.ReceivedAt, replayed: g.cfg.arrivalStarted.IsZero() || r.ReceivedAt.Before(g.cfg.arrivalStarted),
		timingOnly: time.Since(r.ReceivedAt) > ack.ReactionBudget}
	g.observeArrival(env, ev, r)
}

// Company admission owns its route and its sender. Recovered receipts enter
// only through observeAdmittedArrival, as replays (nothing at all) or, when
// admitted past the reaction budget, timing-only; the durable attempt
// registry also absorbs paired event copies.
func (g *companyGateway) observeArrival(env slackEventEnvelope, ev slackMessageEvent, r *IngressReceipt) {
	if g == nil || g.cfg.arrival == nil || r == nil {
		return
	}
	// Admission and delivery can mutate the receipt after this call. Keep only
	// a private snapshot for the independent eligibility and sender lookup.
	receipt := *r
	go func() {
		addressed := isDMFamilyKind(receipt.Kind)
		dir := g.dirStore.Snapshot()
		msg := decodeCompanyMessage(receipt.Origin, env.Event)
		if addressed {
			// A stored receipt is not permission to send its text to Jev.
			// Reuse the DM worker's policy and MPIM mention decisions.
			if isBotAuthored(msg) || g.dmAuthorDecision(dir, &receipt, msg) != dmAuthorAllow {
				return
			}
			if receipt.Kind == receiptKindMpim && len(mpimWokenAgents(dir, msg)) == 0 {
				return
			}
		}
		if !addressed {
			decision := ComputeWakeSet(dir, msg, g.cfg.companySelfBotUserID)
			if decision.Room == nil {
				return
			}
			decision = g.addThreadAmbientWakes(dir, decision.Room, &receipt, msg, decision)
			if len(decision.Wakes) == 0 && len(decision.OverlayCandidates) > 0 {
				// Never the worker's counter, operator line or membership
				// probe: a fresh cache entry or nothing. On a cold cache the
				// worker starts this observation once it admits the wake.
				if members, ok := g.cachedChannelMembers(msg.ChannelID); ok {
					decision.Wakes = rosterDriftOverlay(decision, members)
				}
			}
			if len(decision.Wakes) == 0 {
				return
			}
			for _, wake := range decision.Wakes {
				if wake.Kind == wakeKindTargeted {
					addressed = true
				}
			}
		}
		// Missing custody never falls back to the switchboard for a DM.
		observeSlackArrival(g.cfg, env, ev, addressed, func(ctx context.Context, in ack.Input, row ack.Row) error {
			token, ok := g.ackActorToken(&receipt)
			if !ok {
				return errors.New("sender unavailable")
			}
			return slackArrivalSender(token)(ctx, in, row)
		})
	}()
}
