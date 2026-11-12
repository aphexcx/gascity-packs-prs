package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func newNeverSilent(cfg config, identities *identityRegistry, aliases *handleAliasRegistry) (*neversilent.Engine, error) {
	health := os.Getenv("MAYOR_HEALTH_FILE")
	if health == "" {
		health = filepath.Join(cfg.cityPath, ".gc", "mayor-health.json")
	}
	return neversilent.New(neversilent.Options{Enabled: os.Getenv("NEVER_SILENT") == "on", City: cfg.cityName, Channel: "slack", Path: companyStateDirDefault(cfg.cityPath, "never-silent.json"), HealthFile: health,
		OperatorContext: os.Getenv("NEVER_SILENT_OPERATOR_CONTEXT"), OperatorAgent: os.Getenv("NEVER_SILENT_OPERATOR_AGENT"), GC: os.Getenv("GC_BIN"),
		Post: func(_ context.Context, m neversilent.Message, line string) error {
			post := slackPostMessageReq{Channel: m.Conversation, ThreadTS: m.Thread, Text: line}
			targets := append([]string{}, m.Targets...)
			if aliases != nil {
				if session, ok := aliases.Get("mayor"); ok {
					targets = append(targets, session)
				}
			}
			targets = append(targets, "mayor")
			if identities != nil {
				for _, session := range targets {
					if identity, ok := identities.Get(session); ok {
						post.Username, post.IconURL, post.IconEmoji = identity.Username, identity.IconURL, identity.IconEmoji
						break
					}
				}
			}
			token := cfg.slackBotToken
			if m.OwnerAgent != "" {
				var err error
				token, err = loadOwnerBotToken(cfg.companySecretsDir, m.OwnerAgent)
				if err != nil {
					return errors.New("Slack notice owner identity unavailable")
				}
				post.Username, post.IconURL, post.IconEmoji = "", "", ""
			}
			if m.CanPost != nil && !m.CanPost() {
				return context.Canceled
			}
			result, err := postToSlack(token, post)
			if err != nil {
				return errors.New("Slack notice outcome unavailable")
			}
			if !result.OK {
				return errors.New("Slack refused notice")
			}
			return nil
		},
		SendLocal: func(ctx context.Context, m neversilent.Message, line string) error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			bin := os.Getenv("GC_BIN")
			if bin == "" {
				bin = "gc"
			}
			key := fmt.Sprintf("slack-owner-notice:%x", sha256.Sum256([]byte(m.ID+m.Created.Format(time.RFC3339Nano))))
			body := fmt.Sprintf("The adapter told the owner: %q\nChannel: %s\nThread: %s\nStatus: gc slack status\n", line, m.Conversation, m.Thread)
			cmd := exec.CommandContext(ctx, bin, "mail", "send", "mayor", "--dedup", key, "-s", "slack: owner delivery notice", "-m", body)
			cmd.WaitDelay = time.Second
			return cmd.Run()
		},
	})
}

func neverSilentID(channel, ts string) string { return channel + ":" + ts }
func (cfg config) noticeError(err error) {
	if err != nil {
		log.Print("never-silent: durable observation failed")
	}
}

// Record actual words independently of readback/proof and its optional journal.
// The activity spool survives a failed sidecar write without retrying the post.
func (cfg config) recordNeverSilentWords(conversation, thread, session, proof, words string, at time.Time) {
	if !cfg.neverSilent.Enabled() || strings.TrimSpace(session) == "" || strings.TrimSpace(words) == "" || at.IsZero() {
		return
	}
	a := struct {
		Conversation string    `json:"conversation"`
		Thread       string    `json:"thread"`
		Session      string    `json:"session"`
		At           time.Time `json:"at"`
	}{conversation, thread, session, at}
	body, err := json.Marshal(a)
	if err == nil {
		key := fmt.Sprintf("%x.json", sha256.Sum256([]byte(conversation+"\x00"+session+"\x00"+proof+"\x00"+at.UTC().Format(time.RFC3339Nano))))
		err = companyWriteFileAtomic(filepath.Join(companyStateDirDefault(cfg.cityPath, "never-silent-acts"), key), body)
	}
	cfg.noticeError(err)
	cfg.noticeError(cfg.neverSilent.Act(conversation, thread, session, at))
}

func (cfg config) trackNeverSilent(m externalInboundMessage, eventID string, addressed bool) {
	if cfg.neverSilent == nil || !addressed || m.Actor.IsBot || strings.HasPrefix(eventID, "backfill:") {
		return
	}
	if m.ExplicitTarget != "" && m.ExplicitTarget != "mayor" {
		return
	}
	thread := m.ReplyToMessageID
	if thread == "" {
		thread = m.ProviderMessageID
	}
	cfg.noticeError(cfg.neverSilent.Track(neversilent.Message{ID: neverSilentID(m.Conversation.ConversationID, m.ProviderMessageID), Conversation: m.Conversation.ConversationID, Thread: thread, Created: time.Now()}))
}

func neverSilentMembers(m externalInboundMessage) []string {
	ids := []string{neverSilentID(m.Conversation.ConversationID, m.ProviderMessageID)}
	for _, w := range m.injectionWindows {
		id := neverSilentID(m.Conversation.ConversationID, w.MessageTS)
		if w.MessageTS != "" && id != ids[0] {
			ids = append(ids, id)
		}
	}
	return ids
}

func (cfg config) observeNeverSilent(m externalInboundMessage, r deliveryReceipt, postErr error, concluded ...bool) {
	if cfg.neverSilent == nil {
		return
	}
	kind := r.status
	if kind == "delivered" && r.verdict(true) != receiptVouched {
		kind = "pending"
	}
	if kind == "failed" {
		for _, member := range r.members {
			if member.Delivered > 0 || member.Status == "partial" {
				kind = "pending"
			}
		}
	}
	targets := []string{}
	for _, member := range r.members {
		if member.SessionID != "" {
			targets = append(targets, member.SessionID)
		}
	}
	if postErr != nil {
		kind = "other"
		var httpErr *inboundPostError
		var dial *net.OpError
		if errors.As(postErr, &httpErr) {
			if httpErr.Status >= 500 {
				kind = "unreachable"
			}
			if httpErr.Status == 404 {
				var problem struct {
					Code   string `json:"code"`
					Detail string `json:"detail"`
				}
				if json.Unmarshal([]byte(httpErr.Body), &problem) == nil && (problem.Code == "city-not-found" || strings.HasPrefix(problem.Detail, "not_found: city not found or not running")) {
					kind = "unreachable"
				}
			}
		} else if errors.As(postErr, &dial) && dial.Op == "dial" {
			kind = "unreachable"
		}
	} else if r.status == "pending" && len(r.members) > 0 && len(concluded) > 0 && concluded[0] {
		whole := true
		for _, member := range r.members {
			if !member.landedWhole() {
				whole = false
			}
		}
		if whole {
			kind = "delivered"
		}
	}
	for _, id := range neverSilentMembers(m) {
		cfg.noticeError(cfg.neverSilent.Result(id, kind, targets))
	}
}

func (cfg config) stopNeverSilent(m externalInboundMessage, definite bool) {
	for _, id := range neverSilentMembers(m) {
		cfg.noticeError(cfg.neverSilent.Stop(id, definite || cfg.neverSilent.DefinitelyFailed(id)))
	}
}

func (cfg config) runNeverSilent(ctx context.Context) {
	if cfg.neverSilent == nil {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := cfg.reconcileNeverSilent(); err != nil {
				cfg.noticeError(err)
				continue
			}
			cfg.noticeError(cfg.neverSilent.Poll(ctx))
		}
	}
}

func (cfg config) neverSilentStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg.neverSilent.Status())
}
