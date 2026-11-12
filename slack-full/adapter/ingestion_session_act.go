package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Only the explicit /react and successful session /publish paths write these
// facts. Admission/busy reactions never do. No message text is retained.
type ingestionSessionAct struct {
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Channel   string    `json:"channel"`
	MessageTS string    `json:"message_ts"`
	ThreadTS  string    `json:"thread_ts,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Emoji     string    `json:"emoji,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

const ingestionActCacheLimit = 4096

func (j *inboundJournal) recordSessionAct(act ingestionSessionAct) {
	if j == nil {
		return
	}
	if act.Timestamp.IsZero() {
		act.Timestamp = time.Now().UTC()
		if j.now != nil {
			act.Timestamp = j.now().UTC()
		}
	}
	if act.Kind == "reaction" {
		j.watch.Record(stopwatch.Event{Event: "reaction", Conversation: act.Channel, Message: act.MessageTS, At: act.Timestamp, Actor: "agent"})
	}
	if act.Kind == "reply" {
		j.watch.Record(stopwatch.Event{Event: "words", Conversation: act.Channel, Message: act.MessageTS, Thread: act.ThreadTS, At: act.Timestamp})
	}
	if ingestionObserverDisabled() {
		return
	}
	if err := j.append(inboundJournalRecord{Phase: act.Kind, Channel: act.Channel, MessageTS: act.MessageTS, ThreadTS: act.ThreadTS, SessionAct: &act}); err != nil {
		log.Printf("ingestion observer: persist %s act: %v", act.Kind, err)
	}
}

func validIngestionAct(act ingestionSessionAct) bool {
	if act.Channel == "" || act.MessageTS == "" || act.SessionID == "" || act.Timestamp.IsZero() {
		return false
	}
	return (act.Kind == "reaction" && act.Source == "session_react" && act.Emoji != "") ||
		(act.Kind == "reply" && act.Source == "session_publish" && act.ThreadTS != "" && act.SessionID != "")
}

// Caller holds j.mu. This cache is only an accelerator: overflow falls back to
// the bounded durable journal so eviction cannot turn a seen line into an alarm.
func (j *inboundJournal) cacheSessionActLocked(act ingestionSessionAct) {
	if !validIngestionAct(act) {
		return
	}
	if j.sessionActs == nil {
		j.sessionActs = make(map[string]ingestionSessionAct)
	}
	key := strings.Join([]string{act.Kind, act.Channel, act.MessageTS, act.ThreadTS, act.SessionID, act.Emoji, act.Timestamp.UTC().Format(time.RFC3339Nano)}, "\x00")
	if _, exists := j.sessionActs[key]; exists {
		return
	}
	if len(j.sessionActs) >= ingestionActCacheLimit {
		j.sessionActsOverflow = true
		// The journal remains authoritative. Avoid O(capacity) eviction work
		// on each replayed row; later acts are read from disk on a cache miss.
		return
	}
	j.sessionActs[key] = act
}

func sessionActEvidence(act ingestionSessionAct, r inboundJournalRecord) *ingestionEvidence {
	start := r.StartedAt
	if start.IsZero() {
		start = r.DeliveredAt
	}
	if !validIngestionAct(act) || act.Channel != r.Channel ||
		act.Timestamp.Before(start.Add(-30*time.Second)) ||
		act.Timestamp.After(r.DeliveredAt.Add(ingestionLateWindow)) {
		return nil
	}
	session := r.CanonicalSessionID
	if session == "" && len(r.TargetSessions) > 0 {
		session = r.TargetSessions[0]
	}
	if act.Kind == "reaction" {
		if act.MessageTS != r.MessageTS || act.SessionID != session {
			return nil
		}
	} else {
		thread := r.ThreadTS
		if thread == "" {
			thread = r.MessageTS
		}
		// Slack also accepts a reply's own message ID as thread_ts and
		// normalizes it to the root, as the busy-reaction registry does.
		matchesThread := act.ThreadTS == thread || act.ThreadTS == r.MessageTS
		if !matchesThread || act.SessionID != session || compareSlackTS(act.MessageTS, r.MessageTS) <= 0 {
			return nil
		}
	}
	return &ingestionEvidence{SessionID: session, Timestamp: act.Timestamp, Kind: act.Kind,
		MessageTS: r.MessageTS, ThreadTS: r.ThreadTS,
		EntryID: strings.Join([]string{act.Source, act.MessageTS, act.Emoji}, ":")}
}

func ingestionMessageWindows(r inboundJournalRecord) []injectionWindow {
	if len(r.Windows) > 0 {
		return r.Windows
	}
	return []injectionWindow{{MessageTS: r.MessageTS, ThreadTS: r.ThreadTS}}
}

// Coalesced messages share the receipt's saved send interval, but each needs
// its own qualifying act. A partial cache hit must still scan the durable
// journal on overflow; the returned missing members are only a mail snapshot.
func (j *inboundJournal) sessionActProof(r inboundJournalRecord) (*ingestionEvidence, []injectionWindow, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	members := ingestionMessageWindows(r)
	proofs := make([]*ingestionEvidence, len(members))
	consider := func(act ingestionSessionAct) {
		for i, member := range members {
			message := r
			message.MessageTS, message.ThreadTS = member.MessageTS, member.ThreadTS
			if p := sessionActEvidence(act, message); p != nil && (proofs[i] == nil || p.Timestamp.Before(proofs[i].Timestamp)) {
				proofs[i] = p
			}
		}
	}
	result := func() (*ingestionEvidence, []injectionWindow, error) {
		var missing []injectionWindow
		var latest *ingestionEvidence
		for i, proof := range proofs {
			if proof == nil {
				missing = append(missing, members[i])
			} else if latest == nil || proof.Timestamp.After(latest.Timestamp) {
				latest = proof
			}
		}
		if len(missing) > 0 {
			return nil, missing, nil
		}
		// The completing act supplies the legacy kind/time. Keep every
		// member's proof so mixed reaction/reply coverage is auditable.
		complete := *latest
		if len(proofs) > 1 {
			for _, proof := range proofs {
				complete.Members = append(complete.Members, *proof)
			}
		}
		return &complete, nil, nil
	}
	for _, act := range j.sessionActs {
		consider(act)
	}
	if proof, missing, err := result(); proof != nil || !j.sessionActsOverflow {
		return proof, missing, err
	}
	paths, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	remaining := int64(64 << 20)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, err
		}
		remaining -= info.Size()
		if remaining < 0 {
			return nil, nil, fmt.Errorf("journal exceeds 64 MiB session-act observation budget")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 4096), 1<<20)
		for s.Scan() {
			var row inboundJournalRecord
			if json.Unmarshal(s.Bytes(), &row) == nil && row.SessionAct != nil {
				consider(*row.SessionAct)
			}
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return result()
}

// Use Slack's original publication time, not a later idempotent replay or
// readback time, which could otherwise falsely cover a newer inbound.
func ingestionPublishTime(ts string) (time.Time, bool) {
	seconds, fraction, found := strings.Cut(ts, ".")
	if !found || len(fraction) == 0 || len(fraction) > 9 {
		return time.Time{}, false
	}
	for _, c := range seconds + fraction {
		if c < '0' || c > '9' {
			return time.Time{}, false
		}
	}
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	nsec, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec).UTC(), true
}
