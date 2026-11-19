package inbound

// The off-parity hook at the tip: it calls the adapter's own arrival
// constructor, arrival.New and arrival.Wire, exactly as app/app.go and
// app/workers.go do at the start, and sets Ack in the Options as
// app/workers.go builds them. The base's version of this file does nothing.

import (
	"errors"
	"os"
	"testing"
	"time"

	"gchat-adapter/internal/arrival"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
)

func offParityWire(t *testing.T, opts *Options, stateDir string, log func(string)) (wait func()) {
	c := opts.Chat
	service, watch, err := arrival.New(arrival.Options{Acknowledgement: opts.Config.Acknowledgement, StateDir: stateDir, Log: log,
		Chat: func() *chat.Client { return c }})
	if err != nil {
		t.Fatal(err)
	}
	arrival.Wire(c, service, watch)
	opts.Ack = service
	t.Cleanup(watch.Close)
	return func() {
		// inbound.go starts the space lookup in a goroutine the service does
		// not track; let it reach Observe, then wait for the service.
		time.Sleep(200 * time.Millisecond)
		service.Wait()
		watch.Flush()
	}
}

func offParityStart(t *testing.T, cfg *config.Config, stateDir, executable string, log func(string)) error {
	o := arrival.Options{Acknowledgement: cfg.Acknowledgement, StateDir: stateDir, Log: log}
	if executable != "" {
		o.Executable = func() (string, error) {
			if _, err := os.Stat(executable); err == nil {
				return "", errors.New("the executable must be missing")
			}
			return executable, nil
		}
	}
	_, watch, err := arrival.New(o)
	if err == nil {
		watch.Close()
	}
	return err
}
