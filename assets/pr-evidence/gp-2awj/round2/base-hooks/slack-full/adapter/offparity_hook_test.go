package main

// The off-parity hook at the PR's base, c9969436: the base has no arrival
// service and no stopwatch, so the start has nothing more to run and the
// company gateway nothing to copy. Copy this file over
// slack-full/adapter/offparity_hook_test.go in a tree of the base to re-run
// TestOffParityWithTheBase and TestOffParityStartsAsTheBase there.

import "testing"

func offParityStart(t *testing.T, cfg *config, executable string) error { return nil }

func offParityCompany(t *testing.T, gw *companyGateway, started config) {}

func offParityWait(t *testing.T, cfg config) {}
