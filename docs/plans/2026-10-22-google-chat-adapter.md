# Google Chat Adapter Pack (`gchat`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A generic Gas City pack, `packs/gchat`, whose supervised Go adapter lets a city's agents live in Google Chat under one Workspace user identity: every message in the identity's spaces becomes a gc turn, replies land in the right thread, files and reactions work both ways, and nothing is lost across outages.

**Architecture:** One Go binary (`gc-gchat-adapter`) supervised by gc as a `proxy_process` service. It holds the identity's OAuth token, keeps a Workspace Events API subscription over all the identity's spaces, streams the resulting events out of Pub/Sub, turns them into gc inbound messages, and serves gc's publish callback plus a small command surface over the Unix socket gc hands it. A single JSON ledger file is the source of truth for delivery state. Every Google endpoint is configurable, so the whole binary runs against in-process fakes in tests. The spec is `docs/superpowers/specs/2026-10-22-google-chat-adapter-design.md`; when this plan and the spec disagree, the spec wins and the plan gets a fix.

**Tech Stack:** Go 1.27 (this machine has go1.27.2); `google.golang.org/api v0.307.0` (packages `chat/v1`, `workspaceevents/v1`, `option`, `googleapi`); `cloud.google.com/go/pubsub/v2 v2.8.0` (and its `pstest` fake); `golang.org/x/oauth2 v0.37.0`; `github.com/BurntSushi/toml v1.6.0`; `google.golang.org/grpc v1.84.0`; standard library for everything else (net/http over a Unix socket, encoding/json, os, time). Shell wrappers are POSIX `sh`. No other dependencies.

**Lane discipline (goes into every bead):** work only in the lane's worktree on the bead's branch; never touch the city root or another lane; delete nothing; a dcg block is never a reason to wait for a human: take the safe alternative it names (write files through the editor tool or a Go program, not a shell redirect onto an existing home path), or file a papercut bead and continue; a destructive step that is truly needed goes into the READY mail as an ask to the mayor. No network in tests except the in-process fakes; the contract test is skipped unless `GC_BIN` is set. Nothing APM-, Bonanza- or Syndi-specific in code, config examples, or tests: the identity in examples is `example` and the city `examplecity`. Commit after every green step. Run `gofmt -l`, `go vet ./...`, `staticcheck ./...` and `shellcheck` before READY.

---

## File structure

```
packs/gchat/
  pack.toml                          # [pack] + one [[service]] gchat (proxy_process)
  city-fragment.toml                 # [[extmsg.default_route]] provider="gchat" agent="mayor"
  README.md                          # Google-side checklist (spec §8), setup, secrets, settings, commands
  CHANGELOG.md
  doctor/gchat/doctor.toml           # description
  doctor/gchat/run.sh                # secrets present, token refreshes, subscription alive, route set
  commands/auth/command.toml         # each: description + run = "../<verb>.sh"
  commands/auth.sh                   # execs the binary's auth subcommand directly
  commands/publish/command.toml  commands/publish.sh      # -> /commands/publish via gc's /svc/gchat proxy
  commands/reply-current/command.toml  commands/reply-current.sh
  commands/react/command.toml    commands/react.sh        # -> /react
  commands/upload/command.toml   commands/upload.sh       # -> /commands/upload
  commands/read/command.toml     commands/read.sh         # -> /read
  commands/spaces/command.toml   commands/spaces.sh       # -> /spaces (list; --wake sets)
  commands/status/command.toml   commands/status.sh       # -> /status
  commands/resolve/command.toml  commands/resolve.sh      # -> /resolve (unresolved deliveries)
  commands/state/command.toml    commands/state.sh        # -> /state (restore, reset)
  adapter/
    go.mod  go.sum                   # module gchat-adapter (independent module, like slack-full)
    run.sh                           # resolves the secrets env file, builds if needed, execs the binary
    build.sh                         # go build -o ./gc-gchat-adapter ./cmd/gc-gchat-adapter
    cmd/gc-gchat-adapter/main.go     # subcommands: serve (default), auth, state restore|reset, version
    internal/config/config.go        # Secrets (env) + Settings (config.toml) + Endpoints
    internal/config/config_test.go
    internal/wire/wire.go            # gc wire types (inbound message, publish request/receipt, registration)
    internal/wire/wire_test.go
    internal/ledger/ledger.go        # the state file: types, atomic save, recovery-required
    internal/ledger/ledger_test.go
    internal/markdown/markdown.go    # markdown -> Chat text; split into parts
    internal/markdown/markdown_test.go
    internal/token/token.go          # token source, file watch, auth flow
    internal/token/token_test.go
    internal/chat/chat.go            # thin Chat API client: messages, reactions, media, spaces, spaceEvents
    internal/chat/chat_test.go
    internal/events/events.go        # Workspace Events subscription lifecycle (create/renew/reconcile)
    internal/events/events_test.go
    internal/puller/puller.go        # Pub/Sub streaming pull -> decoded events -> handler; ack/nack
    internal/puller/puller_test.go
    internal/inbound/inbound.go      # event -> changes -> gc inbound; wake policy; dedup; delivery outcome
    internal/inbound/inbound_test.go
    internal/catchup/catchup.go      # frozen interval, messages.list + spaceEvents.list, backlog
    internal/catchup/catchup_test.go
    internal/gcclient/gcclient.go    # gc API: register, inbound, receipt poll, outbound
    internal/gcclient/gcclient_test.go
    internal/server/server.go        # the Unix-socket HTTP server: /publish, /commands/*, /react, ...
    internal/server/server_test.go
    internal/app/app.go              # wiring: startup order, health state, degraded reasons
    internal/fakes/fakegoogle/fakegoogle.go   # httptest server: OAuth token, Events API, Chat API
    internal/fakes/fakegc/fakegc.go           # httptest server: gc's extmsg endpoints + receipt states
    test/e2e/e2e_test.go             # scenarios from spec §6
    test/contract/contract_test.go   # against a real gc (GC_BIN), skipped otherwise
```

Each file has one responsibility and is small enough to hold in context. Tests sit beside the code they test. The fakes are packages, not test files, so the e2e and contract tests can import them.

Everything under `packs/gchat/adapter` is one Go module named `gchat-adapter`. Run all Go commands from `packs/gchat/adapter`.

---

### Task 1: Module scaffold, wire types, config

This task fixes the contracts every other task compiles against. Do it first and alone.

**Files:**
- Create: `packs/gchat/adapter/go.mod`
- Create: `packs/gchat/adapter/internal/wire/wire.go`, `wire_test.go`
- Create: `packs/gchat/adapter/internal/config/config.go`, `config_test.go`
- Create: `packs/gchat/adapter/cmd/gc-gchat-adapter/main.go` (version subcommand only for now)

- [ ] **Step 1: Create the module**

```bash
mkdir -p packs/gchat/adapter && cd packs/gchat/adapter
go mod init gchat-adapter
go get google.golang.org/api@v0.307.0 cloud.google.com/go/pubsub/v2@v2.8.0 golang.org/x/oauth2@v0.37.0 github.com/BurntSushi/toml@v1.6.0 google.golang.org/grpc@v1.84.0
```
Do not run `go mod tidy` yet: on a module with no packages it drops all five pins (seen in the first build, papercut bz-rrn1). Run `go mod tidy` after Step 4, once the first package exists, and then confirm the five pins are still present.
Expected: `go.mod` lists those five with those versions; `go build ./...` prints nothing once a package exists.

- [ ] **Step 2: Write the failing wire test**

`internal/wire/wire_test.go`:
```go
package wire

import (
	"encoding/json"
	"testing"
	"time"
)

func TestInboundMessageJSON(t *testing.T) {
	m := InboundMessage{
		ProviderMessageID: "spaces/AAA/messages/BBB",
		Conversation: Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example",
			ConversationID: "spaces/AAA", Kind: "room"},
		Actor:            Actor{ID: "users/1", DisplayName: "Pat", IsBot: false},
		Text:             "hello",
		ReplyToMessageID: "spaces/AAA/threads/CCC",
		Attachments:      []Attachment{{ProviderID: "a1", URL: "file:///tmp/x.png", MIMEType: "image/png"}},
		ReceivedAt:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	b, err := json.Marshal(InboundEnvelope{Message: m})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"message":{"provider_message_id":"spaces/AAA/messages/BBB","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"actor":{"id":"users/1","display_name":"Pat","is_bot":false},"text":"hello","reply_to_message_id":"spaces/AAA/threads/CCC","attachments":[{"provider_id":"a1","url":"file:///tmp/x.png","mime_type":"image/png"}],"received_at":"2026-01-02T03:04:05Z"}}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestPublishReceiptJSON(t *testing.T) {
	r := PublishReceipt{MessageID: "spaces/AAA/messages/BBB",
		Conversation: Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/AAA", Kind: "room"},
		Delivered:    true, Metadata: map[string]string{"thread": "spaces/AAA/threads/CCC", "parts": "2"}}
	b, _ := json.Marshal(r)
	want := `{"message_id":"spaces/AAA/messages/BBB","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"delivered":true,"metadata":{"parts":"2","thread":"spaces/AAA/threads/CCC"}}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
	f := PublishReceipt{Conversation: r.Conversation, Delivered: false, FailureKind: "rate_limited", RetryAfter: int64(2 * time.Second)}
	b, _ = json.Marshal(f)
	want = `{"conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"delivered":false,"failure_kind":"rate_limited","retry_after":2000000000}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestRegistrationCapabilitiesPascalCase(t *testing.T) {
	b, _ := json.Marshal(Registration{Provider: "gchat", AccountID: "example", Name: "gchat adapter",
		CallbackURL: "http://127.0.0.1:8372/v0/city/examplecity/svc/gchat",
		Capabilities: Capabilities{SupportsAttachments: true, MaxMessageLength: 32000},
		ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"})
	want := `{"provider":"gchat","account_id":"example","name":"gchat adapter","callback_url":"http://127.0.0.1:8372/v0/city/examplecity/svc/gchat","capabilities":{"SupportsAttachments":true,"SupportsChildConversations":false,"MaxMessageLength":32000},"reply_instructions":"gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}
```

Note for the registration test: `json.Marshal` escapes `<` and `>` as `\u003c` and `\u003e`, so marshal through a `json.Encoder` with `SetEscapeHTML(false)` (and trim the trailing newline) for the `<path>` in the golden string to compare byte for byte (papercut bz-q328). The runtime wire types are unaffected.

- [ ] **Step 3: Run it to see it fail**

Run: `go test ./internal/wire/ -run . -v`
Expected: FAIL, "undefined: InboundMessage".

- [ ] **Step 4: Write `internal/wire/wire.go`**

```go
// Package wire holds the exact JSON shapes gc exchanges with an extmsg adapter.
// Field names and casing come from gc's internal/extmsg/types.go and
// internal/api/huma_types_extmsg.go; do not "fix" them.
package wire

import "time"

// Conversation is gc's ConversationRef. ScopeID is required by gc (the city name).
type Conversation struct {
	ScopeID              string `json:"scope_id"`
	Provider             string `json:"provider"`
	AccountID            string `json:"account_id"`
	ConversationID       string `json:"conversation_id"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	Kind                 string `json:"kind"` // "dm" | "room" | "thread"
}

type Actor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

type Attachment struct {
	ProviderID string `json:"provider_id"`
	URL        string `json:"url"`
	MIMEType   string `json:"mime_type,omitempty"`
}

// InboundMessage is gc's ExternalInboundMessage.
type InboundMessage struct {
	ProviderMessageID string       `json:"provider_message_id"`
	Conversation      Conversation `json:"conversation"`
	Actor             Actor        `json:"actor"`
	Text              string       `json:"text"`
	ExplicitTarget    string       `json:"explicit_target,omitempty"`
	ReplyToMessageID  string       `json:"reply_to_message_id,omitempty"`
	Attachments       []Attachment `json:"attachments,omitempty"`
	DedupKey          string       `json:"dedup_key,omitempty"`
	ReceivedAt        time.Time    `json:"received_at"`
}

// InboundEnvelope is the body of POST /v0/city/{city}/extmsg/inbound (pre-normalized path).
type InboundEnvelope struct {
	Message InboundMessage `json:"message"`
}

// InboundResponse is what gc answers. Delivery.Status is one of
// delivered | pending | partial | failed | no_route; ReceiptID is set while a
// fan-out is still running.
type InboundResponse struct {
	Delivery struct {
		Status    string `json:"status"`
		ReceiptID string `json:"receipt_id,omitempty"`
	} `json:"delivery"`
}

// ReceiptResponse is gc's answer to GET .../extmsg/inbound/receipts/{id}:
// State is pending | concluded | unknown; Delivery is set when concluded.
type ReceiptResponse struct {
	State    string `json:"state"`
	Delivery struct {
		Status string `json:"status"`
	} `json:"delivery"`
}

// PublishRequest is what gc POSTs to <callback_url>/publish.
type PublishRequest struct {
	SessionID        string            `json:"session_id,omitempty"`
	Conversation     Conversation      `json:"conversation"`
	Text             string            `json:"text"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// PublishReceipt is the adapter's answer, always with HTTP 200. Metadata values
// must be strings; RetryAfter is a Go duration in nanoseconds.
type PublishReceipt struct {
	MessageID    string            `json:"message_id,omitempty"`
	Conversation Conversation      `json:"conversation"`
	Delivered    bool              `json:"delivered"`
	FailureKind  string            `json:"failure_kind,omitempty"` // transient | rate_limited | auth | not_found | permanent
	RetryAfter   int64             `json:"retry_after,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// Capabilities is serialized by gc with PascalCase keys (untagged struct there).
type Capabilities struct {
	SupportsAttachments        bool `json:"SupportsAttachments"`
	SupportsChildConversations bool `json:"SupportsChildConversations"`
	MaxMessageLength           int  `json:"MaxMessageLength"`
}

// Registration is the body of POST /v0/city/{city}/extmsg/adapters.
type Registration struct {
	Provider          string       `json:"provider"`
	AccountID         string       `json:"account_id"`
	Name              string       `json:"name,omitempty"`
	CallbackURL       string       `json:"callback_url,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
	ReplyInstructions string       `json:"reply_instructions,omitempty"`
}

// OutboundRequest is the body of POST /v0/city/{city}/extmsg/outbound.
type OutboundRequest struct {
	SessionID        string       `json:"session_id"`
	Conversation     Conversation `json:"conversation"`
	Text             string       `json:"text,omitempty"`
	ReplyToMessageID string       `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string       `json:"idempotency_key,omitempty"`
}

// OutboundResult is gc's PascalCase answer; TranscriptEntry nil means gc
// delivered but did not record.
type OutboundResult struct {
	Receipt struct {
		Delivered bool   `json:"Delivered"`
		MessageID string `json:"MessageID"`
	} `json:"Receipt"`
	TranscriptEntry *map[string]any `json:"TranscriptEntry"`
}

// RequestHeader is required by gc on every mutation. Any non-empty value.
const RequestHeader = "X-GC-Request"
```
Note for the worker: confirm `InboundResponse`, `ReceiptResponse` and `OutboundResult` field names against the gc source at `GASCITY_SRC` (default `$HOME/code/gascity`): `internal/api/huma_types_extmsg.go` and `internal/extmsg/inbound_delivery.go`, `inbound_receipt_store.go`, `outbound.go`. If a name differs, fix the struct and its test; note the fix in the commit message.

- [ ] **Step 5: Run the wire tests**

Run: `go test ./internal/wire/ -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Write the failing config test**

`internal/config/config_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretsAndSettings(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	os.WriteFile(env, []byte("GCHAT_PROJECT_ID=proj\nGCHAT_PUBSUB_TOPIC=projects/proj/topics/t\nGCHAT_PUBSUB_SUBSCRIPTION=projects/proj/subscriptions/s\nGCHAT_OAUTH_CLIENT_FILE="+filepath.Join(dir, "client.json")+"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL=http://127.0.0.1:8372\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[defaults]
wake = "all"
[[identity]]
name = "example"
agent = "mayor"
[[space]]
id = "spaces/AAA"
wake = "mentions"
[media]
max_bytes = 1000
`), 0o600)
	c, err := Load(env, filepath.Join(dir, "config.toml"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Secrets.ProjectID != "proj" || c.Secrets.CityName != "examplecity" {
		t.Fatalf("secrets: %+v", c.Secrets)
	}
	if c.Secrets.TokenFile != filepath.Join(dir, "token.json") {
		t.Fatalf("token file default: %s", c.Secrets.TokenFile)
	}
	if c.Settings.Identities[0].Name != "example" || c.Settings.Identities[0].Agent != "mayor" {
		t.Fatalf("identity: %+v", c.Settings.Identities)
	}
	if c.Wake("spaces/AAA") != WakeMentions || c.Wake("spaces/ZZZ") != WakeAll {
		t.Fatal("wake policy")
	}
	if c.Settings.Media.MaxBytes != 1000 || c.Endpoints.ChatAPIBase != "https://chat.googleapis.com" {
		t.Fatalf("defaults: %+v %+v", c.Settings.Media, c.Endpoints)
	}
}

func TestMissingRequiredSecretNamesIt(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	os.WriteFile(env, []byte("GCHAT_PROJECT_ID=proj\n"), 0o600)
	_, err := Load(env, filepath.Join(dir, "config.toml"), dir)
	if err == nil || !contains(err.Error(), "GCHAT_PUBSUB_TOPIC") {
		t.Fatalf("want error naming GCHAT_PUBSUB_TOPIC, got %v", err)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 7: Run it to see it fail**

Run: `go test ./internal/config/ -v`
Expected: FAIL, "undefined: Load".

- [ ] **Step 8: Write `internal/config/config.go`**

```go
// Package config loads the per-city secrets (an env file) and settings (a TOML
// file). Nothing here knows any particular city.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Secrets struct {
	ProjectID       string // GCHAT_PROJECT_ID
	Topic           string // GCHAT_PUBSUB_TOPIC  projects/<id>/topics/<name>
	Subscription    string // GCHAT_PUBSUB_SUBSCRIPTION
	OAuthClientFile string // GCHAT_OAUTH_CLIENT_FILE
	TokenFile       string // GCHAT_TOKEN_FILE, default <secrets dir>/token.json
	CityName        string // GC_CITY_NAME
	GCAPIBase       string // GC_API_BASE_URL
	ServiceSocket   string // GC_SERVICE_SOCKET (set by gc)
	ServiceURLPrefix string // GC_SERVICE_URL_PREFIX (set by gc)
}

type Endpoints struct {
	ChatAPIBase    string // GCHAT_CHAT_API_BASE,    default https://chat.googleapis.com
	EventsAPIBase  string // GCHAT_EVENTS_API_BASE,  default https://workspaceevents.googleapis.com
	PubSubEndpoint string // GCHAT_PUBSUB_ENDPOINT,  default "" (library default); host:port for the fake, insecure
	OAuthAuthURL   string // GCHAT_OAUTH_AUTH_URL,   default https://accounts.google.com/o/oauth2/auth
	OAuthTokenURL  string // GCHAT_OAUTH_TOKEN_URL,  default https://oauth2.googleapis.com/token
	MediaDir       string // GCHAT_MEDIA_DIR,        default <city>/.gc/gchat-media/inbound
	UploadRoot     string // GCHAT_UPLOAD_ROOT,      default <city>/.gc/gchat-out
	StateFile      string // GCHAT_STATE_FILE,       default <secrets dir>/../state.json
}

type WakePolicy string

const (
	WakeAll      WakePolicy = "all"
	WakeMentions WakePolicy = "mentions"
)

type Identity struct {
	Name      string `toml:"name"`
	Agent     string `toml:"agent"`
	TokenFile string `toml:"token_file"`
}

type Space struct {
	ID   string     `toml:"id"`
	Wake WakePolicy `toml:"wake"`
}

type Media struct {
	InboundDir string `toml:"inbound_dir"`
	UploadRoot string `toml:"upload_root"`
	MaxBytes   int64  `toml:"max_bytes"`
}

type Settings struct {
	Defaults struct {
		Wake WakePolicy `toml:"wake"`
	} `toml:"defaults"`
	Identities []Identity `toml:"identity"`
	Spaces     []Space    `toml:"space"`
	Media      Media      `toml:"media"`
	CatchupBudget int     `toml:"catchup_budget"` // per space per pass, default 50
}

type Config struct {
	Secrets   Secrets
	Settings  Settings
	Endpoints Endpoints
	SecretsDir string
}

// Load reads the env file (KEY=VALUE lines, # comments), the settings file
// (a missing or unreadable file is an error naming its path, per spec section 5;
// keys omitted from an existing file take the defaults: one identity "default"/"mayor", wake all), and the
// endpoint overrides from the process environment. secretsDir is where the
// token file and state file default to.
func Load(envFile, settingsFile, secretsDir string) (*Config, error) { /* worker implements: parse env file into a map; apply to Secrets; required keys GCHAT_PROJECT_ID, GCHAT_PUBSUB_TOPIC, GCHAT_PUBSUB_SUBSCRIPTION, GCHAT_OAUTH_CLIENT_FILE, GC_CITY_NAME, GC_API_BASE_URL; error names every missing key; GC_SERVICE_SOCKET/GC_SERVICE_URL_PREFIX from the process env; settings via toml.DecodeFile when present; defaults as documented; Media.MaxBytes default 209715200; CatchupBudget default 50; Endpoints from env with defaults; identity token_file relative to secretsDir */ }

// Wake returns the policy for a space: the space's own setting, else the default.
func (c *Config) Wake(spaceID string) WakePolicy { /* worker implements */ }

// Identity returns the single configured identity (the first entry).
func (c *Config) Identity() Identity { /* worker implements */ }
```
The worker writes the bodies; the helper for the env file is a `bufio.Scanner` over lines, trimming, skipping blanks and `#`, splitting on the first `=`, stripping one layer of matching quotes.

- [ ] **Step 9: Run config tests**

Run: `go test ./internal/config/ -v`
Expected: PASS (2 tests).

- [ ] **Step 10: `main.go` with the version subcommand**

`cmd/gc-gchat-adapter/main.go`: a `main` that dispatches on `os.Args[1]`: `version` prints `gc-gchat-adapter <Version>` where `Version` is a package-level `var Version = "dev"` (set by `-ldflags` in `build.sh`); `serve`, `auth`, `state` print "not implemented yet" and exit 2 for now. `go build ./... && go run ./cmd/gc-gchat-adapter version` prints `gc-gchat-adapter dev`.

- [ ] **Step 11: Commit**

```bash
git add packs/gchat/adapter
git commit -m "gchat: module scaffold, gc wire types, config loader"
```

---

### Task 2: Ledger (the state file)

**Files:** `internal/ledger/ledger.go`, `ledger_test.go`

The ledger is the only delivery state. Types (all JSON, snake_case tags):

```go
type Ledger struct {
	Version      int                       `json:"version"` // 1
	Subscription SubscriptionState         `json:"subscription"`
	Spaces       map[string]*SpaceState    `json:"spaces"`        // by space name
	Transport    TransportDedup            `json:"transport"`     // recent pubsub + cloudevent ids, bounded ring of 10000
	Messages     map[string]*MessageState  `json:"messages"`      // by Chat message name
	Changes      map[string]*ChangeState   `json:"changes"`       // by provider_message_id (the change id)
	Sends        map[string]*SendState     `json:"sends"`         // by idempotency key
	Unresolved   map[string]*ChangeState   `json:"unresolved"`    // changes needing an operator decision
	Quarantine   []QuarantinedEvent        `json:"quarantine"`
	Counters     map[string]int64          `json:"counters"`      // turns per space, and global counters
}
type SubscriptionState struct { Name, State string; Expiry time.Time; Target string }
type SpaceState struct { Kind string; Wake string; MessageCursor, EventCursor time.Time; Backlog []string /* change ids */; PageToken string; FrozenUpper time.Time }
type MessageState struct { Revision time.Time; Deleted bool; Thread string; Mine bool; SendKey string }
type ChangeState struct { ID, Space, Message, Kind string; Status string /* intent|pending|committed|unresolved */; ReceiptID string; Attempts int; Created time.Time; Event string; Reason string }
type SendState struct { Key string; Space, Thread string; Parts []SendPart; Receipt *wire.PublishReceipt; Recorded bool; Created time.Time }
type SendPart struct { RequestID, Text, MessageName string }
type QuarantinedEvent struct { PubSubID string; Reason string; Path string; At time.Time }
```

Functions: `Load(path string, established bool) (*Ledger, error)` returns `ErrRecoveryRequired` (with the preserved copy's path, written as `<path>.corrupt-<unix>`) when the file is unreadable JSON, or missing while `established` is true; `New()`; `(*Ledger) Save(path string) error` writes `<path>.tmp` then renames; `Established(secretsDir) bool` and `MarkEstablished(secretsDir)` use a zero-byte file `<secretsDir>/.gchat-established`.

Tests (write first, then implement): round trip of a populated ledger; `Save` leaves no `.tmp` behind and a crash simulation (write tmp, no rename) leaves the old file intact; corrupt JSON returns `ErrRecoveryRequired` and preserves the bytes; missing file with `established=false` returns a new ledger; missing with `established=true` returns `ErrRecoveryRequired`; the transport ring drops the oldest id past 10000.

Commit: `git commit -m "gchat: ledger with atomic save and recovery-required"`.

---

### Task 3: Markdown to Chat text, and splitting

**Files:** `internal/markdown/markdown.go`, `markdown_test.go`

`func ToChat(md string) string`: `**x**`/`__x__` → `*x*`; `*x*`/`_x_` → `_x_`; `~~x~~` → `~x~`; inline code stays backticked; fenced blocks stay fenced; `- ` and `* ` bullets → `* `; `[text](url)` → `<url|text>`; `#` headings → bold line. Nothing else is touched.

`func Split(text string, limit int) []string`: parts of at most `limit` bytes; prefer paragraph boundaries (blank lines); a paragraph over the limit is split at the last space before the limit, else at a UTF-8 rune boundary; inside a fenced code block, a split closes the fence (` ``` `) at the end of the part and reopens it with the same info string at the start of the next, with those bytes counted against the limit; the result never has an empty part; joining the parts (with fences removed) round-trips the text.

Tests: each formatting rule; a 70,000-byte three-paragraph text splits in three at the blank lines; a single 40,000-byte paragraph splits at a space; a 40,000-byte code block splits with closed/reopened fences and every part ≤ 32,000; multibyte text (Japanese and emoji) never splits inside a rune; `Split("", n)` is empty.

Commit: `git commit -m "gchat: markdown to Chat text and size splitting"`.

---

### Task 4: Token source, file watch, auth flow

**Files:** `internal/token/token.go`, `token_test.go`

```go
type Source struct { /* client config, token file, endpoints, mu, current *oauth2.Token, onReload func() */ }
func New(clientFile, tokenFile string, ep config.Endpoints, scopes []string) (*Source, error)
func (s *Source) Client(ctx context.Context) (*http.Client, error) // refreshes via oauth2, ErrNeedsSignIn on invalid_grant
func (s *Source) TokenSource(ctx context.Context) oauth2.TokenSource
func (s *Source) Watch(ctx context.Context, onReload func()) // polls the token file's mtime every 2s; on change: parse, validate with validateFn, swap, call onReload; invalid -> keep old, log
func (s *Source) State() State // Ready | NeedsSignIn | Missing
func RunAuth(ctx context.Context, clientFile, tokenFile string, ep config.Endpoints, scopes []string, stdout io.Writer, open func(url string) error) error
```
`RunAuth`: loopback listener on `127.0.0.1:0`, PKCE, prints the URL (and calls `open`), waits for the redirect, exchanges the code at `OAuthTokenURL`, checks every requested scope is in the granted `scope` field (else error naming the missing ones, nothing written), writes `<tokenFile>.tmp` mode 0600 then renames. Errors: cancelled context (nothing written), `access_denied`, listener bind failure (names the port), write failure (names the error, old file kept).

Tests with an `httptest` OAuth server: refresh success; `invalid_grant` → `ErrNeedsSignIn` and `State()==NeedsSignIn`; `Watch` sees an atomic replacement and calls `onReload` once; an invalid replacement keeps the old token; `RunAuth` end to end with a fake browser that GETs the redirect URL with a code, token file written 0600, partial scopes rejected, cancel writes nothing.

Commit: `git commit -m "gchat: oauth token source, file watch, auth flow"`.

---

### Task 5: Fakes: Google (OAuth, Events API, Chat API) and gc

**Files:** `internal/fakes/fakegoogle/fakegoogle.go`, `internal/fakes/fakegc/fakegc.go` (+ a small `_test.go` each that the fake answers its own routes).

`fakegoogle.New(t testing.TB, topic *pstest.Server-connected publisher) *Server` with `URL()` and `Close()`. Routes, all JSON, with an in-memory store:
- `POST /token`: returns `{"access_token":"fake","expires_in":3600,"token_type":"Bearer","scope":"<all requested>"}`; a test can set `s.TokenError = "invalid_grant"`.
- Events API: `POST /v1/subscriptions` → Operation (`{"name":"operations/1","done":false}`; `s.OperationDelay`), `GET /v1/operations/{id}`, `GET /v1/subscriptions/{id}` → Subscription (`name`, `targetResource`, `eventTypes`, `state` ACTIVE|SUSPENDED, `expireTime`), `PATCH /v1/subscriptions/{id}` (renew; `ttl`), `POST /v1/subscriptions/{id}:reactivate`, `GET /v1/subscriptions?filter=...` (paginated). Switches: `s.CreateReturnsAlreadyExists`, `s.DropNextResponse`, `s.SuspendSubscription()`.
- Chat API: `GET /v1/spaces` (paginated, `pageSize`), `POST /v1/spaces/{s}/messages` (honors `requestId`: same id → same message; honors `thread.name` and `messageReplyOption`; rejects the option in DM spaces with 400; counts writes per space per second and answers 429 with `Retry-After` past 1/s when `s.EnforceRate`), `GET /v1/spaces/{s}/messages` (filter per the documented grammar: `create_time > "..."` and an unquoted `thread.name = spaces/x/threads/y`, paginated), `GET /v1/spaces/{s}/messages/{m}`, `GET /v1/spaces/{s}/spaceEvents` (filter per the documented grammar: `event_types:` required, else INVALID_ARGUMENT, and `start_time=` / `end_time=`, paginated, 28-day window), `POST /v1/spaces/{s}/messages/{m}/reactions`, `GET .../reactions`, `DELETE .../reactions/{r}`, `POST /upload/v1/{parent}/attachments:upload` → attachmentDataRef, `GET /v1/media/{name}?alt=media`.
- `s.Emit(eventType string, resource any, space string)` publishes a CloudEvent-shaped Pub/Sub message to the pstest topic with attributes `ce-type`, `ce-subject`, `ce-id`, and a `data` body `{"<wrapper>": resource}` (`message`, `reaction`, `membership`) or the batch array form.

`fakegc.New(t) *Server` with `URL()`: `POST /v0/city/{city}/extmsg/adapters` (requires `X-GC-Request`; the registration body has no `scope_id`, it is the inbound and outbound `conversation.scope_id` that gc requires, and the fake rejects a missing one with 400), `POST /v0/city/{city}/extmsg/inbound` (records the message; answers with a scripted delivery status from `s.NextDelivery` default delivered; `pending` answers get a receipt id), `GET .../extmsg/inbound/receipts/{id}` (states per `s.Receipts[id]`; eviction via `s.EvictReceipt(id)`), `POST .../extmsg/outbound` (calls the adapter's registered callback `/publish`, returns the PascalCase `OutboundResult`; `s.FailTranscript` makes `TranscriptEntry` null). `s.Inbound()` returns every message received, in order; `s.Published()` every callback it made.

Tests: each fake route answers; `Emit` produces a message a `pubsub/v2` client receives with the attributes.

Commit: `git commit -m "gchat: in-process fakes for Google and gc"`.

---

### Task 6: Chat API client

**Files:** `internal/chat/chat.go`, `chat_test.go`

```go
type Client struct { /* *chat.Service built with option.WithEndpoint(ep.ChatAPIBase), option.WithHTTPClient(authed) */ }
func New(ctx context.Context, httpClient *http.Client, ep config.Endpoints) (*Client, error)
func (c *Client) ListSpaces(ctx) ([]Space, error)                                        // all pages
func (c *Client) CreateMessage(ctx, space, text, thread, requestID string, named bool) (name, threadName string, err error) // named: use messageReplyOption
func (c *Client) ListMessages(ctx, space string, since time.Time, thread string, page func([]*chat.Message) error) error
func (c *Client) GetMessage(ctx, name string) (*chat.Message, error)
func (c *Client) ListSpaceEvents(ctx, space string, since time.Time, types []string, page func([]*chat.SpaceEvent) error) error
func (c *Client) CreateReaction(ctx, message, emoji string) (string, error)
func (c *Client) DeleteReaction(ctx, message, emoji string) error                        // list then delete the identity's own
func (c *Client) UploadAttachment(ctx, space, path string) (*chat.AttachmentDataRef, error)
func (c *Client) DownloadMedia(ctx, resourceName, dest string, maxBytes int64) (int64, error)
```
Plus `type Limiter` with one token per second per space and five per second for reactions, and `Classify(err) (kind string, retryAfter time.Duration)` mapping `googleapi.Error` codes: 429 → rate_limited (honor `Retry-After`), 401/403 → auth (403 on a space → "not_found" when the message says the caller is not a member), 404 → not_found, other 4xx → permanent, 5xx and network → transient.

Tests against `fakegoogle`: create with the same requestId twice returns one message; the reply option is sent only when `named`; 429 → rate_limited with the hint; pagination of spaces and messages; reaction create and delete; upload then a message with the attachment; download respects `maxBytes`.

Commit: `git commit -m "gchat: chat api client with limits and error classes"`.

---

### Task 7: Workspace Events subscription lifecycle

**Files:** `internal/events/events.go`, `events_test.go`

```go
type Manager struct { /* workspaceevents.Service, topic, target "//chat.googleapis.com/spaces/-", event types, ledger, clock, log */ }
var EventTypes = []string{ "google.workspace.chat.message.v1.created", ".updated", ".deleted", "google.workspace.chat.reaction.v1.created", ".deleted", "google.workspace.chat.membership.v1.created", ".updated", ".deleted" } // full strings in code
func (m *Manager) Reconcile(ctx) (SubscriptionState, error) // the one entry point: see below
func (m *Manager) RenewLoop(ctx)                             // sleeps until expiry minus margin (25% of ttl), then Reconcile
func (m *Manager) HandleLifecycle(ctx, eventType string)    // expirationReminder | expired | suspended -> Reconcile
```
`Reconcile`: if the ledger has a name, `Get`; else `List` by target to discover. If none: `Create` with `payloadOptions.includeResource=true`, `ttl` omitted (max), wait for the Operation (poll `operations.get` with backoff up to 2 minutes), persist name/expiry/state. If `ALREADY_EXISTS` on create: list, adopt. If ACTIVE and expiry within margin: `Patch` with `ttl: "0s"` (max) via its Operation, persist the returned expiry. If SUSPENDED: `Reactivate`, wait, persist. On a lost response (timeout) do nothing but retry `Reconcile` after backoff; never create a second subscription blindly.

Tests with `fakegoogle`: fresh create persists the returned expiry; renewal before expiry; a lost create response then discovery adopts the existing one; `ALREADY_EXISTS` adopts; suspended → reactivated; `RenewLoop` with a fake clock fires at 75% of the ttl; lifecycle events call Reconcile.

Commit: `git commit -m "gchat: workspace events subscription manager"`.

---

### Task 8: Pub/Sub puller and event decoding

**Files:** `internal/puller/puller.go`, `puller_test.go`

```go
type Event struct { PubSubID, CloudEventID, Type, Subject string; Changes []Change; Raw []byte }
type Change struct { Kind string /* message.created|message.updated|message.deleted|reaction.created|reaction.deleted|membership.created|membership.updated|membership.deleted|subscription.expirationReminder|... */; Message *chat.Message; Reaction *chat.Reaction; Membership *chat.Membership; Space string }
func Decode(msg *pubsub.Message) (*Event, error) // singular and batch (wrapper arrays) forms; unknown ce-type -> Event with no Changes and Type set
type Handler func(ctx context.Context, ev *Event) (Outcome, error) // Outcome: Ack | Nack | Quarantine
func Run(ctx context.Context, sub *pubsub.Subscriber, h Handler, log Logger) error // Receive loop; extends deadline by using the client's automatic lease management; Ack/Nack per Outcome; Quarantine = write Raw to <mediaDir>/../quarantine/<id>.json then Ack
```
Tests with `pstest`: a singular message event decodes to one change; a `batchCreated` with three decodes to three; an unknown type decodes to zero changes and is acked; a handler returning Nack leads to redelivery (pstest redelivers after the ack deadline; use a 1s deadline in the test); Quarantine writes the file and acks.

Commit: `git commit -m "gchat: pub/sub streaming pull and event decoding"`.

---

### Task 9: gc client

**Files:** `internal/gcclient/gcclient.go`, `gcclient_test.go`

```go
type Client struct { base, city string; http *http.Client }
func New(base, city string) *Client
func (c *Client) Register(ctx, reg wire.Registration) error              // POST .../extmsg/adapters, X-GC-Request
func (c *Client) Inbound(ctx, m wire.InboundMessage) (wire.InboundResponse, error)
func (c *Client) Receipt(ctx, id string) (wire.ReceiptResponse, error)
func (c *Client) Outbound(ctx, req wire.OutboundRequest) (wire.OutboundResult, error)
```
Every call sets `X-GC-Request: gc-gchat-adapter` and `Content-Type: application/json`; a non-2xx answer is an error carrying the status and body. Tests against `fakegc`: register succeeds; register without the header is rejected by the fake; inbound returns the scripted status and receipt id; receipt states; outbound result with and without a transcript entry.

Commit: `git commit -m "gchat: gc api client"`.

---

### Task 10: Inbound: changes to gc turns, wake policy, delivery outcomes

**Files:** `internal/inbound/inbound.go`, `inbound_test.go`

```go
type Processor struct { cfg *config.Config; ledger *ledger.Ledger (with its save func); gc *gcclient.Client; chat *chat.Client; identityUserID string; mediaDir string; log }
func (p *Processor) Handle(ctx, ev *puller.Event) (puller.Outcome, error) // the puller's Handler
func (p *Processor) Poll(ctx)                                             // resolves pending changes by polling receipts; runs every 5s
```
`Handle`: transport dedup (ledger ring; repeat → Ack); for each change: build the change id (spec §4 step 2), skip the identity's own messages and `Mine` reactions-on-others per the policy, apply the wake policy (`dm` always; `all`; `mentions` = annotations contain a USER_MENTION whose user name equals the identity's), render text (mentions → display names; one attachment line per attachment: `[attachment] <name> (<mime>) file:///...` or `[attachment] <name> unavailable: <reason>`), download uploads to `mediaDir/<space>/<message>/<name>` under `MaxBytes`, persist the change as `intent`, POST to gc, then: delivered/no_route → committed; failed/partial → retry up to 3 times with backoff inside the handler, then unresolved; pending → store the receipt id, status pending (Poll resolves it: concluded+delivered → committed; concluded+pending, unknown → unresolved). Return Ack when every change is committed, pending with a persisted receipt (Poll owns its recovery), or unresolved; Nack when a transient error (gc unreachable, or a 5xx or 429 answer from gc) stopped progress; Quarantine when the event cannot be decoded. A 4xx answer from gc is a definite rejection: unresolved with the status in the reason, never the body. A receipt still `pending` one hour after the intent becomes unresolved ("receipt pending beyond deadline"). Edits and deletes (spec §4 step 6) and reactions (own message → turn; else record in the ledger only). Memberships: a space the identity joined → `Spaces[space]` with the default wake. Counters: `turns:<space>` on every committed turn.

Tests against `fakegoogle` + `fakegc` (no pstest needed; construct `puller.Event` values directly): DM delivered; room `all`; room `mentions` with and without a mention; own message skipped; bot sender flagged; edit and delete follow-ups with the right ids; two reactions from two users; update-before-create; attachment downloaded and its line rendered; oversize attachment marked unavailable; every delivery status path; the crash-after-terminal case (simulate by returning before the ledger save and re-running Handle: the second run finds the committed change and does not repost); receipt eviction → unresolved.

Commit: `git commit -m "gchat: inbound processing with wake policy and delivery outcomes"`.

---

### Task 11: Catch-up

**Files:** `internal/catchup/catchup.go`, `catchup_test.go`

```go
type Runner struct { cfg, ledger, chat, proc *inbound.Processor, budget int }
func (r *Runner) Freeze(ctx, now time.Time) error // for every known space: FrozenUpper = now; refresh the space list (new spaces get zero cursors)
func (r *Runner) Pass(ctx) (delivered int, remaining int, err error) // one pass over all spaces, each within the per-space budget (spec section 4 step 10; Config.CatchupBudget, default 50 per space per pass)
```
`Pass` per space: messages from `MessageCursor-1m` to `FrozenUpper` (page by page through the Chat client's single-page `ListMessagesPage` and `ListSpaceEventsPage`, added by this task with frozen bounds, an incoming page token and the returned `NextPageToken`; persist `PageToken` after every page), space events from `EventCursor-1m` for updated/deleted messages and reactions; each record becomes a `puller.Change` fed to `proc.HandleChange` with a `catch-up` marker in the text header; after each page the ledger is saved; a cursor advances only when the interval is fully enumerated and every change in it is committed or unresolved; `remaining` counts backlog beyond the budget. A gap older than 28 days is recorded in `Counters["unrecoverable:<space>"]` and logged.

Tests: an outage with 120 missed messages across two spaces, 110 in one and 10 in the other, drains over three passes with a per-space budget of 50 and the backlog visible after the first two passes; a live message delivered during the pass does not move the cursor; equal create times across a page boundary are not skipped; a restart mid-pass resumes from `PageToken`; a space joined during the outage is included; an old message edited during the outage is recovered via space events; a gap of 40 days is reported unrecoverable.

Commit: `git commit -m "gchat: catch-up with frozen intervals and backlog"`.

---

### Task 12: Outbound server, sends, registrar

**Files:** `internal/server/server.go`, `server_test.go`

HTTP over the Unix socket at `GC_SERVICE_SOCKET` (tests use a temp socket path). Routes and behavior:
- `GET /healthz`: 200 always once bound; header `X-GC-Health: ok|degraded`, body `{"state":"ready|initializing|needs-sign-in|recovery-required|catching-up","reason":"..."}`.
- `POST /publish` (gc's callback): decode `wire.PublishRequest`; key = `IdempotencyKey` or derived (sha256 of session|conversation|reply|text, prefixed `derived-`); if `Sends[key]` has a receipt, return it; else persist the intent (parts via `markdown.Split(markdown.ToChat(text), 32000)`, request ids `<key>-<n>`), send part 1 (named space → with the reply option), persist the thread Chat used, send the rest into it, persist the receipt, return it; on failure return the typed undelivered receipt with HTTP 200; everything within 25 seconds (a context deadline) so gc's 30-second timeout is never hit.
- `POST /commands/publish` body `{session_id, conversation_id, thread, text, idempotency_key}`: bound session → `gc.Outbound`, report `{delivered, recorded, message_id, key}` (recorded = `TranscriptEntry != nil`); unbound (gc answers 422 `no active binding` for the session) → direct send as above; gc cannot record it, because `/extmsg/outbound` refuses a session without an active binding, so report `{delivered, recorded: false, reason: <gc's problem detail>, message_id, key}` and make no second attempt. Only a bound session's direct send (an upload) is recorded through `gc.Outbound` with the same key.
- `POST /commands/upload` body `{conversation_id, thread, path, text, idempotency_key, session_id}`: path must be under `UploadRoot` (realpath, no symlinks) and under `MaxBytes`; persist the intent, `UploadAttachment`, create the message with the attachment and optional text in the thread, persist the receipt, then record via `gc.Outbound` as above.
- `POST /react` `{message, emoji, remove}`; `GET /read?space=&thread=&since=&limit=`; `GET /spaces` and `POST /spaces` `{id, wake}` (writes the settings file); `GET /status` (subscription, cursors, backlog, counters, unresolved count, health); `POST /resolve` `{id, action: accept|replay}`; `POST /state` `{action: restore|reset, file}`.
- Registrar: `Register(ctx)` builds the callback base `GCAPIBase + ServiceURLPrefix` and posts the `wire.Registration` (capabilities as in Task 1, reply instructions as in the spec); retried with backoff until it succeeds; called again whenever a gc call fails with connection refused (gc restarted).

Tests against `fakegc` + `fakegoogle`: `/publish` idempotent on the key; derived key when absent; multipart with a vanished thread yields one new thread; the receipt wire shape byte-for-byte as in Task 1; undelivered receipt with `retry_after` on 429; `/commands/publish` from a bound session makes exactly one Google send and reports recorded; the unbound path sends directly and reports `recorded: false` with gc's reason; an upload from a bound session records with the same key; `/commands/upload` rejects a path outside the root and a symlink; recording failure (null transcript entry) reported as delivered-but-unrecorded and retried; `/resolve` accept and replay; `/healthz` degraded during init.

Commit: `git commit -m "gchat: socket server, publish callback, commands, registrar"`.

---

### Task 13: App wiring and the binary

**Files:** `internal/app/app.go`, `cmd/gc-gchat-adapter/main.go` (complete), `adapter/run.sh`, `adapter/build.sh`

`app.Run(ctx, cfg)`: bind the socket and start the server at once (state `initializing`); load the ledger (recovery-required → state `recovery-required`, serve only `/healthz`, `/status`, `/state`); start `token.Watch`; register with gc (retry); if the token is missing or `NeedsSignIn` → state `needs-sign-in` and wait for the watch; else build clients, `events.Reconcile`, `catchup.Freeze(now)`, start the puller (`pubsub/v2` subscriber with `ReceiveSettings` left at the puller's forced values (`NumGoroutines = 1`, `MaxOutstandingMessages = 1`, since the ledger has no lock and handlers run one at a time; task 8 overrides whatever is set) and `puller.RunOptions{MediaDir, RecordQuarantine}` passed as task 13's bead note says; `PubSubEndpoint` set → `option.WithEndpoint` + `WithoutAuthentication` + insecure gRPC), state `catching-up`, run `catchup.Pass` until `remaining == 0`, state `ready`; `events.RenewLoop` and `inbound.Poll` in goroutines; on `onReload` (new token): rebuild clients and repeat from `Reconcile`. SIGTERM: stop the puller (nack in flight), save the ledger, exit 0.

`main.go` subcommands: `serve` (default; reads `GC_GCHAT_ADAPTER_ENV`, else `$GC_SERVICE_SECRETS_DIR/env`, else `~/.config/gc-gchat-adapter/env`; settings at `<secrets dir>/../config.toml`), `auth` (runs `token.RunAuth` with the same files, opens the browser with `open` on macOS / `xdg-open` elsewhere, prints the URL regardless), `state restore <file>` and `state reset` (operate on the ledger file directly, refuse while the service holds the socket unless `--force`), `version`.

`run.sh`: like wecom's: resolve the env file (fail with a message naming the path and the required keys), `set -a; . env; set +a`, build the binary if missing or older than the sources (`./build.sh`), `exec ./gc-gchat-adapter serve`. `build.sh`: `cd "$(dirname "$0")" && go build -ldflags "-X main.Version=$(git -C . describe --always --dirty 2>/dev/null || echo dev)" -o gc-gchat-adapter ./cmd/gc-gchat-adapter`.

Tests: `app` startup order against the fakes with a 5-second readiness assertion (`/healthz` answers within 100 ms of start even when the fakes delay 10 s); `needs-sign-in` then `auth`-style token file replacement brings it to `ready`; a corrupt ledger yields `recovery-required` with `/status` reachable. `shellcheck adapter/run.sh adapter/build.sh` clean.

Commit: `git commit -m "gchat: app wiring, serve/auth/state subcommands, run and build scripts"`.

---

### Task 14: Pack files, commands, doctor, README

**Files:** `packs/gchat/pack.toml`, `city-fragment.toml`, `commands/*`, `doctor/gchat/*`, `README.md`, `CHANGELOG.md`

`pack.toml`:
```toml
# gchat — Google Chat provider pack for Gas City. One Workspace user identity per city,
# Workspace Events API + Pub/Sub streaming pull inbound, Chat API outbound. No public listener.
[pack]
name = "gchat"
version = "0.1.0"
schema = 2

[[service]]
name = "gchat"
kind = "proxy_process"

[service.publication]
visibility = "private"

[service.process]
command = ["./adapter/run.sh"]
health_path = "/healthz"
```
`city-fragment.toml`: the `[[extmsg.default_route]]` block with provider `gchat`, agent `mayor`, and the same comment wecom carries about pasting it into city.toml.

Each `commands/<verb>/command.toml` has `description = "..."` and `run = "../<verb>.sh"`. Each `.sh` (POSIX, `set -eu`) parses its flags, builds JSON with `printf` and `jq -n --arg` (jq is present on macOS with the status line; document it as a requirement), and calls the adapter through gc's proxy: `curl -sS -X POST -H 'Content-Type: application/json' -H 'X-GC-Request: gc-gchat' --unix-socket` is NOT used; instead the wrapper calls the gc API base (`GC_API_BASE_URL`, default `http://127.0.0.1:8372`) at `/v0/city/$GC_CITY_NAME/svc/gchat/<route>`, the same path gc proxies for slack-full. `reply-current` resolves the conversation the session was last woken from exactly as slack-full's `reply-current.sh` does (copy its lookup; keep it provider-agnostic). `auth.sh` execs `"$GC_PACK_DIR/adapter/gc-gchat-adapter" auth` after resolving the same env file as `run.sh`. `status.sh` prints `/status` as text. `resolve.sh` and `state.sh` wrap `/resolve` and `/state`.

`doctor/gchat/run.sh`: exits non-zero naming the first failing check: secrets file present with every required key; token file present and `gc-gchat-adapter auth --check` (add this flag: refresh once, print the state, exit 0 on ready) succeeds; `/status` reports a subscription state ACTIVE; `gc config show` contains `provider = "gchat"` under `extmsg.default_route`.

`README.md` sections: What it is; Google-side setup (spec §8 verbatim as a checklist, including the Chat API configuration step); Install (import the pack, write the secrets env, paste the route, `gc reload`, `gc gchat auth`, `gc gchat status`); Secrets and settings (the two files from spec §3); Commands (one line each); Operating (health states, unresolved deliveries and `resolve`, recovery-required and `state restore|reset`, catch-up and the 28-day limit); Troubleshooting (the error texts the binary prints). No city, person or credential anywhere.

Tests: `shellcheck commands/*.sh doctor/gchat/run.sh`; `gc lint` on the pack from the city root (`gc lint packs/gchat`, or the closest lint gc offers; record the command used in the commit); a `bash -n` of every script.

Commit: `git commit -m "gchat: pack manifest, commands, doctor check, README"`.

---

### Task 15: End-to-end scenarios

**Files:** `test/e2e/e2e_test.go`

One test binary that starts `pstest`, `fakegoogle`, `fakegc` and the real `app.Run` in-process on a temp socket, with helpers `emitMessage(space, text, sender)`, `expectTurn(t, matchText)`, `publishViaGC(text, thread)`. Implement every named scenario in spec §6 as its own `t.Run` (the list there is the checklist; name each subtest with the scenario's words). Each scenario asserts on `fakegc.Inbound()`, `fakegoogle`'s message store, and the ledger file.

Commit: `git commit -m "gchat: end-to-end scenarios against the fakes"`.

---

### Task 16: Contract test against real gc

**Files:** `test/contract/contract_test.go`

Skipped unless `GC_BIN` is set. With it: `gc init` a temp city under a temp `GC_HOME`, start `GC_BIN supervisor run` on an ephemeral port (pass it the way the gc docs for `gc supervisor run` describe; the worker reads `gc supervisor run --help`), register the adapter with a real `wire.Registration`, POST a real inbound envelope and assert HTTP 200 with a `delivery` object, then call `/extmsg/outbound` and assert the callback reached the adapter's `/publish` with the `X-GC-Request` header and that the receipt decoded (`Receipt.Delivered` true) and the metadata survived as strings; stop the supervisor. If any field name in `wire` turns out wrong here, fix `wire` and its unit test in the same commit.

Commit: `git commit -m "gchat: contract test against a real gc"`.

---

### Task 17: Lint, docs, changelog, final review

- `gofmt -l .` empty; `go vet ./...` clean; `staticcheck ./...` clean (install with `go install honnef.co/go/tools/cmd/staticcheck@latest` into `~/go/bin` if absent); `shellcheck` clean; `go test ./... -count=1` green; `grep -ri "apm\|bonanza\|syndi" packs/gchat` returns nothing.
- `CHANGELOG.md`: `0.1.0: first version, text, threads, DMs, ambient listening, history, files both ways, reactions, catch-up, unresolved deliveries`.
- Commit: `git commit -m "gchat: lint clean, changelog"`.

---

## Self-review (done by the plan's author before dispatch)

**Spec coverage.** §2 architecture → Tasks 7, 8, 10, 12, 13. §3 pack layout → 14; config → 1; state → 2; units 1–8 → 4, 7, 8, 10, 12, 6, 2, 12. §4 inbound 1–7 → 8, 10; outbound 8–10 → 12, 11. §5 → 4, 7, 8, 12, 13, 2. §6 unit → each task's tests; stand-in → 5; end to end → 15; contract → 16; lint → 17; live checklist → not code (the README's last section). §7, §8 → 14 (README) and 5 (the fakes encode the facts). §9 → nothing to build now.

**Placeholders.** Task 1 Step 8 leaves two function bodies to the worker with their behavior spelled out beside them; that is deliberate and bounded. Every other code step is complete or names the exact behavior and test that pins it.

**Type consistency.** `wire.Conversation`, `wire.PublishReceipt`, `ledger.ChangeState.Status` values (`intent|pending|committed|unresolved`), `puller.Outcome` (`Ack|Nack|Quarantine`), `config.WakePolicy` (`all|mentions`), the health states (`initializing|ready|needs-sign-in|recovery-required|catching-up`) are used with the same names in Tasks 10 to 15.

## Execution

Dispatch order: Task 1 alone first (it is the contract). Then Tasks 2, 3, 4, 5, 9 in parallel (they depend only on Task 1). Then 6, 7, 8 in parallel (they need 5). Then 10 (needs 2, 6, 9), 11 (needs 10), 12 (needs 2, 3, 6, 9), then 13, 14, 15, 16, 17. One bead per task, each carrying this plan's path, its task number, the spec path, and the lane discipline text; codex lanes for every task (well-scoped implementation), Opus lanes if a codex lane is unavailable. The mayor merges each lane's branch into the integration branch `gchat-adapter` after its own gate read, and runs the live checklist with a person at the keyboard on the integration branch.
