package main

// The off-parity hook at the PR head, 2ce179bd. No seam names the
// executable there, so offParityStart repeats the head's own start lines,
// arrival.go:39-62 (startArrival, called at main.go:1702), with the given
// executable in place of os.Executable. offParityCompany repeats main's copy
// of the started cfg into the company gateway (main.go:1914).

import (
	"os"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

func offParityStart(t *testing.T, cfg *config, executable string) error {
	if executable == "" {
		return startArrival(cfg)
	}
	arrivalKeyStatus.Store(cfg.arrivalChoice.KeyStatus()) // arrival.go:40-48
	if _, err := os.Stat(executable); err == nil {
		t.Fatal("the executable must be missing")
	}
	release, err := stopwatch.Release(executable)
	if err != nil {
		return err
	}
	_ = release
	return nil
}

func offParityCompany(t *testing.T, gw *companyGateway, started config) {
	gw.cfg.arrival = started.arrival
	gw.cfg.arrivalChoice = started.arrivalChoice
	gw.cfg.arrivalState = started.arrivalState
	gw.cfg.arrivalStarted = started.arrivalStarted
	gw.cfg.inboundJournal = started.inboundJournal
}

func offParityWait(t *testing.T, cfg config) {
	// The arrival tasks and the upload's files.info run detached.
	time.Sleep(500 * time.Millisecond)
	cfg.arrival.Wait()
	var _ *ack.Service = cfg.arrival
}
