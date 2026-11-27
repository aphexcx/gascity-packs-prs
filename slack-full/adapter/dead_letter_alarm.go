package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	alarmRowHorizon   = 24 * time.Hour
	alarmRetention    = 48 * time.Hour
	alarmCommandLimit = 120 * time.Second
	alarmPassInterval = 10 * time.Second
	alarmRecordsFile  = "dead-letter-alarms.json"
)

type alarmMessage struct {
	channel, ts, thread, state, composer string
	row, held                            time.Time
	class                                int
	marked                               bool
}

type alarmRecord struct {
	Raised        time.Time `json:"raised"`
	Closed        bool      `json:"closed,omitempty"`
	LocalMailed   bool      `json:"local_mailed,omitempty"`
	LocalAsked    bool      `json:"local_asked,omitempty"`
	LocalEnded    bool      `json:"local_ended,omitempty"`
	LocalTries    int       `json:"local_tries"`
	LocalNext     time.Time `json:"local_next,omitzero"`
	Operator      string    `json:"operator"`
	OperatorTries int       `json:"operator_tries"`
	OperatorNext  time.Time `json:"operator_next,omitzero"`
}

func (r alarmRecord) wake() string {
	if !r.LocalMailed {
		return "not_known"
	}
	if r.LocalAsked {
		return "asked"
	}
	return "not_asked"
}

type deadLetterAlarm struct {
	cfg                        config
	path, bin, operator, agent string
	limit, interval            time.Duration
	now                        func() time.Time
	mu                         sync.Mutex // protects records and pending local sends
	records                    map[string]alarmRecord
	sending                    map[string]chan struct{}
	dirty                      bool
}

func newDeadLetterAlarm(cfg config) *deadLetterAlarm {
	a := &deadLetterAlarm{cfg: cfg, now: time.Now, limit: alarmCommandLimit, interval: alarmPassInterval,
		bin: os.Getenv("GC_BIN"), operator: os.Getenv("NEVER_SILENT_OPERATOR_CONTEXT"), agent: os.Getenv("NEVER_SILENT_OPERATOR_AGENT"),
		path: filepath.Join(filepath.Dir(cfg.inboundRecovery.path), alarmRecordsFile), records: make(map[string]alarmRecord),
		sending: make(map[string]chan struct{})}
	if a.bin == "" {
		a.bin = "gc"
	}
	if a.agent == "" {
		a.agent = "mayor"
	}
	a.load()
	log.Printf("dead-letter alarm: operator=%s", a.operatorSetting())
	return a
}

func (a *deadLetterAlarm) operatorSetting() string {
	if a.operator == "" {
		return "not_configured"
	}
	return "configured"
}

// After damaged state, continue using the most recent replacement on every
// restart. Reading the same damaged original again would repeat remote counts.
func (a *deadLetterAlarm) load() {
	base := a.path
	dir := filepath.Dir(base)
	files, err := os.ReadDir(dir)
	var generation int64
	if err == nil {
		for _, f := range files {
			suffix, ok := strings.CutPrefix(f.Name(), alarmRecordsFile+".")
			n, e := strconv.ParseInt(suffix, 10, 64)
			if ok && e == nil && n > generation {
				generation = n
				a.path = filepath.Join(dir, f.Name())
			}
		}
		err = a.readRecords()
		if errors.Is(err, os.ErrNotExist) && generation == 0 {
			return
		}
	}
	if err != nil {
		a.path = base + "." + strconv.FormatInt(max(a.now().UnixNano(), generation+1), 10)
		log.Print("dead-letter alarm: records unreadable; using a new file")
	}
}

func (a *deadLetterAlarm) readRecords() error {
	f, err := openBeneathFlags(filepath.Dir(a.path), filepath.Base(a.path), unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("invalid records file")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	var records map[string]alarmRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	if records == nil {
		return errors.New("missing alarm records")
	}
	for _, r := range records {
		if r.Raised.IsZero() {
			return errors.New("missing alarm time")
		}
		switch r.Operator {
		case "not_configured", "owed", "sent", "not_known":
		default:
			return errors.New("invalid operator outcome")
		}
		if r.OperatorTries < 0 || r.LocalTries < 0 {
			return errors.New("invalid alarm try count")
		}
	}
	a.records = records
	return nil
}

func (a *deadLetterAlarm) save() bool {
	b, err := json.Marshal(a.records)
	if err == nil {
		err = companyWriteFileAtomic(a.path, b)
	}
	a.dirty = err != nil
	if err != nil {
		log.Print("dead-letter alarm: records write failed")
		return false
	}
	return true
}

func (a *deadLetterAlarm) newRecord(now time.Time) alarmRecord {
	op := "owed"
	if a.operator == "" {
		op = "not_configured"
	}
	return alarmRecord{Raised: now, Operator: op}
}

// Called with mu held. Release the reservation only once the result is visible.
func (a *deadLetterAlarm) reserveLocal(key string) func() {
	done := make(chan struct{})
	a.sending[key] = done
	return func() {
		delete(a.sending, key)
		close(done)
	}
}

func (a *deadLetterAlarm) mailWritten(key string, send func() (operatorMailReceipt, error)) error {
	if a == nil {
		_, err := send()
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.sending[key] != nil {
		done := a.sending[key]
		a.mu.Unlock()
		<-done
		a.mu.Lock()
	}
	if a.records[key].LocalMailed {
		return nil
	}
	release := a.reserveLocal(key)
	defer release()
	a.mu.Unlock()
	receipt, err := send()
	a.mu.Lock()
	if err == nil && receipt.accepted() {
		r, ok := a.records[key]
		if !ok {
			r = a.newRecord(a.now())
		}
		r.LocalMailed, r.LocalAsked = true, receipt.Notified
		a.records[key] = r
		a.save()
	}
	return err
}

func (a *deadLetterAlarm) pass(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	set := a.cfg.inboundRecovery.deadLetterAlarmSet(now)
	changed := false
	for key, r := range a.records {
		if now.Sub(r.Raised) >= alarmRetention {
			delete(a.records, key)
			changed = true
			continue
		}
		// A write can mail before recovery imports the row. Keep that receipt
		// until the scanner sees it or retention expires. Remote escalation
		// becomes eligible only when the message is actually outstanding.
		_, present := set[key]
		if present && r.LocalMailed {
			r.LocalEnded = true
		}
		if !present && (!r.LocalMailed || r.LocalEnded) {
			r.Closed = true
		}
		if !r.Closed {
			if a.operator == "" && r.Operator == "owed" {
				r.Operator = "not_configured"
			}
			if a.operator != "" && r.Operator == "not_configured" {
				r.Operator = "owed"
			}
		}
		if r != a.records[key] {
			a.records[key] = r
			changed = true
		}
	}
	for key := range set {
		if _, ok := a.records[key]; !ok {
			a.records[key] = a.newRecord(now)
			changed = true
		}
	}
	if (changed || a.dirty) && !a.save() {
		return
	}
	for _, key := range slices.Sorted(maps.Keys(set)) {
		r := a.records[key]
		if ctx.Err() != nil {
			return
		}
		if r.Closed || r.LocalMailed || a.sending[key] != nil || r.LocalNext.After(a.now()) {
			continue
		}
		if !a.localMail(ctx, key, r) {
			return
		}
	}
	if ctx.Err() == nil && a.operator != "" {
		a.operatorMail(ctx)
	}
}

func (a *deadLetterAlarm) localMail(ctx context.Context, key string, r alarmRecord) bool {
	r.LocalTries++
	r.LocalNext = a.now().Add(retryLadderDelay(inboundHoldBackoff, r.LocalTries-1, 0))
	before := a.records[key]
	a.records[key] = r
	if !a.save() {
		a.records[key] = before
		return false
	}
	m, present := a.cfg.inboundRecovery.deadLetterAlarmSet(a.now())[key]
	if !present {
		before.Closed = true
		a.records[key] = before
		return a.save()
	}
	body := fmt.Sprintf("channel: %s\nthread_ts: %s\nmessage_ts: %s\nheld_since: %s\ndead_lettered_at: %s\nstate: %s\ncomposer: %s\noperator: %s\nlook: gc slack status\nredrive: gc slack inbound-redrive --conversation-id %s\nonly after you have read this message in Slack and answered it yourself (this delivers nothing and closes the alarm): gc slack inbound-redrive --conversation-id %s --acknowledge %s",
		m.channel, m.thread, m.ts, m.held.UTC().Format(time.RFC3339), m.row.UTC().Format(time.RFC3339), m.state, m.composer, a.operatorSetting(), m.channel, m.channel, m.ts)
	release := a.reserveLocal(key)
	defer release()
	out, err, timedOut := a.command(ctx, "--city", a.cfg.cityPath, "mail", "send", "mayor", "--dedup", "slack-dead:"+ingestionDigest(key), "--notify", "--json", "-s", "slack: a message did not reach the session", "-m", body)
	r.LocalEnded = true
	if err == nil {
		for line := range bytes.Lines(out) {
			var result struct{ OK, Notified bool }
			if json.Unmarshal(line, &result) == nil && result.OK {
				r.LocalMailed = true
				r.LocalAsked = result.Notified
				break
			}
		}
	}
	if !r.LocalMailed {
		cause := "mail_no_result"
		if timedOut {
			cause = "mail_timeout"
		} else if ingestionNotifyDefinitelyRefused(err) {
			cause = "mail_refused"
		}
		log.Printf("dead-letter alarm: channel=%s message_ts=%s try=%d cause=%s", m.channel, m.ts, r.LocalTries, cause)
		r.LocalNext = a.now().Add(retryLadderDelay(inboundHoldBackoff, r.LocalTries-1, 0))
	}
	a.records[key] = r
	return a.save()
}

func (a *deadLetterAlarm) command(ctx context.Context, args ...string) ([]byte, error, bool) {
	// The pass owns mu. Let unrelated write-time sends run while gc replies.
	a.mu.Unlock()
	defer a.mu.Lock()
	limited, cancel := context.WithTimeout(ctx, a.limit)
	defer cancel()
	cmd := exec.CommandContext(limited, a.bin, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return out, err, errors.Is(limited.Err(), context.DeadlineExceeded) && (cmd.ProcessState == nil || !cmd.ProcessState.Exited())
}

// Only explicitly pre-request errors permit another remote send. A generic
// error (even a nonzero exit) can follow acceptance at the peer.
func operatorDefinitelyRefused(out []byte, err error, contextName string) bool {
	var start *exec.Error
	var path *os.PathError
	if errors.As(err, &start) || errors.As(err, &path) {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() < 0 {
		return false
	}
	hasJSON := false
	for line := range bytes.Lines(out) {
		if !json.Valid(line) {
			continue
		}
		hasJSON = true
		var result struct {
			OK    bool
			Error struct{ Code string }
		}
		if json.Unmarshal(line, &result) == nil && !result.OK {
			switch result.Error.Code {
			case "invalid_arguments", "unsupported_remote":
				return true
			}
		}
	}
	if hasJSON {
		return false
	}
	msg := strings.TrimSpace(string(exit.Stderr))
	msg = strings.TrimPrefix(msg, "gc: ")
	msg = strings.TrimPrefix(msg, "gc mail send: ")
	return !strings.ContainsAny(msg, "\r\n") &&
		strings.HasPrefix(msg, fmt.Sprintf("context %q is not defined in ", contextName)) &&
		strings.HasSuffix(msg, " (run 'gc context list')")
}

func (a *deadLetterAlarm) operatorMail(ctx context.Context) {
	now := a.now()
	var keys []string
	due := false
	for key, r := range a.records {
		if !r.Closed && r.LocalEnded && r.Operator == "owed" {
			keys = append(keys, key)
			due = due || !r.OperatorNext.After(now)
		}
	}
	if !due {
		return
	}
	slices.Sort(keys)
	before := make(map[string]alarmRecord, len(keys))
	for _, key := range keys {
		r := a.records[key]
		before[key] = r
		// Reserve the entire batch durably. If interrupted from here on, a
		// remote send has an unknown outcome and must never be repeated.
		r.Operator = "not_known"
		r.OperatorTries++
		a.records[key] = r
	}
	if !a.save() {
		maps.Copy(a.records, before)
		return
	}
	now = a.now()
	set := a.cfg.inboundRecovery.deadLetterAlarmSet(now)
	stillDue := false
	active := keys[:0]
	for _, key := range keys {
		if _, ok := set[key]; !ok {
			r := before[key]
			r.Closed = true
			a.records[key] = r
			continue
		}
		active = append(active, key)
		stillDue = stillDue || !before[key].OperatorNext.After(now)
	}
	if !stillDue {
		for _, key := range active {
			a.records[key] = before[key]
		}
		a.save()
		return
	}
	keys = active
	body := a.operatorBody(keys, set, now)
	subject := fmt.Sprintf("slack: %s: %d message(s) did not reach the session", a.cfg.cityName, len(keys))
	out, err, _ := a.command(ctx, "mail", "send", "--context", a.operator, a.agent, "--dedup", "slack-dead-operator:"+ingestionDigest(strings.Join(keys, "\n")), "--json", "-s", subject, "-m", body)
	state := "sent"
	cause := ""
	if err != nil {
		if operatorDefinitelyRefused(out, err, a.operator) {
			state = "owed"
			cause = "operator_refused"
		} else {
			state = "not_known"
			cause = "operator_not_known"
		}
	}
	try := 0
	for _, key := range keys {
		r := a.records[key]
		r.Operator = state
		if state == "owed" {
			r.OperatorNext = a.now().Add(retryLadderDelay(inboundHoldBackoff, r.OperatorTries-1, 0))
		}
		try = max(try, r.OperatorTries)
		a.records[key] = r
	}
	if cause != "" {
		log.Printf("dead-letter alarm: messages=%d try=%d cause=%s", len(keys), try, cause)
	}
	a.save()
}

func (a *deadLetterAlarm) operatorBody(keys []string, set map[string]alarmMessage, now time.Time) string {
	oldest := keys[0]
	channels := make(map[string]bool)
	counts := make(map[string]int)
	for _, key := range keys {
		m := set[key]
		channels[m.channel] = true
		counts[m.state]++
		if m.held.Before(set[oldest].held) {
			oldest = key
		}
	}
	m := set[oldest]
	return fmt.Sprintf("city: %s\nchannel: slack\nmessages: %d\nconversations: %d\noldest_age_seconds: %d\nretrying: %d\nwaiting: %d\nhand_check: %d\ncomposer: %s\nlocal_wake: %s\nstatus (run in the city named above): gc slack status --json",
		a.cfg.cityName, len(keys), len(channels), max(0, int64(now.Sub(m.held)/time.Second)), counts["retrying"], counts["waiting"], counts["hand_check"], m.composer, a.records[oldest].wake())
}

func runDeadLetterAlarm(ctx context.Context, cfg config) {
	if cfg.deadLetterAlerts == nil || cfg.deadLetterAlerts.alarm == nil {
		return
	}
	a := cfg.deadLetterAlerts.alarm
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		a.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
