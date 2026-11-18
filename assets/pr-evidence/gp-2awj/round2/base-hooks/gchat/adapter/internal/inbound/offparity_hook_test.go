package inbound

// The off-parity hook at the PR's base, c9969436: the base has no arrival
// service and no stopwatch, so there is nothing to wire and every start
// that config.Load accepts starts. Copy this file over
// gchat/adapter/internal/inbound/offparity_hook_test.go in a tree of the base
// to re-run TestOffParityWithTheBase and TestOffParityStartsAsTheBase there.

import (
	"testing"

	"gchat-adapter/internal/config"
)

func offParityWire(t *testing.T, opts *Options, stateDir string, log func(string)) (wait func()) {
	return func() {}
}

func offParityStart(t *testing.T, cfg *config.Config, stateDir, executable string, log func(string)) error {
	return nil
}
