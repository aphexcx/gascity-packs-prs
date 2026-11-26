package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// UNPROVEN Q3/Q5: this mailbox models local unarchived-key dedup, a requested
// wake and a remote mailbox without dedup. It never contacts either city.
const alarmFake = `#!/bin/sh
set -eu
cd "$ALARM_FIXTURE"
if [ "$1" = "--city" ]; then road=local; key=$7; else road=operator; key=$7; fi
printf '%s\0' "$@" >> "$road.args"
printf '\0' >> "$road.args"
mode=$(cat "$road.mode")
case "$mode" in
  refused) printf '%s\n' '{"ok":false,"error":{"code":"invalid_arguments"}}'; exit 1;;
  unsupported) printf '%s\n' '{"ok":false,"error":{"code":"unsupported_remote"}}'; exit 1;;
  context) echo "gc mail send: context \"fixture-context\" is not defined in contexts.toml (run 'gc context list')" >&2; exit 1;;
  ambiguous) printf '%s\n' '{"ok":false,"error":{"code":"mail_send_failed"}}'; exit 1;;
  noise) echo noise; exit 0;;
  blank) exit 0;;
  false) echo '{"ok":false}'; exit 0;;
esac
if [ "$road" = local ] && [ -e "$key" ]; then
  echo '{"ok":true,"already_done":true}'; exit 0
fi
echo "$key" >> "$road.mails"
if [ "$road" = local ]; then touch "$key"; fi
if [ -n "${ALARM_RECORDS:-}" ]; then cp "$ALARM_RECORDS" "$road.before"; fi
case "$mode" in
  hang) touch "$road.open"; exec sleep 30;;
  wait) touch "$road.open"; while [ ! -e release ]; do sleep 0.01; done;;
  lost) exit 0;;
  unwoken) echo '{"ok":true}'; exit 0;;
esac
echo '{"ok":true,"notified":true}'
`

const alarmMarker = "private-fixture-payload"

type alarmFixture struct {
	t   *testing.T
	dir string
	cfg config
	now *time.Time
	a   *deadLetterAlarm
}

func alarmWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0700); err != nil {
		t.Fatal(err)
	}
}

// UNPROVEN Q2/Q4/Q6: operator configuration, reading remote mail, and the
// production command deadline are premises. This fixture models settings and
// mailbox writes only, and timeout cases use a shorter limit.
func newAlarmFixture(t *testing.T, operator bool) *alarmFixture {
	t.Helper()
	r, now := round2Recovery(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "gc")
	alarmWrite(t, bin, alarmFake)
	t.Setenv("ALARM_FIXTURE", dir)
	t.Setenv("GC_BIN", bin)
	t.Setenv("NEVER_SILENT", "")
	t.Setenv("NEVER_SILENT_OPERATOR_CONTEXT", "")
	if operator {
		t.Setenv("NEVER_SILENT_OPERATOR_CONTEXT", "fixture-context")
	}
	t.Setenv("NEVER_SILENT_OPERATOR_AGENT", "fixture-agent")
	f := &alarmFixture{t: t, dir: dir, cfg: config{cityName: "fixture", cityPath: dir, inboundRecovery: r}, now: now}
	f.mode("local", "ok")
	f.mode("operator", "ok")
	f.restart()
	return f
}

func (f *alarmFixture) restart() {
	f.a = newDeadLetterAlarm(f.cfg)
	f.a.now = func() time.Time { return *f.now }
	f.t.Setenv("ALARM_RECORDS", f.a.path)
}
func (f *alarmFixture) mode(road, mode string) {
	alarmWrite(f.t, filepath.Join(f.dir, road+".mode"), mode)
}
func (f *alarmFixture) pass() { f.a.pass(context.Background()) }
func (f *alarmFixture) add(channel, ts string) inboundRecoveryEntry {
	return inboundRecoveryEntry{Inbound: testPending(channel, ts, alarmMarker).inbound, HeldAt: f.now.Add(-time.Hour), DeadLetteredAt: *f.now}
}
func (f *alarmFixture) put(slot string, e inboundRecoveryEntry) {
	r := f.cfg.inboundRecovery
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Entries[slot] = e
}
func (f *alarmFixture) file(name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}
func (f *alarmFixture) args(road string) [][]string {
	raw := strings.TrimSuffix(f.file(road+".args"), "\x00\x00")
	if raw == "" {
		return nil
	}
	var calls [][]string
	for _, call := range strings.Split(raw, "\x00\x00") {
		calls = append(calls, strings.Split(call, "\x00"))
	}
	return calls
}
func (f *alarmFixture) count(road string, want int) {
	f.t.Helper()
	if got := len(f.args(road)); got != want {
		f.t.Fatalf("%s calls = %d, want %d", road, got, want)
	}
}
func (f *alarmFixture) one(road string) []string {
	f.t.Helper()
	f.count(road, 1)
	return f.args(road)[0]
}

func TestDeadLetterAlarmSet(t *testing.T) {
	for _, row := range []struct {
		name string
		want int
	}{
		{"dead", 1}, {"hand", 1}, {"held", 0}, {"pasted", 0}, {"acknowledged", 0}, {"reaction", 0}, {"bot", 0}, {"old", 0}, {"room-and-handle", 1}, {"replay-refused", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAlarmFixture(t, false)
			e := f.add("C1", "100.1")
			switch row.name {
			case "hand":
				e.DeadLetteredAt = time.Time{}
				e.ManualRecovery = true
			case "held":
				e.DeadLetteredAt = time.Time{}
			case "pasted":
				e.DeliveredAt = *f.now
			case "acknowledged":
				e.AcknowledgedAt = *f.now
			case "reaction":
				e.Reaction = true
			case "bot":
				e.Inbound.Actor.IsBot = true
			case "old":
				e.DeadLetteredAt = f.now.Add(-25 * time.Hour)
			case "room-and-handle":
				f.put("handle", e)
			}
			f.put("room", e)
			f.pass()
			if row.name == "replay-refused" {
				e.ReplayBlockedReason = "refused"
				f.put("room", e)
			}
			f.pass()
			f.pass()
			f.count("local", row.want)
		})
	}
}

func TestDeadLetterAlarmOnce(t *testing.T) {
	f := newAlarmFixture(t, true)
	f.put("room", f.add("C1", "100.1"))
	f.mode("local", "hang")
	f.a.limit = 250 * time.Millisecond
	for i := 0; i < 1000; i++ {
		if i == 1 {
			f.mode("local", "ok")
		}
		if i == 500 {
			f.restart()
		}
		f.pass()
		*f.now = f.now.Add(time.Second)
	}
	f.count("local", 2)
	f.count("operator", 1)
	if f.args("local")[0][6] != f.args("local")[1][6] || len(strings.Fields(f.file("local.mails"))) != 1 {
		t.Fatal("retry changed key or created another mail")
	}
}

func TestDeadLetterAlarmArchive(t *testing.T) {
	// UNPROVEN Q5: archiving between a lost result and a retry releases the fake dedup key.
	for _, row := range []struct {
		name          string
		archive, land bool
		calls, mails  int
	}{
		{"unarchived", false, false, 2, 1}, {"archive-between-lost-result-and-retry", true, false, 2, 2}, {"arrived-before-retry", true, true, 1, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAlarmFixture(t, false)
			e := f.add("C1", "100.1")
			f.put("room", e)
			f.mode("local", "lost")
			f.pass()
			first := f.one("local")
			if row.archive {
				if err := os.Remove(filepath.Join(f.dir, first[6])); err != nil {
					t.Fatal(err)
				}
			}
			if row.land {
				e.DeliveredAt = *f.now
				f.put("room", e)
			}
			*f.now = f.now.Add(20 * time.Second)
			f.mode("local", "ok")
			f.pass()
			f.count("local", row.calls)
			if got := len(strings.Fields(f.file("local.mails"))); got != row.mails {
				t.Errorf("mails=%d want=%d", got, row.mails)
			}
		})
	}
}

func TestDeadLetterAlarmArrived(t *testing.T) {
	for _, name := range []string{"before-pass", "before-operator", "replay-row"} {
		t.Run(name, func(t *testing.T) {
			f := newAlarmFixture(t, true)
			e := f.add("C1", "100.1")
			f.put("room", e)
			if name == "before-operator" {
				f.mode("local", "wait")
				f.a.limit = 2 * time.Second
				done := make(chan struct{})
				go func() { defer close(done); f.pass() }()
				defer func() { alarmWrite(t, filepath.Join(f.dir, "release"), ""); <-done }()
				f.waitOpen("local", done)
				f.count("local", 1)
				e.DeliveredAt = *f.now
				f.put("room", e)
				alarmWrite(t, filepath.Join(f.dir, "release"), "")
				<-done
			} else {
				e.DeliveredAt = *f.now
				e.Landed = true
				f.put("room", e)
				if name == "replay-row" {
					if err := f.cfg.inboundRecovery.writeDead(e, *f.now); err != nil {
						t.Fatal(err)
					}
				}
				f.pass()
				f.count("local", 0)
			}
			f.count("operator", 0)
		})
	}
}

func (f *alarmFixture) waitOpen(road string, done <-chan struct{}) {
	f.t.Helper()
	end := time.NewTimer(2 * time.Second)
	defer end.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(filepath.Join(f.dir, road+".open")); err == nil {
			return
		}
		select {
		case <-done:
			return
		case <-tick.C:
		case <-end.C:
			f.t.Fatal("stub did not start")
		}
	}
}

func TestDeadLetterAlarmRoads(t *testing.T) {
	for _, row := range []struct {
		name, local, operator     string
		localCalls, operatorCalls int
		wake                      string
	}{
		{"success", "ok", "ok", 1, 1, "asked"},
		{"local-refused", "refused", "ok", 2, 1, "not_known"},
		{"wake-absent", "unwoken", "ok", 1, 1, "not_asked"},
		{"empty", "blank", "ok", 2, 1, "not_known"}, {"non-json", "noise", "ok", 2, 1, "not_known"}, {"ok-false", "false", "ok", 2, 1, "not_known"},
		{"operator-refused", "ok", "refused", 1, 2, "asked"}, {"operator-unsupported", "ok", "unsupported", 1, 2, "asked"},
		{"operator-context", "ok", "context", 1, 2, "asked"}, {"remote-ambiguous", "ok", "ambiguous", 1, 1, "asked"}, {"operator-timeout", "ok", "hang", 1, 1, "asked"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAlarmFixture(t, true)
			f.mode("local", row.local)
			f.mode("operator", row.operator)
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			f.a.limit = 250 * time.Millisecond
			f.put("room", f.add("C1", "100.1"))
			f.pass()
			*f.now = f.now.Add(19 * time.Second)
			f.pass()
			f.count("local", 1)
			f.count("operator", 1)
			op := f.one("operator")
			if !strings.Contains(op[len(op)-1], "local_wake: "+row.wake+"\n") {
				t.Error("incorrect wake result")
			}
			*f.now = f.now.Add(time.Second)
			f.pass()
			f.count("local", row.localCalls)
			f.count("operator", row.operatorCalls)
			if row.operator == "hang" || row.operator == "ambiguous" {
				if strings.Count(logs.String(), "cause=operator_not_known") != 1 {
					t.Error("expected one unknown-outcome diagnostic for the operator batch")
				}
			}
		})
	}
	t.Run("not-configured", func(t *testing.T) {
		var out bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&out)
		defer log.SetOutput(prev)
		f := newAlarmFixture(t, false)
		f.put("room", f.add("C1", "100.1"))
		f.pass()
		f.count("operator", 0)
		cmd := f.one("local")
		if !strings.Contains(cmd[len(cmd)-1], "operator: not_configured") || !strings.Contains(out.String(), "not_configured") {
			t.Error("missing operator configuration status")
		}
	})
	t.Run("both-refused-for-a-day", func(t *testing.T) {
		f := newAlarmFixture(t, true)
		f.put("room", f.add("C1", "100.1"))
		f.mode("local", "refused")
		f.mode("operator", "refused")
		start := *f.now
		next := start
		tries := 0
		ladder := []time.Duration{20 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}
		for next.Before(start.Add(24 * time.Hour)) {
			*f.now = next
			f.pass()
			tries++
			next = next.Add(ladder[min(tries-1, len(ladder)-1)])
			*f.now = next.Add(-time.Second)
			f.pass()
		}
		*f.now = start.Add(24 * time.Hour)
		f.pass()
		*f.now = f.now.Add(time.Hour)
		f.pass()
		if tries >= 110 {
			t.Fatalf("unexpected daily cost: %d", tries)
		}
		f.count("local", tries)
		f.count("operator", tries)
	})
}

func TestDeadLetterAlarmWords(t *testing.T) {
	for _, row := range []struct{ name, state, composer string }{
		{"clock", "retrying", "own_paste"}, {"plain", "waiting", "none"}, {"manual", "hand_check", "unreadable_composer"}, {"aged", "hand_check", "unowned_draft"}, {"off", "hand_check", "none"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			f := newAlarmFixture(t, true)
			e := f.add("C1", "100.1")
			e.Inbound.ReplyToMessageID = "99.1"
			switch row.name {
			case "clock":
				e.LastHeldAnswerAt = *f.now
			case "manual":
				e.ManualRecovery = true
				e.DeadLetteredAt = time.Time{}
			case "aged":
				e.DeadLetteredAt = f.now.Add(-2 * time.Minute)
				f.cfg.inboundRecovery.replayMaxAge = time.Minute
			case "off":
				f.cfg.inboundRecovery.replayMaxAge = 0
			}
			if row.composer != "none" {
				e.ComposerHold = &deliveryComposerHold{State: row.composer}
			}
			f.put("room", e)
			f.pass()
			dead := e.DeadLetteredAt
			if dead.IsZero() {
				dead = e.HeldAt
			}
			lbody := strings.Join([]string{"channel: C1", "thread_ts: 99.1", "message_ts: 100.1", "held_since: " + e.HeldAt.UTC().Format(time.RFC3339), "dead_lettered_at: " + dead.UTC().Format(time.RFC3339), "state: " + row.state, "composer: " + row.composer, "operator: configured", "look: gc slack status", "redrive: gc slack inbound-redrive --conversation-id C1", "by hand: gc slack inbound-redrive --conversation-id C1 --acknowledge 100.1"}, "\n")
			local := []string{"--city", f.cfg.cityPath, "mail", "send", "mayor", "--dedup", "slack-dead:" + ingestionDigest("C1|100.1"), "--notify", "--json", "-s", "slack: a message did not reach the session", "-m", lbody}
			if got := f.one("local"); !slices.Equal(got, local) {
				t.Error("local arguments or body differ")
			}
			count := map[string]int{row.state: 1}
			obody := fmt.Sprintf("city: fixture\nchannel: slack\nmessages: 1\nconversations: 1\noldest_age_seconds: %d\nretrying: %d\nwaiting: %d\nhand_check: %d\ncomposer: %s\nlocal_wake: asked\nstatus: gc slack status --json", int(f.now.Sub(e.HeldAt).Seconds()), count["retrying"], count["waiting"], count["hand_check"], row.composer)
			op := []string{"mail", "send", "--context", "fixture-context", "fixture-agent", "--dedup", "slack-dead-operator:" + ingestionDigest("C1|100.1"), "--json", "-s", "slack: fixture: 1 message(s) did not reach the session", "-m", obody}
			if got := f.one("operator"); !slices.Equal(got, op) {
				t.Error("operator arguments or body differ")
			}
			for _, s := range []string{logs.String(), f.file("local.args"), f.file("operator.args")} {
				if strings.Contains(s, alarmMarker) {
					t.Error("payload disclosed")
				}
			}
			if s := f.file("operator.args"); strings.Contains(s, "100.1") || strings.Contains(s, "C1") {
				t.Error("operator received message coordinates")
			}
		})
	}
	t.Run("batch-and-earliest-retry", func(t *testing.T) {
		f := newAlarmFixture(t, true)
		f.mode("operator", "refused")
		f.put("first", f.add("C1", "100.1"))
		f.pass()
		*f.now = f.now.Add(5 * time.Second)
		f.put("second", f.add("C2", "100.2"))
		f.pass()
		f.count("operator", 2)
		calls := f.args("operator")
		if !strings.Contains(calls[1][len(calls[1])-1], "messages: 2\nconversations: 2") {
			t.Fatal("newly due record did not gather both")
		}
		*f.now = f.now.Add(19 * time.Second)
		f.pass()
		f.count("operator", 2)
		*f.now = f.now.Add(time.Second)
		f.pass()
		f.count("operator", 3)
	})
}

func TestDeadLetterAlarmApart(t *testing.T) {
	f := newAlarmFixture(t, false)
	f.mode("local", "hang")
	f.a.limit = 1500 * time.Millisecond
	c := clockCase(t, true)
	f.cfg.inboundRecovery = c.r
	f.restart()
	f.a.limit = 1500 * time.Millisecond
	key := c.hold("100.1", "s1")
	c.through(1800)
	polls := 0
	c.answer = func(e inboundRecoveryEntry) (deliveryReceipt, error) { return clockReceipt("pending", "s1"), nil }
	c.r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
		polls++
		return receiptPollPending, deliveryReceipt{}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); f.pass() }()
	defer func() { <-done }()
	f.waitOpen("local", done)
	// The empty implementation has no open command. A real pass must leave
	// sufficient time for both the scheduled offer and the receipt poll.
	c.through(1920)
	if len(c.posts[key]) == 0 || polls == 0 {
		t.Error("hold queue missed an offer or a poll")
	}
	if f.file("local.args") != "" {
		select {
		case <-done:
			t.Error("hold queue blocked until command ended")
		default:
		}
	}
}

func TestDeadLetterAlarmBack(t *testing.T) {
	// UNPROVEN Q7: compare the normal held clock and vouch paths against the
	// same fake receipts with the alarm enabled.
	for _, state := range []string{"retrying", "waiting", "hand_check"} {
		t.Run(state, func(t *testing.T) {
			var baseline []int
			for _, on := range []bool{false, true} {
				f := newAlarmFixture(t, false)
				c := clockCase(t, true)
				key := c.hold("100.1", "s1")
				c.through(1800)
				e := c.r.state.Entries[key]
				if state != "retrying" {
					e.LastHeldAnswerAt = time.Time{}
					e.NextAttemptAt = time.Time{}
				}
				if state == "hand_check" {
					e.ManualRecovery = true
				}
				c.r.state.Entries[key] = e
				before := len(c.posts[key])
				c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) { return clockReceipt("delivered", "s1"), nil }
				if on {
					f.cfg.inboundRecovery = c.r
					f.restart()
					f.pass()
				}
				if state == "waiting" {
					c.r.observe(clockReceipt("delivered", "s1"))
				}
				c.through(1920)
				if on {
					f.pass()
				}
				want := 1
				if state == "hand_check" {
					want = 0
				}
				if c.pastes[key] != want || len(c.posts[key])-before != want {
					t.Errorf("expected %d recovery offers and pastes", want)
				}
				if !on {
					baseline = slices.Clone(c.posts[key])
				} else if !reflect.DeepEqual(c.posts[key], baseline) {
					t.Error("alarm changed offer schedule")
				}
			}
		})
	}
}

func TestDeadLetterAlarmRestart(t *testing.T) {
	for _, road := range []string{"local", "operator"} {
		t.Run("in-flight-"+road, func(t *testing.T) {
			f := newAlarmFixture(t, true)
			f.put("room", f.add("C1", "100.1"))
			f.pass()
			snapshot := f.file(road + ".before")
			if snapshot == "" {
				t.Fatal("missing pre-command record")
			}
			alarmWrite(t, f.a.path, snapshot)
			f.restart()
			*f.now = f.now.Add(16 * time.Minute)
			f.pass()
			if road == "local" {
				f.count("local", 2)
			} else {
				f.count("operator", 1)
				f.count("local", 1)
			}
		})
	}
	t.Run("damaged-records", func(t *testing.T) {
		f := newAlarmFixture(t, true)
		f.put("room", f.add("C1", "100.1"))
		f.pass()
		f.count("operator", 1)
		path := f.a.path
		alarmWrite(t, path, "broken")
		var logs bytes.Buffer
		old := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(old)
		f.restart()
		f.pass()
		f.count("operator", 2)
		if logs.Len() == 0 || !strings.HasPrefix(f.a.path, path+".") {
			t.Fatal("no damaged-state diagnostic or alternate file")
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "broken" {
			t.Fatal("damaged file overwritten")
		}
		f.restart()
		f.pass()
		f.count("operator", 2)
	})
}

func TestDeadLetterAlarmWire(t *testing.T) {
	f := newAlarmFixture(t, false)
	*f.now = time.Now().UTC()
	f.put("room", f.add("C1", "100.1"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runDeadLetterAlarm(ctx, f.cfg) }()
	defer func() { cancel(); <-done }()
	end := time.Now().Add(3 * time.Second)
	for len(f.args("local")) == 0 && time.Now().Before(end) {
		select {
		case <-done:
			t.Fatal("worker returned before sending")
		case <-time.After(time.Millisecond):
		}
	}
	f.count("local", 1)
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(source), "runDeadLetterAlarm(") != 1 || !strings.Contains(string(source), "go cfg.inboundRecovery.run(janitorCtx, cfg)\n\tgo runDeadLetterAlarm(janitorCtx, cfg)") {
		t.Error("main does not start exactly one alarm beside recovery")
	}
}
