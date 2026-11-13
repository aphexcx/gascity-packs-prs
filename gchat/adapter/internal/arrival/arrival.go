// Package arrival builds what the adapter's start wires for the arrival
// acknowledgement and the stopwatch, so the start and the off-parity test
// reach the same constructor.
package arrival

import (
	"context"
	"errors"
	"os"
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
		SendChoice: func(ctx context.Context, in ack.Input, choose func(context.Context) (ack.Row, string)) (string, error) {
			var c *chat.Client
			if o.Chat != nil {
				c = o.Chat()
			}
			if c == nil {
				return "", errors.New("chat unavailable")
			}
			return c.CreateArrivalReaction(ctx, in.Message, in.Seen.Add(ack.ReactionBudget), ack.Default.Emoji, func(choiceCtx context.Context) (string, string) { row, how := choose(choiceCtx); return row.Emoji, how })
		},
	}
	return service, watch, nil
}

// Wire prepares a new client before it is shared: the words hook always (the
// stopwatch runs on or off, item 15), and the shared arrival pace only while
// the acknowledgement is on, so the switch unset keeps the base's gate.
func Wire(c *chat.Client, service *ack.Service, watch *stopwatch.Writer) {
	c.MessageCreated = func(space, message, thread, stamp string) {
		if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			watch.Record(stopwatch.Event{Event: "words", Conversation: space, Message: message, Thread: thread, At: at})
		}
	}
	if service != nil && service.Choice.Enabled() {
		c.EnableArrivalPace()
	}
}
