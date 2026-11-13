package inbound

// The off-parity hook at the PR head, 2ce179bd. No arrival constructor is
// factored out there, so this file repeats the head's own start lines:
// app/app.go:89-94 (the choice), :104-131 (state directory, release, the
// stopwatch writer and the arrival service) and app/workers.go:51-55 (the
// words hook installed on the chat client) and :86 (Ack in the Options).

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/stopwatch"
)

func offParityWire(t *testing.T, opts *Options, stateDir string, log func(string)) (wait func()) {
	arrivalOptions := opts.Config.Acknowledgement // app.go:89-94
	arrivalOptions.Log = log
	choice, err := ack.New(arrivalOptions)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable() // app.go:108-116
	if err != nil {
		t.Fatal(err)
	}
	release, err := stopwatch.Release(executable)
	if err != nil {
		t.Fatal(err)
	}
	watch := stopwatch.New(stateDir, "gchat", release, arrivalOptions.Log)
	c := opts.Chat
	service := &ack.Service{Choice: choice, Dir: stateDir, Log: arrivalOptions.Log, // app.go:117-131
		Inbound: func(in ack.Input) {
			watch.Record(stopwatch.Event{Event: "inbound", Conversation: in.Conversation, Message: in.Message, Thread: in.Thread, MessageAt: in.Time, At: in.Seen})
		},
		Reaction: func(in ack.Input, how string, at time.Time) {
			watch.Record(stopwatch.Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at, Actor: "adapter", Choice: how})
		},
		SendChoice: func(ctx context.Context, in ack.Input, choose func(context.Context) (ack.Row, string)) (string, error) {
			return c.CreateArrivalReaction(ctx, in.Message, in.Seen.Add(ack.ReactionBudget), ack.Default.Emoji, func(choiceCtx context.Context) (string, string) { row, how := choose(choiceCtx); return row.Emoji, how })
		},
	}
	c.MessageCreated = func(space, message, thread, stamp string) { // workers.go:51-55
		if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			watch.Record(stopwatch.Event{Event: "words", Conversation: space, Message: message, Thread: thread, At: at})
		}
	}
	opts.Ack = service // workers.go:86
	var _ *chat.Client = c
	return func() {
		// inbound.go:233 starts the space lookup in a goroutine the service
		// does not track; let it reach Observe, then wait for the service.
		time.Sleep(200 * time.Millisecond)
		service.Wait()
	}
}

func offParityStart(t *testing.T, cfg *config.Config, stateDir, executable string, log func(string)) error {
	arrivalOptions := cfg.Acknowledgement // app.go:89-94
	arrivalOptions.Log = log
	if _, err := ack.New(arrivalOptions); err != nil {
		return err
	}
	if executable == "" { // app.go:108-115
		var err error
		if executable, err = os.Executable(); err != nil {
			return errors.New("stopwatch: executable unavailable")
		}
	}
	release, err := stopwatch.Release(executable)
	if err != nil {
		return errors.New("stopwatch: executable digest unavailable")
	}
	stopwatch.New(stateDir, "gchat", release, arrivalOptions.Log)
	return nil
}
