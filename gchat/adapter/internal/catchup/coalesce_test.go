package catchup

import (
	"context"
	"fmt"
	"github.com/BurntSushi/toml"
	"strings"
	"testing"
	"time"
)

func TestCatchUpRetryCountsEveryAct(t *testing.T) {
	f := setup(t)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &f.opts.Config.Settings); err != nil {
		t.Fatal(err)
	}
	if f.r.budget != 50 {
		t.Fatalf("default budget = %d, want 50", f.r.budget)
	}
	f.addSpace("spaces/room", true)
	for i := 0; i < 70; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Hour+time.Duration(i+1)*time.Second)).Text = fmt.Sprintf("unique-body-%03d!", i)
	}
	f.freeze(t)
	f.gc.CityNotRunning = true
	if delivered, _, err := f.r.Pass(context.Background()); err == nil || delivered != 0 {
		t.Fatalf("refused pass = %d, %v", delivered, err)
	}
	f.gc.CityNotRunning = false
	previous := 0
	for pass := 0; pass < 3; pass++ {
		delivered, remaining, err := f.r.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		text := ""
		for _, w := range f.gc.Inbound() {
			text += w.Text
		}
		total := strings.Count(text, "unique-body-")
		if actual := total - previous; actual > f.r.budget || delivered != actual {
			t.Errorf("pass %d: delivered=%d actual=%d budget=%d", pass, delivered, actual, f.r.budget)
		}
		previous = total
		if remaining == 0 {
			break
		}
	}
	text := ""
	for _, w := range f.gc.Inbound() {
		text += w.Text
	}
	last := -1
	for i := 0; i < 70; i++ {
		body := fmt.Sprintf("unique-body-%03d!", i)
		at := strings.Index(text, body)
		if strings.Count(text, body) != 1 || at <= last {
			t.Fatalf("missing, duplicate or reordered act %d", i)
		}
		last = at
	}
	s := f.opts.Ledger.Spaces["spaces/room"]
	if !s.MessageCursor.Equal(frozen) || !s.EventCursor.Equal(frozen) || len(s.Backlog) != 0 {
		t.Fatal("cursor did not settle")
	}
}

func TestCatchUpIsMerged(t *testing.T) {
	f := setup(t)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &f.opts.Config.Settings); err != nil {
		t.Fatal(err)
	}
	f.addSpace("spaces/room", true)
	for i := 0; i < 120; i++ {
		m := f.addMessage("spaces/room", i, frozen.Add(-time.Hour+time.Duration(i+1)*time.Second))
		m.Text = fmt.Sprintf("unique-body-%03d!", i)
	}
	f.freeze(t)
	f.pass(t, 50, 70)
	f.pass(t, 50, 20)
	f.pass(t, 20, 0)
	w := f.gc.Inbound()
	if len(w) != 3 {
		t.Fatalf("turns=%d want=3", len(w))
	}
	all := ""
	for _, m := range w {
		if !strings.HasPrefix(m.Text, "[catch-up") {
			t.Fatal(m.Text)
		}
		all += m.Text
	}
	for i := 0; i < 120; i++ {
		if strings.Count(all, fmt.Sprintf("unique-body-%03d!", i)) != 1 {
			t.Fatalf("lost or duplicate %d", i)
		}
	}
	s := f.opts.Ledger.Spaces["spaces/room"]
	if !s.MessageCursor.Equal(frozen) || !s.EventCursor.Equal(frozen) || len(s.Backlog) != 0 {
		t.Fatal("cursor changed")
	}
}

func TestCatchUpWindowKeepsFloor(t *testing.T) {
	for _, window := range []string{"0s", "8s"} {
		t.Run(window, func(t *testing.T) {
			f := setup(t)
			_, _ = toml.Decode("coalesce_window = "+fmt.Sprintf("%q", window), &f.opts.Config.Settings)
			f.addSpace("spaces/room", true)
			f.opts.Ledger.HistoryFloor = frozen.Add(-30 * time.Minute)
			old := f.addMessage("spaces/room", 0, frozen.Add(-time.Hour))
			old.Text = "old-body-never-delivered"
			for i := 1; i <= 2; i++ {
				f.addMessage("spaces/room", i, frozen.Add(-time.Duration(i)*time.Minute))
			}
			f.freeze(t)
			f.pass(t, 2, 0)
			want := 1
			if window == "0s" {
				want = 2
			}
			turns := f.gc.Inbound()
			if len(turns) != want {
				t.Fatalf("turns=%d", len(turns))
			}
			for _, w := range turns {
				if strings.Contains(w.Text, old.Text) {
					t.Fatal("old catch-up line")
				}
			}
		})
	}
}
