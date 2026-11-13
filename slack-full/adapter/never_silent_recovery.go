package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func (g *companyGateway) neverSilentAdmission(env slackEventEnvelope, ev slackMessageEvent) bool {
	return g.cfg.neverSilent.Enabled() && ev.User != "" && ev.BotID == "" && ev.AppID == "" && len(ev.BotProfile) == 0 && ev.Subtype != "bot_message" && !strings.HasPrefix(env.EventID, "backfill:")
}

// The admission marker is durable in the primary receipt. Old receipts never
// acquire it. Frozen human targets exclude bot, ambient, denied and no-mention
// routes, including during terminal-inclusive startup recovery.
func (g *companyGateway) observeCompanyNeverSilent(r *IngressReceipt) error {
	e := g.cfg.neverSilent
	if !e.Enabled() || !r.NeverSilent {
		return nil
	}
	keys := make([]string, 0, len(r.Targets))
	for key, td := range r.Targets {
		local := td.City == "" || td.City == g.cfg.cityName
		mayor := td.Agent == "mayor" || td.Session == "mayor" || td.Session == g.cfg.cityName+"/mayor"
		if local && mayor && (td.Kind == wakeKindTargeted || td.Kind == "dm" || td.Kind == "mpim") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	targets := []string{}
	owner := ""
	delivered := false
	stopped := true
	definite := true
	unreachable := true
	failed := true
	at := time.Time{}
	for _, key := range keys {
		td := r.Targets[key]
		if td.Session != "" {
			targets = append(targets, td.Session)
		}
		if owner == "" || td.Agent == "mayor" {
			owner = td.Agent
		}
		if td.UpdatedAt.After(at) {
			at = td.UpdatedAt
		}
		delivered = delivered || (td.Status == companyTargetDelivered && td.NoticeKind == "delivered")
		stopped = stopped && td.Status == companyTargetFailed
		definite = definite && td.NoticeKind == "failed"
		unreachable = unreachable && td.NoticeKind == "unreachable"
		failed = failed && td.NoticeKind == "failed"
	}
	if owner == "" {
		return nil
	} // no reply identity is known yet
	id := neverSilentID(r.Origin.ChannelID, r.Origin.TS)
	thread := r.ThreadRootTS
	if thread == "" {
		thread = r.Origin.TS
	}
	if (r.Kind == receiptKindDM || r.Kind == receiptKindMpim) && thread == r.Origin.TS {
		thread = ""
	}
	if err := e.Track(neversilent.Message{ID: id, Conversation: r.Origin.ChannelID, Thread: thread, Created: r.ReceivedAt, Targets: targets, OwnerAgent: owner}); err != nil {
		return err
	}
	if at.IsZero() {
		at = r.UpdatedAt
	}
	kind := "pending"
	switch {
	case delivered:
		kind = "delivered"
	case unreachable:
		kind = "unreachable"
	case failed:
		kind = "failed"
	}
	if err := e.ResultAt(id, kind, targets, at); err != nil {
		return err
	}
	if stopped && !delivered {
		return e.StopAt(id, definite, at)
	}
	return nil
}

// Transport observation is deliberately narrower than the existing retry
// policy: timeouts, rate limits and a missing session are not proof of outage.
func companyNoticeTransport(status int, body []byte, err error) string {
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "unreachable"
		}
		return "unknown"
	}
	if status >= 500 {
		return "unreachable"
	}
	if status == 404 {
		var p struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &p) == nil && p.Code == "city-not-found" {
			return "unreachable"
		}
		return "unknown"
	}
	if status >= 400 && status < 500 && status != 408 && status != 429 {
		return "failed"
	}
	return "pending"
}

// Reconcile only enrolled legacy messages; log replay never admits traffic.
// StopAt rejects an old dead letter after a newer delivery/withdrawal.
func (cfg config) reconcileNeverSilent() error {
	if !cfg.neverSilent.Enabled() {
		return nil
	}
	if cfg.publishReceipts != nil {
		entries, err := os.ReadDir(cfg.publishReceipts.dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			r, err := cfg.publishReceipts.read(filepath.Join(cfg.publishReceipts.dir, entry.Name()))
			if err != nil {
				continue
			}
			if r.Delivered && r.NoticeSession != "" && !r.NoticeAt.IsZero() {
				if err = cfg.neverSilent.Act(r.ConversationID, r.ThreadTS, r.NoticeSession, r.NoticeAt); err != nil {
					return err
				}
			}
		}
	}
	if cfg.companyGateway != nil && cfg.companyGateway.store() != nil {
		receipts, err := cfg.companyGateway.store().List()
		if err != nil {
			return err
		}
		for _, r := range receipts {
			if err = cfg.companyGateway.observeCompanyNeverSilent(r); err != nil {
				return err
			}
		}
	}
	if cfg.inboundDeadLetterDir != "" {
		entries, err := os.ReadDir(cfg.inboundDeadLetterDir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			if err := cfg.reconcileNeverSilentDeadFile(filepath.Join(cfg.inboundDeadLetterDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	if cfg.inboundJournal != nil {
		paths, err := filepath.Glob(filepath.Join(cfg.inboundJournal.dir, "*.jsonl"))
		if err != nil {
			return err
		}
		for _, path := range paths {
			if err = scanNeverSilentRecords(path, func(line []byte) error {
				var row inboundJournalRecord
				if json.Unmarshal(line, &row) != nil || row.SessionAct == nil {
					return nil
				}
				a := row.SessionAct
				if a.Channel == "" || a.SessionID == "" || a.MessageTS == "" || a.Timestamp.IsZero() || a.NoticeNoActivity {
					return nil
				}
				if (a.Kind == "reply" && a.Source == "session_publish") || (a.Kind == "reaction" && a.Source == "session_react" && a.Emoji != "") {
					return cfg.neverSilent.Act(a.Channel, a.ThreadTS, a.SessionID, a.Timestamp)
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	// Company reply commands write a small local activity spool, separate from
	// their Slack side effects. It survives a restart and never contains text.
	if cfg.companyIntentsDir != "" {
		entries, err := os.ReadDir(cfg.companyIntentsDir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(cfg.companyIntentsDir, entry.Name()))
			if err != nil {
				return err
			}
			var intent struct {
				Status  string `json:"status"`
				Channel string `json:"channel_id"`
				Session string `json:"requester_session"`
				Posted  string `json:"posted_ts"`
			}
			if json.Unmarshal(body, &intent) != nil || intent.Status != "published" || intent.Channel == "" || intent.Session == "" {
				continue
			}
			at, ok := ingestionPublishTime(intent.Posted)
			if !ok {
				continue
			}
			sessions := []string{intent.Session}
			if strings.Contains(intent.Session, "__") {
				sessions = append(sessions, strings.ReplaceAll(intent.Session, "__", "."))
			} else if strings.Contains(intent.Session, ".") {
				sessions = append(sessions, strings.ReplaceAll(intent.Session, ".", "__"))
			}
			for _, session := range sessions {
				if err = cfg.neverSilent.Act(intent.Channel, "", session, at); err != nil {
					return err
				}
			}
		}
	}
	dir := companyStateDirDefault(cfg.cityPath, "never-silent-acts")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var a struct {
			Conversation string    `json:"conversation"`
			Thread       string    `json:"thread"`
			Session      string    `json:"session"`
			At           time.Time `json:"at"`
		}
		if err = json.Unmarshal(body, &a); err != nil {
			log.Print("never-silent: invalid activity record quarantined")
			_ = os.Rename(path, path+".invalid")
			continue
		}
		if a.Conversation == "" || a.Session == "" || a.At.IsZero() {
			log.Print("never-silent: invalid activity coordinates quarantined")
			_ = os.Rename(path, path+".invalid")
			continue
		}
		if err = cfg.neverSilent.Act(a.Conversation, a.Thread, a.Session, a.At); err != nil {
			return err
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (cfg config) reconcileNeverSilentDeadFile(path string) error {
	return scanNeverSilentRecords(path, func(line []byte) error {
		var r inboundDeadLetterRecord
		if json.Unmarshal(line, &r) != nil || r.DeadLetteredAt.IsZero() {
			return nil
		}
		for _, id := range neverSilentMembers(r.Inbound) {
			// Recovery writes its refusal in the history record. A refusal
			// that first stops a held message is definite, but a later refused
			// replay must not alter an already stopped message's certainty.
			refused := !cfg.neverSilent.IsStopped(id) && legacyPayloadRefusal(r.ReplayBlockedReason, r.AliasSessionID != "")
			if err := cfg.neverSilent.StopAt(id, r.NoticeDefinite || refused || cfg.neverSilent.DefinitelyFailed(id), r.DeadLetteredAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanNeverSilentRecords(path string, consume func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	for {
		var line []byte
		oversized := false
		var readErr error
		for {
			part, e := reader.ReadSlice('\n')
			readErr = e
			if len(line)+len(part) > maxInboundSpoolLineBytes {
				oversized = true
				line = nil
			}
			if !oversized {
				line = append(line, part...)
			}
			if !errors.Is(e, bufio.ErrBufferFull) {
				break
			}
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return nil
		}
		if oversized || !json.Valid(line) {
			log.Print("never-silent: corrupt historical observation skipped")
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			continue
		}
		if err = consume(line); err != nil {
			return err
		}
	}
}
