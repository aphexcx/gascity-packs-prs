package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIngestionNotifyAcceptanceRecovery(t *testing.T) {
	refusal := exec.Command("/bin/sh", "-c", "printf 'fixture refused' >&2; exit 17")
	_, refused := refusal.Output()
	if refused == nil {
		t.Fatal("expected fixture refusal")
	}
	_, reset := exec.Command("/bin/sh", "-c", "printf 'read: connection reset by peer' >&2; exit 1").Output()
	_, killed := exec.Command("/bin/sh", "-c", "kill -KILL $$").Output()
	for _, mode := range []string{"refused", "not_started", "connection_refused", "not_attempted", "ambiguous", "ambiguous_reset", "ambiguous_signal", "ambiguous_then_refused"} {
		proofs := []string{"A", "both"}
		if mode == "not_attempted" {
			proofs = append(proofs, "none") // Restart retries the entire untouched plan.
		}
		for _, proof := range proofs {
			t.Run(mode+"/"+proof, func(t *testing.T) {
				at := time.Now().UTC()
				now := at
				j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return now }}
				r := ingestionTestRecord("200.1", at, "notify acceptance")
				r.CanonicalSessionID = "s1"
				r.Windows = []injectionWindow{{MessageTS: "100.1", ThreadTS: "100.0"}, {MessageTS: "200.1", ThreadTS: "200.0"}}
				if err := j.append(r); err != nil {
					t.Fatal(err)
				}
				now = at.Add(ingestionWindow + time.Second)
				attempts := 0
				accepted := make(map[string]bool)
				named := make(map[string]int)
				o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
					fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
					notify: func(_ context.Context, rows []inboundJournalRecord) error {
						attempts++
						if attempts == 1 && mode == "not_started" {
							return &exec.Error{Name: "gc", Err: exec.ErrNotFound}
						}
						if attempts == 1 && mode == "connection_refused" {
							return syscall.ECONNREFUSED
						}
						if (mode == "refused" && attempts == 1) || (mode == "ambiguous_then_refused" && attempts == 2) {
							return refused
						}
						id := rows[0].AlertBatchID
						if mode == "not_attempted" && proof == "none" && (attempts != 1 || len(ingestionAlertWindows(rows[0])) != 2) {
							t.Fatal("unattempted plan was not retried in full exactly once")
						}
						if !accepted[id] {
							accepted[id] = true
							for _, row := range rows {
								for _, w := range ingestionAlertWindows(row) {
									named[w.MessageTS]++
								}
							}
						}
						if strings.HasPrefix(mode, "ambiguous") && attempts == 1 {
							if mode == "ambiguous_reset" {
								return reset
							}
							if mode == "ambiguous_signal" {
								return killed
							}
							return context.DeadlineExceeded
						}
						return nil
					}}
				var firstID string
				if mode == "not_attempted" {
					batch, _, err := o.prepareAlertBatch(r.Channel, []inboundJournalRecord{r})
					if err != nil {
						t.Fatal(err)
					}
					firstID = batch.ID // Crash after durable planning, before notify.
				} else if err := o.step(context.Background()); err == nil {
					t.Fatal("expected first attempt failure")
				}
				for _, row := range ingestionJournalRows(t, j) {
					if row.Phase == "ingestion_batch" {
						firstID = row.AlertBatch.ID
						break
					}
				}
				now = now.Add(5 * time.Second) // Round-six ruling: first eligible refusal retry.
				for i, w := range r.Windows {
					if (proof == "A" && i == 0) || proof == "both" {
						recordDeliveredPublish(config{inboundJournal: j}, r.Channel, w.ThreadTS, fmt.Sprintf("%d.000000", now.Unix()), "s1")
					}
				}
				restart := func() {
					j = &inboundJournal{dir: j.dir, now: func() time.Time { return now }}
					o.cfg.inboundJournal = j
				}
				restart()
				if mode == "ambiguous_then_refused" && proof != "both" {
					if err := o.step(context.Background()); err == nil {
						t.Fatal("expected refusal of the ambiguous attempt's retry")
					}
					now = now.Add(5 * time.Second) // The refused retry also backs off.
					restart()
				}
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				for _, row := range ingestionJournalRows(t, j) {
					if row.Phase == "ingestion_batch_done" && row.AlertBatch.ID == firstID {
						if row.AlertBatch.Unconfirmed != strings.HasPrefix(mode, "ambiguous") {
							t.Fatalf("wrong ambiguous coverage marker: %+v", row.AlertBatch)
						}
					}
				}
				late := r
				late.TargetSessions, late.CanonicalSessionID = []string{"s2"}, "s2"
				if err := j.append(late); err != nil {
					t.Fatal(err)
				}
				restart()
				for i := 0; i < 3; i++ {
					if err := o.step(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if named["100.1"] != 1 || named["200.1"] != 1 || len(named) != 2 {
					t.Fatalf("refused/unattempted mail hid a late target or repeated an accepted line: %v", named)
				}
			})
		}
	}
}

func TestIngestionBatchAttemptIsDurableBeforeNotify(t *testing.T) {
	at := time.Now().UTC()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { return at }}
	r := ingestionTestRecord("100.1", at.Add(-ingestionWindow-time.Second), "attempt order")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return at },
		fetch: func(context.Context, string) ([]json.RawMessage, error) { return nil, nil },
		notify: func(_ context.Context, rows []inboundJournalRecord) error {
			state := ingestionReadAttempt(t, &inboundJournal{dir: j.dir}, rows[0].AlertBatchID)
			if state.Refused || state.ID != rows[0].AlertBatchID || state.AttemptedAt.IsZero() {
				t.Fatal("notify ran without its durable attempt")
			}
			return nil
		}}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIngestionMailRefusalRetainsExitOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte("#!/bin/sh\nprintf 'fixture refusal' >&2\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	o := &ingestionObserver{}
	err := o.mailUnseen(context.Background(), []inboundJournalRecord{ingestionTestRecord("100.1", time.Now(), "mail")})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 || string(exit.Stderr) != "fixture refusal" {
		t.Fatalf("definite refusal lost its process evidence: %v", err)
	}
}
