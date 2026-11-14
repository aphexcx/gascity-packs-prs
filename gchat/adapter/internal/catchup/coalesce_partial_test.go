package catchup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
)

func TestCatchUpReportsPartialSuccess(t *testing.T) {
	f := setup(t)
	window := "8s"
	f.opts.Config.Settings.CoalesceWindow = &window
	target, err := url.Parse(f.gc.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var attempts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/inbound") && attempts.Add(1) == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer s.Close()
	f.p, err = inbound.New(inbound.Options{Config: f.opts.Config, Ledger: f.opts.Ledger, LedgerMu: f.opts.LedgerMu, LedgerPath: f.opts.LedgerPath, Chat: f.opts.Chat, GC: gcclient.New(s.URL, "examplecity"), IdentityUserID: "users/123", Now: func() time.Time { return frozen }, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Processor = f.p
	f.newRunner(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 3; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Hour+time.Duration(i+1)*time.Second)).Text = strings.Repeat(string(rune('a'+i)), 20000)
	}
	f.freeze(t)
	delivered, _, err := f.r.Pass(context.Background())
	if err == nil || delivered != 1 || len(f.gc.Inbound()) != 1 {
		t.Errorf("partly refused pass: delivered=%d actual=%d error=%v", delivered, len(f.gc.Inbound()), err)
	}
	f.pass(t, 2, 0)
	posts := f.gc.Inbound()
	if len(posts) != 3 {
		t.Fatalf("posts=%d, want 3", len(posts))
	}
	for i, post := range posts {
		if !strings.HasSuffix(post.Text, strings.Repeat(string(rune('a'+i)), 20000)) {
			t.Fatalf("act %d lost, duplicated or reordered", i)
		}
	}
}
