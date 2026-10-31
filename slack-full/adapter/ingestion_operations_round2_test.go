package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIngestionMailExecDeduplicatesFanoutAndRetriesFailure(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	// Exercise exec.CommandContext and its exact argv without sending mail.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$INGESTION_TEST_ARGS\"\nexit \"$INGESTION_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("INGESTION_TEST_ARGS", argsPath)
	t.Setenv("INGESTION_TEST_EXIT", "17")
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
	r := ingestionTestRecord("100.1", at, "private payload must not enter mail")
	r.TargetSessions = []string{"s1", "s2"}
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(ingestionWindow + time.Second)
	o := &ingestionObserver{cfg: config{inboundJournal: j, cityPath: "/fixture/city"}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil }}
	o.notify = o.mailUnseen
	if err := o.step(context.Background()); err == nil {
		t.Fatal("failed mail executable was treated as an alert")
	}
	pending, err := o.pending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("lost retryable fanout: %v %v", pending, err)
	}
	for _, row := range pending {
		if row.Alerted {
			t.Fatal("failed exec marked receipt alerted")
		}
	}
	t.Setenv("INGESTION_TEST_EXIT", "0")
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	// Two failed attempts (one per member), then one successful shared
	// receipt alert; late observation must never call the executable again.
	if len(args) != 3*11 {
		t.Fatalf("unexpected mail calls/argv: %q", args)
	}
	for i := 0; i < len(args); i += 11 {
		if !reflect.DeepEqual(args[i:i+6], []string{"--city", "/fixture/city", "mail", "send", "mayor", "--dedup"}) ||
			args[i+6] != args[6] || !strings.HasPrefix(args[i+6], "slack-ingestion:") ||
			args[i+7] != "-s" || args[i+8] != "Slack delivered_unseen" || args[i+9] != "-m" ||
			!strings.Contains(args[i+10], "message_ts=100.1") || strings.Contains(args[i+10], "private payload") {
			t.Fatalf("incorrect alert invocation: %q", args[i:i+11])
		}
	}
}

func TestIngestionTornJournalLineDoesNotHideCompleteRecords(t *testing.T) {
	at := time.Now().UTC()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return at }}
	r := ingestionTestRecord("100.1", at, "body")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(j.dir, at.Format("20060102")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"status":"inges`)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("torn-line fixture: %v %v", writeErr, closeErr)
	}
	var warnings bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return at },
		fetch: func(context.Context, string) ([]json.RawMessage, error) {
			return []json.RawMessage{ingestionTestEntry(t, r, "body", "attachment")}, nil
		},
		notify: func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected alert"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := o.pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("torn line hid the durable ingestion result: %v %v", pending, err)
	}
	if !strings.Contains(warnings.String(), "skipped 1 malformed journal line(s)") {
		t.Fatalf("missing counted warning: %s", warnings.String())
	}
	if strings.Contains(warnings.String(), `{"status"`) {
		t.Fatal("warning exposed journal contents")
	}
}

func TestIngestionDisabledRetainsNormallyAndCanBeReenabled(t *testing.T) {
	t.Setenv("SLACK_INGESTION_OBSERVER_DISABLED", "1")
	at := time.Date(2026, 10, 30, 23, 59, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
	old := ingestionTestRecord("100.1", at, "old body")
	if err := j.append(old); err != nil {
		t.Fatal(err)
	}
	now = at.Add(2 * time.Minute)
	current := ingestionTestRecord("100.2", now, "current body")
	if err := j.append(current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(j.dir, at.Format("20060102")+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("disabled observer pinned an expired handoff: %v", err)
	}
	rows := ingestionJournalRows(t, j)
	if len(rows) != 1 || rows[0].MessageTS != current.MessageTS {
		t.Fatalf("disabled mode must continue normal journaling: %v", rows)
	}
	// Re-enable as on a later adapter start. Retained deliveries can still
	// become observations; expired data does not pin or poison reconstruction.
	t.Setenv("SLACK_INGESTION_OBSERVER_DISABLED", "0")
	j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) {
			return []json.RawMessage{ingestionTestEntry(t, current, "current body", "attachment")}, nil
		},
		notify: func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected alert"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.MessageTS == current.MessageTS && row.Status == "ingested" {
			return
		}
	}
	t.Fatal("re-enabled observer failed to record retained input")
}
