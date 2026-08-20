package main

import (
	"strings"
	"testing"
)

// gp-729 item 2: priors the audience already received as their own
// inbounds collapse to a one-line note instead of a full re-quote.

func dedupReplies() []slackThreadMessage {
	return []slackThreadMessage{
		{User: "U1", Text: "parent message", TS: "1.0"},
		{User: "U2", Text: "first reply", TS: "2.0"},
		{User: "U1", Text: "second reply", TS: "3.0"},
		{User: "U1", Text: "current", TS: "4.0"},
	}
}

func TestPreambleCollapsesDeliveredParent(t *testing.T) {
	delivered := map[string]bool{"1.0": true}
	got := formatThreadContextPreamble(dedupReplies(), "4.0", "", nil,
		func(ts string) bool { return delivered[ts] })

	if !strings.Contains(got, "1 earlier message already delivered (newest ts 1.0) — not re-quoted.") {
		t.Fatalf("missing collapse line:\n%s", got)
	}
	if strings.Contains(got, "parent message") {
		t.Fatalf("delivered parent must not be re-quoted:\n%s", got)
	}
	for _, want := range []string{"@U2: first reply", "@U1: second reply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("undelivered prior %q must keep the full quote:\n%s", want, got)
		}
	}
}

func TestPreambleAllDeliveredCollapsesToOneLine(t *testing.T) {
	got := formatThreadContextPreamble(dedupReplies(), "4.0", "", nil,
		func(string) bool { return true })
	if !strings.Contains(got, "3 earlier messages already delivered (newest ts 3.0) — not re-quoted.") {
		t.Fatalf("missing collapse line:\n%s", got)
	}
	for _, quoted := range []string{"parent message", "first reply", "second reply"} {
		if strings.Contains(got, quoted) {
			t.Fatalf("fully-delivered thread must quote nothing, found %q:\n%s", quoted, got)
		}
	}
	if !strings.HasSuffix(got, "\n---\n\n") {
		t.Fatalf("preamble must keep its separator:\n%q", got)
	}
}

func TestPreambleNilFilterQuotesEverything(t *testing.T) {
	got := formatThreadContextPreamble(dedupReplies(), "4.0", "", nil, nil)
	if !strings.Contains(got, "Thread context (3 earlier messages):") {
		t.Fatalf("nil filter must keep pre-gp-729 shape:\n%s", got)
	}
	if strings.Contains(got, "not re-quoted") {
		t.Fatalf("nil filter must not emit collapse lines:\n%s", got)
	}
	for _, want := range []string{"parent message", "first reply", "second reply"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing full quote %q:\n%s", want, got)
		}
	}
}

func TestPreambleNeverSeenAudienceQuotesInFull(t *testing.T) {
	// Conservative direction: an empty delivered-set (fresh restart,
	// alias audience) re-quotes everything rather than losing context.
	d := newDeliveredIDs()
	got := formatThreadContextPreamble(dedupReplies(), "4.0", "", nil,
		func(ts string) bool { return d.seen("mayor", "C1", ts) })
	if !strings.Contains(got, "Thread context (3 earlier messages):") {
		t.Fatalf("never-seen audience must get the full window:\n%s", got)
	}
}
