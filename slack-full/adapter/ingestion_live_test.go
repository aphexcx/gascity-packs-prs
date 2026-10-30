package main

// Opt-in operational proof, never part of ordinary test runs. The mayor must
// reserve the thread and busy interval first. This sends clearly labelled
// synthetic events to the existing adapter; it does not replace that service.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIngestionLiveBusySessionProof(t *testing.T) {
	path := os.Getenv("GC_SLACK_INGESTION_PROOF_SPEC")
	if path == "" {
		t.Skip("requires a mayor-coordinated live proof spec")
	}
	var spec struct {
		Channel   string    `json:"channel"`
		ThreadTS  string    `json:"thread_ts"`
		UserID    string    `json:"user_id"`
		SessionID string    `json:"session_id"`
		EventsURL string    `json:"events_url"`
		Start     time.Time `json:"start"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Channel == "" || spec.ThreadTS == "" || spec.UserID == "" || spec.SessionID == "" || spec.Start.IsZero() {
		t.Fatal("incomplete proof spec")
	}
	u, err := url.Parse(spec.EventsURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/slack/events" {
		t.Fatal("proof requires the local adapter events endpoint")
	}
	if time.Until(spec.Start) < 0 || time.Until(spec.Start) > 10*time.Minute {
		t.Fatal("proof start must be within the next ten minutes")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.slackSigningKey == "" {
		t.Fatal("signing key unavailable")
	}
	ctx, cancel := context.WithDeadline(context.Background(), spec.Start.Add(4*time.Minute))
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second}
	marker := "gp-41p6 synthetic ingestion proof " + spec.Start.UTC().Format(time.RFC3339)
	var timestamps []string
	for i := 0; i < 3; i++ {
		delay := time.Until(spec.Start.Add(time.Duration(i) * 30 * time.Second))
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		ts := fmt.Sprintf("%d.%06d", time.Now().Unix(), i+1)
		timestamps = append(timestamps, ts)
		event, _ := json.Marshal(slackMessageEvent{Type: "message", User: spec.UserID, Channel: spec.Channel, ThreadTS: spec.ThreadTS, TS: ts, Text: fmt.Sprintf("%s line %d/3. Diagnostic only; no founder instruction. @mayor-citadel", marker, i+1)})
		body, _ := json.Marshal(slackEventEnvelope{Type: "event_callback", TeamID: cfg.accountID, EventID: "Ev-gp-41p6-" + ts, Event: event})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.EventsURL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", signFor(cfg.slackSigningKey, stamp, body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event HTTP %d", resp.StatusCode)
		}
		t.Logf("synthetic event accepted channel=%s thread=%s ts=%s", spec.Channel, spec.ThreadTS, ts)
	}

	// On a pre-upgrade adapter, obtain expected payloads from gc's external
	// ingress store, independently of the harness being checked. Import only
	// these three receipt fingerprints, explicitly labelled proof_import.
	imported := map[string]bool{}
	observer := &ingestionObserver{cfg: cfg, now: time.Now}
	observer.fetch, observer.notify = observer.fetchTranscript, observer.mailUnseen
	for ctx.Err() == nil {
		q := url.Values{"scope_id": {cfg.cityName}, "provider": {"slack"}, "account_id": {cfg.accountID}, "conversation_id": {spec.Channel}, "kind": {"room"}, "order": {"desc"}, "limit": {"30"}}
		endpoint := cfg.gcAPIBase + "/v0/city/" + url.PathEscape(cfg.cityName) + "/extmsg/transcript?" + q.Encode()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, ingestionReadLimit+1))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || len(data) > ingestionReadLimit {
			t.Fatalf("external transcript unavailable: status=%d err=%v", resp.StatusCode, err)
		}
		var external struct {
			Items []struct {
				ProviderMessageID string
				Text              string
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &external); err != nil {
			t.Fatal(err)
		}
		payloads := map[string]string{}
		for _, item := range external.Items {
			if strings.Contains(item.Text, marker) {
				payloads[item.ProviderMessageID] = item.Text
			}
		}
		files, err := filepath.Glob(filepath.Join(cfg.inboundJournal.dir, "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var imports []inboundJournalRecord
		proven := map[string]bool{}
		for _, name := range files {
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			scanner := bufio.NewScanner(io.LimitReader(f, 32<<20))
			scanner.Buffer(make([]byte, 4096), 1<<20)
			for scanner.Scan() {
				var r inboundJournalRecord
				if json.Unmarshal(scanner.Bytes(), &r) != nil {
					continue
				}
				selected := false
				for _, ts := range timestamps {
					if r.MessageTS == ts {
						selected = true
					}
				}
				if !selected || r.Channel != spec.Channel || r.ThreadTS != spec.ThreadTS {
					continue
				}
				if len(r.TargetSessions) != 1 || r.TargetSessions[0] != spec.SessionID {
					f.Close()
					t.Fatal("proof routed to an unexpected target")
				}
				if r.Status == "ingested" && r.Evidence != nil {
					proven[r.MessageTS] = true
				}
				if r.Status == "delivered_unseen" {
					f.Close()
					t.Fatalf("proof failed: delivered_unseen receipt=%s ts=%s", r.ReceiptID, r.MessageTS)
				}
				payload, ok := payloads[r.MessageTS]
				if r.Status != "delivered" || imported[r.MessageTS] || !ok {
					continue
				}
				if len(payload) != r.Bytes {
					f.Close()
					t.Fatal("external payload length differs from handoff journal")
				}
				if r.PayloadSHA256 == "" {
					r.DeliveredAt = r.TS
					r.PayloadSHA256 = ingestionDigest(ingestionSanitize(payload))
					r.ProofKind, r.Phase = "extmsg", "proof_import"
					imports = append(imports, r)
				}
				imported[r.MessageTS] = true
			}
			err = scanner.Err()
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range imports {
			if err := cfg.inboundJournal.append(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := observer.step(ctx); err != nil {
			t.Fatal(err)
		}
		if len(proven) == 3 {
			t.Logf("all three live receipts ingested: %v", timestamps)
			return
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	t.Fatalf("proof did not reach three ingested receipts: %v", ctx.Err())
}
