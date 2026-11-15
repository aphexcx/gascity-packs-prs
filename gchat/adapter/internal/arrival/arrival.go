// Package arrival builds what the adapter's start wires for the arrival
// acknowledgement and the stopwatch, so the start and the off-parity test
// reach the same constructor.
package arrival

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/stopwatch"
)

type Options struct {
	// Acknowledgement is config.Config.Acknowledgement, as config.Load reads it.
	Acknowledgement ack.Options
	// StateDir holds the stopwatch files and the attempt markers.
	StateDir string
	// Executable names the running binary for the release id; nil means
	// os.Executable. Tests name a file that cannot be read.
	Executable func() (string, error)
	Log        func(string)
	// Chat returns the client the adapter shares, nil until one is built.
	Chat func() *chat.Client
}

// New builds the acknowledgement service and its stopwatch writer. Only a
// setting the switch reads can refuse the start: a release that cannot be
// hashed is "unknown" with one warning (item 19). Attempt markers older than
// ack.MarkerAge are pruned here; the caller runs ack.KeepPruned for the daily
// pass and Closes the writer at shutdown.
func New(o Options) (*ack.Service, *stopwatch.Writer, error) {
	options := o.Acknowledgement
	options.Log = o.Log
	choice, err := ack.New(options)
	if err != nil {
		return nil, nil, err
	}
	executable := o.Executable
	if executable == nil {
		executable = os.Executable
	}
	watch := stopwatch.New(o.StateDir, "gchat", stopwatch.ReleaseOf(executable, o.Log), o.Log)
	ack.PruneAttempts(o.StateDir, time.Now())
	service := &ack.Service{Choice: choice, Dir: o.StateDir, Log: o.Log,
		Inbound: func(in ack.Input) {
			watch.Record(stopwatch.Event{Event: "inbound", Conversation: in.Conversation, Message: in.Message, Thread: in.Thread, MessageAt: in.Time, At: in.Seen})
		},
		Reaction: func(in ack.Input, how string, at time.Time) {
			watch.Record(stopwatch.Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at, Actor: "adapter", Choice: how})
		},
	}
	service.SendChoice = func(ctx context.Context, in ack.Input, choose func(context.Context) (ack.Row, string)) (string, error) {
		var c *chat.Client
		if o.Chat != nil {
			c = o.Chat()
		}
		if c == nil {
			return "", errors.New("chat unavailable")
		}
		// The react verb reacts as this same identity (Wire). The placement
		// record is written before the request and dropped only when Google
		// answers that it placed nothing, so the session's own reaction of
		// that emoji counts for never-silent however the request ends.
		return c.PlaceArrivalReaction(ctx, in.Message, in.Seen.Add(ack.ReactionBudget), ack.Default.Emoji,
			func(choiceCtx context.Context) (string, string) { row, how := choose(choiceCtx); return row.Emoji, how },
			func(emoji string) {
				if err := service.NotePlacing(in, emoji); err != nil && o.Log != nil {
					o.Log("ACK_REACTION: placement record unavailable; the session's same reaction will not count for never-silent")
				}
			},
			func(emoji string) { service.DropPlacing(in, emoji) })
	}
	return service, watch, nil
}

// Wire prepares a new client before it is shared: the words hook always (the
// stopwatch runs on or off, item 15); the shared arrival pace and the react
// verb's placement lookup and turn only while the acknowledgement is on, so
// the switch unset keeps the base's gate and the base's react verb.
func Wire(c *chat.Client, service *ack.Service, watch *stopwatch.Writer) {
	c.MessageCreated = func(space, message, thread, stamp string) {
		if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			watch.Record(stopwatch.Event{Event: "words", Conversation: space, Message: message, Thread: thread, At: at})
		}
	}
	if service != nil && service.Choice.Enabled() {
		c.EnableArrivalPace()
		c.ArrivalPlaced = func(message, emoji string, created bool) bool {
			// The acknowledgement keys the message by its space (inbound.go).
			space, _, _ := strings.Cut(message, "/messages/")
			if created {
				service.SessionPlaced(space, message, emoji)
				return true
			}
			return service.TakePlaced(space, message, emoji)
		}
		c.ArrivalRemoved = func(message, emoji string) {
			space, _, _ := strings.Cut(message, "/messages/")
			service.SessionRemoved(space, message, emoji)
		}
		c.ArrivalHold = func(message, emoji string) func() {
			space, _, _ := strings.Cut(message, "/messages/")
			return service.HoldReaction(space, message, emoji)
		}
	}
}
