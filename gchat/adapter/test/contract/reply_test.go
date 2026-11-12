package contract_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// A file-backed runtime captures exactly what the real gc renderer delivers.
// It starts no model or terminal. Unsupported optional operations use the exec
// runtime's exit-2 fallback, including the protocol handshake and exec carrier.
const captureRuntime = `#!/bin/sh
set -eu
root=$(dirname "$0")/runtime-state
mkdir -p "$root"
state=$root/${2:-unused}
case $1 in
    start) mkdir -p "$state"; cat > "$state/config" ;;
    stop) rm -rf "$state" ;;
    is-running) if [ -d "$state" ]; then echo true; else echo false; fi ;;
    process-alive) echo true ;;
    is-attached) echo false ;;
    set-meta) cat > "$state/meta-$3" ;;
    get-meta) cat "$state/meta-$3" 2>/dev/null || true ;;
    remove-meta) rm -f "$state/meta-$3" ;;
    list-running) find "$root" -mindepth 1 -maxdepth 1 -type d -name "${2:-}*" -exec basename {} \; ;;
    nudge) cat > "$root/reminder.tmp"; mv "$root/reminder.tmp" "$root/reminder" ;;
    peek) printf 'Ready\n' ;;
    send-keys|interrupt|clear-scrollback) : ;;
    *) exit 2 ;;
esac
`

func TestRealGCReplyReminder(t *testing.T) {
	bin := os.Getenv("GC_BIN")
	if bin == "" {
		t.Skip("set GC_BIN to a real gc binary to run the rendered reply contract")
	}
	gc := startGC(t, bin, true)
	google := fakegoogle.New(t, nil)
	const space = "spaces/AAAA"
	const message = space + "/messages/QUESTION"
	const thread = space + "/threads/QUESTION"
	google.Configure(func(g *fakegoogle.Server) {
		g.Spaces[space] = &api.Space{Name: space, SpaceType: "SPACE", DisplayName: "Group A"}
		g.Spaces["spaces/BBBB"] = &api.Space{Name: "spaces/BBBB", SpaceType: "SPACE"}
		g.Messages[message] = &api.Message{Name: message, Thread: &api.Thread{Name: thread}, Text: "question"}
	})
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity", GCAPIBase: gc.base}, SecretsDir: gc.root,
		Endpoints: config.Endpoints{ChatAPIBase: google.URL(), StateFile: filepath.Join(gc.root, "reply-ledger.json")}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "example"}}
	c, err := chat.New(t.Context(), gc.client, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	client := gcclient.New(gc.base, "examplecity")
	adapter, err := server.New(server.Options{Config: cfg, Ledger: ledger.New(), LedgerMu: new(sync.Mutex), LedgerPath: cfg.Endpoints.StateFile, Chat: c, GC: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	adapter.SetHealth(server.Health{State: "ready"})
	h := httptest.NewServer(adapter)
	t.Cleanup(h.Close)
	reg, err := server.Registration(cfg, h.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	gc.request(t, http.MethodPost, "/v0/city/examplecity/sessions", map[string]string{"kind": "provider", "name": "example", "alias": "reply-contract"}, http.StatusAccepted)
	session := ""
	deadline := time.Now().Add(15 * time.Second)
	for session == "" && time.Now().Before(deadline) {
		rows := decode[struct {
			Items []struct{ ID, Alias, State string }
		}](t, gc.get(t, "/v0/city/examplecity/sessions"))
		for _, row := range rows.Items {
			if row.Alias == "reply-contract" && row.State != "creating" && row.State != "closed" {
				session = row.ID
			}
		}
		if session == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if session == "" {
		t.Fatal("reply session did not start")
	}
	conv := wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: space, Kind: "room"}
	for _, id := range []string{space, "spaces/BBBB"} {
		bound := conv
		bound.ConversationID = id
		gc.post(t, "bind", map[string]any{"conversation": bound, "session_id": session}, http.StatusOK)
	}
	_, err = client.Inbound(t.Context(), wire.InboundMessage{ProviderMessageID: message, Conversation: conv,
		Actor: wire.Actor{ID: "users/example", DisplayName: "Example"}, Text: "question", ReplyToMessageID: thread, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	printed := ""
	deadline = time.Now().Add(10 * time.Second)
	for printed == "" && time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(gc.root, "runtime-state", "reminder"))
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "gc gchat reply-current ") {
				printed = strings.TrimSpace(line)
			}
		}
		if printed == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if printed != "gc gchat reply-current --to "+message+" --body-file <path>" {
		t.Fatalf("real gc printed %q", printed)
	}
	// Import only the real command tree. Google and the service proxy are local
	// HTTP fixtures; the real gc executes its printed command with a scoped API
	// override, and the adapter uses real gc for binding and transcript recording.
	commands, err := filepath.Abs("../../../commands")
	if err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(gc.root, "command-pack")
	if err := os.MkdirAll(pack, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pack, "pack.toml"), "[pack]\nname = \"gchat\"\nversion = \"0.5.0\"\nschema = 2\n")
	if err := os.CopyFS(filepath.Join(pack, "commands"), os.DirFS(commands)); err != nil {
		t.Fatal(err)
	}
	cityPack := filepath.Join(gc.root, "examplecity", "pack.toml")
	existing, err := os.ReadFile(cityPack)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cityPack, string(existing)+"\n[imports.gchat]\nsource = \"../command-pack\"\n")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v0/city/examplecity/svc/gchat"
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		adapter.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	body := filepath.Join(gc.root, "reply.txt")
	writeFile(t, body, "answer to A")
	args := strings.Fields(printed)[1:]
	args[len(args)-1] = body
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Dir = filepath.Join(gc.root, "examplecity")
	cmd.Env = append(append([]string{}, gc.env...), "GC_API_BASE_URL="+proxy.URL, "GC_SESSION_ID="+session)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("printed command: %v; %s", err, stderr.String())
	}
	got := decode[server.CommandResult](t, []byte(stdout.String()))
	if !got.Delivered || !got.Recorded || got.ConversationID != space || got.Thread != thread || got.InReplyTo != message {
		t.Fatalf("printed command outcome: %s", stdout.String())
	}
	google.Configure(func(g *fakegoogle.Server) {
		if len(g.Messages) != 2 || g.Messages[got.MessageID] == nil || !strings.HasPrefix(got.MessageID, space+"/messages/") {
			t.Fatal("printed command posted outside its turn")
		}
	})
	t.Logf("real gc %s rendered and executed %s; reply delivered and recorded in %s", bin, printed, space)
}
