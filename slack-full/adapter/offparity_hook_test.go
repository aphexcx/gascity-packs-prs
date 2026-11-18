package main

// The off-parity hook at the tip: offParityStart is main's own start step,
// startArrival (main.go, after loadConfig), with the executable seam naming
// a missing file when the test asks for one; offParityCompany repeats main's
// copy of the started cfg into the company gateway (main.go, newCompanyGateway).
// The base's version of this file does nothing.

import (
	"errors"
	"os"
	"testing"
	"time"
)

func offParityStart(t *testing.T, cfg *config, executable string) error {
	if executable != "" {
		prev := arrivalExecutable
		arrivalExecutable = func() (string, error) {
			if _, err := os.Stat(executable); err == nil {
				return "", errors.New("the executable must be missing")
			}
			return executable, nil
		}
		defer func() { arrivalExecutable = prev }()
	}
	err := startArrival(cfg)
	if cfg.stopArrival != nil {
		t.Cleanup(cfg.stopArrival)
	}
	return err
}

func offParityCompany(t *testing.T, gw *companyGateway, started config) {
	gw.cfg.arrival = started.arrival
	gw.cfg.arrivalChoice = started.arrivalChoice
	gw.cfg.arrivalState = started.arrivalState
	gw.cfg.arrivalStarted = started.arrivalStarted
	gw.cfg.inboundJournal = started.inboundJournal
}

func offParityWait(t *testing.T, cfg config) {
	// The arrival tasks and an upload's stopwatch record run detached.
	time.Sleep(500 * time.Millisecond)
	cfg.arrival.Wait()
	cfg.inboundJournal.watch.Flush()
}
