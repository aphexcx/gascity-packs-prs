// gc-slack-adapter — out-of-process Slack ↔ gc extmsg bridge.
//
// Registers itself with the gc API as an extmsg adapter (provider=slack).
// Two HTTP endpoints:
//
//	POST /publish        — gc forwards outbound publish requests here;
//	                        we translate to Slack chat.postMessage.
//	POST /slack/events   — Slack forwards user events here; we verify the
//	                        signing secret, normalize, and POST to
//	                        gc /v0/city/{city}/extmsg/inbound.
//
// Threading: gc.PublishRequest.ReplyToMessageID is mapped to Slack
// thread_ts. Slack message ts is returned as PublishReceipt.MessageID
// so subsequent replies thread correctly.
//
// All configuration via env vars — keep secrets out of source.
//
// # Environment contract
//
// Must-set (no default; loadConfigFromEnv returns an error if missing):
//
//   - SLACK_WORKSPACE_ID      Slack team id (e.g. T01234567).
//   - SLACK_BOT_TOKEN         xoxb- bot token. Must have chat:write,
//     reactions:write, files:write, and (for
//     identity overrides) chat:write.customize.
//   - GC_CITY_NAME            Name of the gc city the adapter posts to
//     (matches [workspace].name in city.toml). Used
//     to construct /v0/city/{name}/extmsg/inbound and
//     /v0/city/{name}/session/{id}/messages URLs.
//
// Conditionally required (no default; the dependent endpoint is
// disabled when unset):
//
//   - FILE_UPLOAD_ROOT        Absolute filesystem prefix the adapter is
//     allowed to read for /publish-file. Without
//     it, /publish-file returns 503 (defense-in-
//     depth: anyone on the internal mux could
//     otherwise ask the adapter to upload arbitrary
//     host files like /etc/passwd to Slack). Set
//     to the directory tree gc agents write
//     uploadable artifacts under.
//   - SLACK_SIGNING_SECRET    Single-app fallback for HMAC verification on
//     /slack/events and /slack/interactions. The
//     adapter looks up per-app signing secrets in
//     the apps registry first (keyed by team_id);
//     this env var only takes effect when the
//     registry has no record for the inbound
//     team_id. Multi-app deployments should
//     populate the registry via `gc slack
//     import-app`; single-app dev installs can
//     keep using this env var alone. With neither
//     source set, every inbound is rejected 401
//     (correct fail-closed behavior).
//   - SLACK_APP_TOKEN         xapp- app-level token (connections:write).
//     Enables the Socket Mode inbound transport
//     (gp-3og): the adapter dials OUT to Slack over
//     a WebSocket and receives events/interactions
//     with no public Request URL. Unset = HTTP
//     Events API only. Must start with "xapp-".
//
// Optional override (sane default; set to override):
//
//   - SLACK_SOCKET_MODE            Default "auto": run the Socket Mode
//     transport iff SLACK_APP_TOKEN is set.
//     "on" requires the token (startup error
//     without it); "off" never connects even
//     with a token — the rollback lever.
//
//   - SLACK_SOCKET_SELF_RESTART_AFTER  Default "10m". If the Socket Mode
//     transport has connected at least once
//     and then stays dark this long across
//     repeated connect failures, the adapter
//     runs its orderly shutdown (drain →
//     spool → seal, exactly as on SIGTERM)
//     and exits 1 so the service supervisor
//     restarts it — clearing poisoned
//     in-process state (DNS); the bounce is
//     loss-free via the shutdown spool +
//     startup recovery + watermark backfill
//     (gp-bsk). "0" disables the self-restart.
//
//   - SLACK_LIVENESS_STALL_AFTER   Default "10m". With zero inbound events
//     for this long, the liveness watchdog
//     probes watched channels' history for
//     human messages the adapter never
//     received and ALARMS if it finds any
//     (gp-3og; the 2026-08-19 silent-outage
//     detector). "0" disables the watchdog.
//
//   - SLACK_LIVENESS_CHANNELS      Comma-separated channel ids the watchdog
//     always probes. Channels seen in live
//     traffic are learned automatically; pin
//     the critical ones here so a from-boot
//     outage is still detected.
//
//   - SLACK_LIVENESS_ALERT_CHANNEL Channel id that receives a chat.postMessage
//     alarm when a stall is confirmed and a
//     note when a backfill recovers messages.
//     Unset = log-only.
//
//   - SLACK_LIVENESS_STATE_PATH    Default "<GC_CITY_PATH>/.gc/slack/inbound_liveness.json"
//     (or /tmp/gc-slack-adapter/...). Persists
//     per-channel watermarks so a restart
//     backfills the downtime gap. Set-but-empty
//     disables persistence.
//
//   - SLACK_BACKFILL_MAX_WINDOW    Default "1h". How far back a reconnect/
//     restart/watchdog backfill reads channel
//     history when replaying missed messages
//     through the normal pipeline. "0" disables
//     replay (watchdog alarms only).
//
//   - LISTEN_PUBLIC                Default ":8765". Public TCP listener
//     for /slack/events. Bind 0.0.0.0 if
//     fronted by a tunnel (Tailscale Funnel,
//     ngrok, etc.).
//
//   - LISTEN_INTERNAL              Default "127.0.0.1:8766". Loopback
//     listener for /publish and other gc-side
//     endpoints. Ignored when GC_SERVICE_SOCKET
//     is set (proxy_process mode).
//
//   - INTERNAL_CALLBACK_URL        Default "http://127.0.0.1:8766". URL
//     advertised to gc during self-registration.
//     In proxy_process mode this is computed
//     from GC_API_BASE_URL + GC_SERVICE_URL_PREFIX
//     and the env var is ignored.
//
//   - GC_API_BASE_URL              Default "http://127.0.0.1:9443". Base
//     URL for gc's HTTP API.
//
//   - ADAPTER_PROVIDER             Default "slack". Provider name used in
//     conversation refs and adapter registration.
//
//   - REGISTER_ON_START            Default "true". Set "false" to skip
//     /extmsg/adapters self-registration (used
//     by tests + diagnostics).
//
//   - HANDLE_PREFIX                Default "@". Leading address token
//     recognized on inbound messages for
//     keyword routing (e.g. "@name: text").
//     Empty string disables routing.
//
//   - BUSY_REACTION                Default "hourglass". Emoji name (no
//     colons) added to a targeted inbound
//     message when it is dispatched and removed
//     when the agent's reply is published back
//     into the same conversation/thread — the
//     channel-native replacement for Slack
//     Assistant-mode assistant.threads.setStatus
//     (hq-xizo). Set-but-empty (BUSY_REACTION=)
//     disables the lifecycle entirely.
//
//   - IDENTITY_STORE_PATH          Default "/tmp/gc-slack-adapter/identities.json".
//     JSON file backing the per-session
//     chat:write.customize identity registry.
//     Persisted so adapter restarts don't strip
//     identity from running sessions.
//
//   - HANDLE_ALIAS_STORE_PATH      Default "/tmp/gc-slack-adapter/handle-aliases.json".
//     JSON file backing the cross-channel
//     handle → session-id alias registry.
//
//   - INBOUND_FILE_STORE           Default "/tmp/gc-slack-adapter/inbound".
//     Directory for downloaded inbound Slack
//     file attachments. Files are organized as
//     <store>/<channel>/<ts>-<safe-filename>
//     and exposed to gc as file:// URLs.
//
//   - INBOUND_FILE_TTL             Default "168h" (7 days). Maximum age
//     (mtime-based) before the in-process
//     janitor deletes a file. "0" disables the
//     janitor.
//
//   - INBOUND_FILE_SWEEP_INTERVAL  Default "1h". How often the janitor
//     wakes to scan INBOUND_FILE_STORE. "0"
//     disables the janitor.
//
//   - SLACK_CHANNEL_MAPPING_PATH    Default "<GC_CITY_PATH>/.gc/slack/channel_mappings.json"
//     when GC_CITY_PATH is set, otherwise
//     "/tmp/gc-slack-adapter/channel_mappings.json".
//     JSON file written by `gc slack
//     map-channel`. Read-only on the adapter
//     side; loaded at startup and re-read on
//     SIGHUP (gc-cby.23).
//
//   - SLACK_RIG_MAPPING_PATH        Default "<GC_CITY_PATH>/.gc/slack/rig_mappings.json"
//     when GC_CITY_PATH is set, otherwise
//     "/tmp/gc-slack-adapter/rig_mappings.json".
//     JSON file written by `gc slack map-rig`.
//     Read-only on the adapter side; same
//     SIGHUP-or-restart reload contract as
//     SLACK_CHANNEL_MAPPING_PATH. Channel
//     mappings override rig mappings when both
//     claim the same channel.
//
//   - SLACK_APPS_REGISTRY_PATH      Default "<GC_CITY_PATH>/.gc/slack/apps.json"
//     when GC_CITY_PATH is set, otherwise
//     "/tmp/gc-slack-adapter/apps.json". JSON
//     file written by `gc slack import-app`,
//     populated post-OAuth (gc-cby.9). Read-only
//     on the adapter side; same SIGHUP-or-restart
//     reload contract. Used for per-app signing
//     secret lookup keyed by team_id.
//
//   - GC_CITY_PATH                 Optional; consulted only to derive
//     SLACK_CHANNEL_MAPPING_PATH,
//     SLACK_RIG_MAPPING_PATH, and
//     SLACK_APPS_REGISTRY_PATH defaults.
//
// # File permissions
//
// IDENTITY_STORE_PATH, HANDLE_ALIAS_STORE_PATH, and INBOUND_FILE_STORE
// are written with 0o700 directories and 0o600 files so contents
// (session-id ↔ persona mappings, cross-channel handle aliases, and
// downloaded inbound Slack files — potentially DM content) are
// readable only by the adapter's UID. On startup the adapter
// additionally tightens any pre-existing files/directories that are
// looser. Operators using a custom-mode parent (setgid for
// shared-group access, etc.) should set perms before adapter start;
// the tightener preserves setuid/setgid/sticky bits and never
// loosens. The proxy_process Unix domain socket
// (GC_SERVICE_SOCKET) is also chmod'd to 0o600 after bind as
// defense-in-depth on top of its 0o700 controller-managed parent dir.
//
// Controller-injected (proxy_process mode only — set by gc when the
// adapter runs as a [[service]]):
//
//   - GC_SERVICE_SOCKET            Path to the UDS the adapter binds for
//     /publish and /healthz. Presence of this
//     var switches the adapter into
//     proxy_process mode.
//   - GC_SERVICE_URL_PREFIX        Required when GC_SERVICE_SOCKET is set
//     (e.g. "/svc/slack"). The adapter's
//     self-registered CallbackURL is computed
//     as GC_API_BASE_URL + GC_SERVICE_URL_PREFIX.
//
// Consumer-specific (referenced by deployment scripts and prompts but
// NOT consumed by the adapter binary):
//
//   - any environment used by sibling tooling (deliver-rollup.sh,
//     resolve_rig_channel.py, etc.) lives outside this binary and is
//     documented in the consumer pack's README.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

const (
	// Public listener: serves /slack/events only. Bind to 0.0.0.0 so
	// Tailscale Funnel can reach it.
	defaultPublicListen = ":8765"
	// Internal listener: serves /publish only. Bound to 127.0.0.1 so
	// only processes on this machine (i.e. gc) can reach it.
	defaultInternalListen   = "127.0.0.1:8766"
	defaultInternalCallback = "http://127.0.0.1:8766"
)

// slackAPIBase is a var (not const) so tests can replace it with a fake.
var slackAPIBase = "https://slack.com/api"

// dispatchInflightWG counts in-flight dispatch goroutines that own a
// dispatch-slot release(). Every `go func()` that defers release()
// MUST also Add(1) before the spawn and `defer
// dispatchInflightWG.Done()` inside. The current set of spawn sites:
//
//   - interactions.go: slash → session, block_actions → session,
//     view_submission → session
//   - rig_dispatch.go: block_actions → rig, view_submission → rig
//   - main.go: events-path alias → session (handleSlackEvents)
//
// Tests use it as a barrier: signedSlackInteractionRequest registers
// dispatchInflightWG.Wait via t.Cleanup so any goroutine spawned by
// the test fully drains before the test framework moves on. Without
// the barrier, a leftover goroutine writing to log.Default() races
// the next test's log.SetOutput (gc-cby.36).
//
// Complementary to dispatchTestCompletionHook (rig_dispatch.go): the
// hook is a per-test signal that fires once at goroutine exit and is
// used by tests that assert on dispatch completion side effects;
// the WG is a universal drain barrier covering ALL dispatch sites.
// Folding the hook into the WG is left as a future cleanup.
var dispatchInflightWG sync.WaitGroup

// acquireDispatchSlot tries to acquire one slot on cfg.dispatchSem
// without blocking. On success it returns a release func bound to
// the channel observed at acquire time, plus the channel's capacity
// (handy for log messages); on failure it returns nil and the
// observed cap. The caller must defer release() at goroutine entry.
//
// Capturing the channel reference at acquire time keeps the goroutine
// race-clean even if a future call site builds a fresh cfg between
// acquire and release. A nil cfg.dispatchSem makes the channel send
// case block forever, falling through to default and reporting "not
// acquired"; main() initializes the field before any handler is wired
// in, so production reaches this only if the operator wires a handler
// from a test-style cfg without a sem (in which case the dropped-load
// log line is the intended fail-safe behavior). sec-S-04.
func (c config) acquireDispatchSlot() (release func(), capacity int, ok bool) {
	release, capacity, ok = c.tryAcquireDispatchSlot()
	if !ok {
		dispatchDroppedTotal.Add(1)
	}
	return release, capacity, ok
}

// tryAcquireDispatchSlot attempts to take a dispatch slot WITHOUT counting
// a failed acquire as a drop. The legacy inbound path wraps this via
// acquireDispatchSlot (a miss there is a genuine dropped delivery). The
// company path uses this directly: a company receipt that finds no slot
// stays durably pending for the sweep — backpressure, not a drop — so
// counting it would pollute dispatch_dropped_total and mask real legacy
// loss (F10).
func (c config) tryAcquireDispatchSlot() (release func(), capacity int, ok bool) {
	sem := c.dispatchSem
	semCap := cap(sem)
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, semCap, true
	default:
		return nil, semCap, false
	}
}

type config struct {
	publicListen        string
	internalListen      string // unused when serviceSocket is set
	serviceSocket       string // when set, bind a UDS here for /publish instead of internalListen
	internalCallbackURL string
	gcAPIBase           string
	cityName            string
	provider            string
	accountID           string
	slackBotToken       string
	slackSigningKey     string
	// slackAppID is the switchboard app's own Slack api_app_id (SLACK_APP_ID).
	// When set, an event_callback whose api_app_id equals it verifies against
	// the env signing secret ONLY (Phase 4 verification rule 2, the rooms path)
	// — never the legacy trial set and never a registered agent record that
	// happens to share the id. Empty preserves the pre-Phase-4 behavior (the
	// switchboard's events fall through to the legacy lookupSigningSecrets path).
	slackAppID      string
	registerOnStart bool
	// identityStorePath is the JSON file backing the per-session Slack
	// identity registry (chat:write.customize username/avatar overrides).
	// Persisted so adapter restarts don't strip identity from running
	// sessions.
	identityStorePath string
	// handlePrefix is the leading address token recognized on inbound
	// messages (e.g. "@"). When a message starts with
	// `<prefix><handle>:`, the handle is extracted into ExplicitTarget
	// and the prefix is stripped from the forwarded text. Empty disables
	// keyword routing.
	handlePrefix string
	// handleAliasStorePath is the JSON file backing the handle-alias
	// registry. Maps handle -> gc session id; used to dispatch
	// cross-channel address-by-handle messages (e.g. `@ops:` from any
	// channel routes to the session registered under the "ops" handle
	// even when that session has no Slack binding for the channel).
	handleAliasStorePath string
	// threadSessionsStorePath is the JSON file backing the thread →
	// session registry used by Slack launcher mode (cby.5). When a
	// `@@<handle>` post arrives in a thread, the adapter checks this
	// registry to converge subsequent posts in the same thread on a
	// single agent. Sourced from GC_SLACK_THREAD_SESSIONS_FILE,
	// defaulting to <GC_CITY_PATH>/.gc/slack/thread_sessions.json when
	// GC_CITY_PATH is set, else /tmp/gc-slack-adapter/thread_sessions.json.
	threadSessionsStorePath string
	// roomLaunchPath is the JSON file backing the room-launch mapping
	// registry used by Slack launcher mode (cby.5.3). Maps
	// (workspace_id, channel_id) → pool_template; written by
	// `gc slack enable-room-launch`. Sourced from
	// GC_SLACK_ROOM_LAUNCH_FILE, defaulting to
	// <GC_CITY_PATH>/.gc/slack/room_launch_mappings.json when
	// GC_CITY_PATH is set, else
	// /tmp/gc-slack-adapter/room_launch_mappings.json.
	roomLaunchPath string
	// inboundFileStore is the local directory where inbound Slack file
	// attachments are written so bound sessions can read them directly
	// (no bot-token leak). Files are organized as
	// <store>/<channel>/<ts>-<safe-filename>.
	inboundFileStore string
	// inboundDeadLetterDir receives coalesced inbounds gc rejected as
	// payload (400/413/415/422) maxCoalesceDeliveryAttempts times — one
	// JSONL file per channel, see writeInboundDeadLetter (gp-xnc).
	// SLACK_INBOUND_DEAD_LETTER_DIR; default
	// <GC_CITY_PATH>/.gc/slack/inbound_dead_letter.
	inboundDeadLetterDir string
	// inboundFileTTL is the maximum age (mtime-based) of files in
	// inboundFileStore before the in-process janitor deletes them.
	// Empty or zero disables the janitor entirely.
	inboundFileTTL time.Duration
	// inboundFileSweepInterval is how often the janitor wakes up to
	// scan inboundFileStore. Empty or zero disables the janitor.
	inboundFileSweepInterval time.Duration
	// channelMappingPath is the JSON file written by
	// `gc slack map-channel` mapping (workspace_id, channel_id) →
	// (rig|session, target_id). Read-only on this side; the adapter
	// loads it at startup and re-reads on SIGHUP (gc-cby.23). Sourced
	// from SLACK_CHANNEL_MAPPING_PATH, defaulting to
	// <GC_CITY_PATH>/.gc/slack/channel_mappings.json when GC_CITY_PATH
	// is set, else /tmp/gc-slack-adapter/channel_mappings.json.
	channelMappingPath string
	// rigMappingPath is the JSON file written by `gc slack map-rig`
	// mapping (workspace_id, rig_name) → set-of-channel-ids. Read-only
	// on this side; same SIGHUP-or-restart reload contract as
	// channelMappingPath. Per-channel `map-channel` bindings take
	// precedence over rig defaults — see resolveChannelTarget. Sourced
	// from SLACK_RIG_MAPPING_PATH, defaulting to
	// <GC_CITY_PATH>/.gc/slack/rig_mappings.json when GC_CITY_PATH is
	// set, else /tmp/gc-slack-adapter/rig_mappings.json.
	rigMappingPath string
	// subteamAliasStorePath is the JSON file mapping Slack User Group
	// ("subteam") IDs (e.g. "S0123ABCD") to gc handles. Read-only on
	// this side; same SIGHUP-or-restart reload contract as
	// channelMappingPath. The operator edits the file directly or via a
	// future `gc slack subteam-alias` command. The map is the ONLY gate
	// for the UNLABELED subteam mention shape `<!subteam^Sxxx>` Slack
	// emits in event payloads — without an entry the inbound falls
	// through to channel fanout. The LABELED shape
	// `<!subteam^Sxxx|@handle>` remains gated by handleAliasRegistry
	// against the `@handle` label (bead gpk-2zi). Sourced from
	// SLACK_SUBTEAM_ALIAS_FILE, defaulting to
	// <GC_CITY_PATH>/.gc/slack/subteam-aliases.json when GC_CITY_PATH
	// is set, else /tmp/gc-slack-adapter/subteam-aliases.json. Bead
	// gpk-hmr.2.
	subteamAliasStorePath string
	// userAliasStorePath is the JSON file mapping a bare gc handle
	// (e.g. "mayor") to a raw Slack target ID — a user (Uxxxx/Wxxxx) or
	// a User Group (Sxxxx). handlePublish uses it to rewrite outbound
	// `@handle` body tokens into Slack mention syntax so they render as
	// clickable, notifying mentions instead of literal text (gpk-uha7).
	// This is the outbound inverse of subteamAliasStorePath and follows
	// the identical read-only, SIGHUP-or-restart reload contract: the
	// operator edits the file directly (or via a future `gc slack
	// user-alias` command). A handle absent from the map is left
	// literal — fail-safe, no surprise pings. Sourced from
	// SLACK_USER_ALIAS_FILE, defaulting to
	// <GC_CITY_PATH>/.gc/slack/slack-user-aliases.json when GC_CITY_PATH
	// is set, else /tmp/gc-slack-adapter/slack-user-aliases.json.
	userAliasStorePath string
	// fileUploadRoot is the absolute filesystem prefix
	// /publish-file is allowed to read. Empty disables /publish-file
	// entirely (fail-closed). gc and the adapter share a filesystem,
	// so the trust boundary is the gc controller process — but the
	// internal mux is reachable by anything on the loopback (or, in
	// proxy_process mode, by anything that can connect to the UDS),
	// so confinement here is defense-in-depth: a compromised internal
	// caller cannot ask the adapter to upload arbitrary files (e.g.
	// /etc/passwd) on its behalf. Sourced from FILE_UPLOAD_ROOT.
	fileUploadRoot string
	// dispatchConcurrency caps the number of in-flight inbound
	// dispatch goroutines (slash-command → session, slack-event →
	// session, alias-resolved → session). A burst of inbound traffic
	// otherwise spawns one goroutine per request, each holding an
	// http.Client with a 10s timeout — memory and FD pressure scale
	// linearly with traffic. Sourced from SLACK_DISPATCH_CONCURRENCY,
	// default 50. Must be a positive integer; loadConfig rejects 0,
	// negative, and non-numeric values rather than silently disabling
	// dispatch. sec-S-04.
	dispatchConcurrency int
	// dispatchSem caps the number of concurrent inbound dispatch
	// goroutines. main() initializes this to a buffered channel of
	// size dispatchConcurrency before any handler is wired in;
	// acquireDispatchSlot reads it through the cfg value. Tests build a
	// cfg with their own scoped channel rather than sharing a
	// package-level singleton, so saturation tests can run in parallel
	// without interfering with other tests' slot counts. gc-px8.7
	// (was gc-cby.30).
	dispatchSem chan struct{}
	// appsRegistryPath is the JSON file written by `gc slack import-app`
	// mapping (workspace_id, app_id) → app record (incl. signing_secret
	// populated post-OAuth). Read-only on this side; same SIGHUP-or-
	// restart reload contract as channelMappingPath. Sourced from
	// SLACK_APPS_REGISTRY_PATH, defaulting to
	// <GC_CITY_PATH>/.gc/slack/apps.json when GC_CITY_PATH is set, else
	// /tmp/gc-slack-adapter/apps.json. Used to resolve per-app signing
	// secrets for /slack/events and /slack/interactions request
	// verification.
	appsRegistryPath string
	// appsRegistry is the in-memory snapshot of appsRegistryPath, wired
	// at startup. Nil-safe — when nil, lookupSigningSecrets falls
	// through to slackSigningKey for single-app dev installs.
	appsRegistry *appsRegistry
	// oauthClientID, oauthClientSecret, oauthRedirectURI configure the
	// OAuth install flow (gc-cby.9). When oauthClientID is empty the
	// /slack/oauth/{start,callback} handlers are not registered and
	// install relies on the manual web-UI flow documented in
	// adapter/SETUP.md. When set, the adapter registers the two
	// handlers on the public mux; an operator visits /slack/oauth/start
	// to grant the app to a workspace, and the callback persists the
	// resulting bot_token + workspace_id + app_id into the apps
	// registry and writes <cityPath>/.gc/slack/install.env so the
	// operator can re-source and restart the adapter.
	oauthClientID     string
	oauthClientSecret string
	oauthRedirectURI  string
	// oauthSlackBaseURL overrides the Slack base URL used by the OAuth
	// flow (default https://slack.com). Tests inject an httptest.Server
	// URL via this field; production deployments leave it empty.
	oauthSlackBaseURL string
	// cityPath is the on-disk root of the gc city this adapter is bound
	// to. Sourced from GC_CITY_PATH; required for the rig-target
	// dispatch path (cby.18.3) which must shell `gc bd create` inside the
	// rig's workdir (read from <cityPath>/.beads/routes.jsonl) and
	// `gc sling` from the city root. Empty when GC_CITY_PATH is unset;
	// the rig dispatch path surfaces a fix-it ephemeral in that case.
	cityPath string
	// threadContextCache is the process-singleton cache that
	// short-circuits repeated thread-context fetches for a given
	// (channel, thread_ts). Nil-safe: when nil, processSlackEvent
	// skips the preamble path entirely. Initialized in main(); tests
	// construct one directly. gc-px8.5.
	threadContextCache *threadContextCache
	// slackThreadContextLimit caps how many replies the adapter asks
	// for when seeding thread context. Sourced from
	// SLACK_THREAD_CONTEXT_LIMIT, defaulting to
	// defaultThreadContextLimit. gc-px8.5.
	slackThreadContextLimit int
	// companyDirectoryPath / companyBindingsPath / companyIngressDir are
	// the Slack company-rooms (Phase 1) registry locations. The two JSON
	// registries resolve exactly like the six atomic registries
	// (env override > <GC_CITY_PATH>/.gc/slack/<file> >
	// /tmp/gc-slack-adapter/<file>); the ingress dir mirrors the
	// thread_sessions.json resolution with a chat-ingress/ leaf. The
	// Python CLI (scripts/slack_company_directory.py) resolves the same
	// files. Sourced from SLACK_COMPANY_DIRECTORY_PATH,
	// SLACK_COMPANY_BINDINGS_PATH, SLACK_COMPANY_INGRESS_DIR.
	companyDirectoryPath string
	companyBindingsPath  string
	companyIngressDir    string
	// companyDMBindingsPath / companyAgentAppsPath are the Phase 4 per-agent
	// DM registries (dm_bindings.json, agent_apps.json), resolved exactly
	// like the two Phase 1 registries above (env override >
	// <GC_CITY_PATH>/.gc/slack/<file> > /tmp default). Sourced from
	// SLACK_COMPANY_DM_BINDINGS_PATH and SLACK_COMPANY_AGENT_APPS_PATH.
	companyDMBindingsPath string
	companyAgentAppsPath  string
	// companyVerifySessions gates the advisory session-existence guard
	// (Phase 4): when set, delivery checks GET /v0/city/{city}/session/{id}
	// before the first attempt per (city, session). Advisory only — a guard
	// error or a 404/409 never terminalizes; it just leaves the target
	// pending for the sweep. Sourced from SLACK_COMPANY_VERIFY_SESSIONS.
	companyVerifySessions bool
	// Phase 2 shared-state directories (secrets/intents/delegations/turns/
	// locks). Resolved exactly like the Python side: env override >
	// <GC_CITY_PATH>/.gc/slack/<leaf> > /tmp/gc-slack-adapter/<leaf>. The Go
	// ingress path reads intents (correlation + stale count), reads/writes
	// delegation records (result claims), writes current-turn pointers, and
	// takes advisory locks; the secrets dir is Python-only but resolved here
	// for parity / config visibility.
	companySecretsDir     string
	companyIntentsDir     string
	companyDelegationsDir string
	companyTurnsDir       string
	companyLocksDir       string
	// companyCityAPIs maps a city-qualified binding's city name to that
	// city's supervisor API base URL (each city runs its own supervisor on
	// this host). Parsed from SLACK_COMPANY_CITY_APIS as
	// "city=http://127.0.0.1:8377,other=http://127.0.0.1:8374". The
	// adapter's own city never needs an entry.
	companyCityAPIs map[string]string

	// companySelfBotUserID is the switchboard app's own bot user id,
	// excluded from wake routing so the switchboard never wakes itself.
	// Optional (empty OK in Phase 1). Sourced from
	// SLACK_SWITCHBOARD_BOT_USER_ID.
	companySelfBotUserID string
	// companyVisibleAcks gates the config-driven visible-ack reactions
	// (Phase 3b). Off by default: unset/empty/"0" = off, anything else = on.
	// Sourced from SLACK_COMPANY_VISIBLE_ACKS.
	companyVisibleAcks bool
	// companyGateway owns the durable-admission + delivery path for
	// imported company rooms. Nil disables the company path entirely —
	// every inbound then flows through the legacy path byte-for-byte.
	// Wired in main() before any handler closes over the cfg value.
	companyGateway *companyGateway
	// busyReaction is the emoji name (no colons) added to a targeted
	// inbound message when it is dispatched and removed when the
	// agent's reply is published back into the same conversation/
	// thread — the channel-native busy affordance replacing Slack
	// Assistant-mode assistant.threads.setStatus (hq-xizo). Sourced
	// from BUSY_REACTION, defaulting to busyReactionDefault
	// ("hourglass"); the set-but-empty form (BUSY_REACTION=) yields ""
	// here and disables the lifecycle entirely — no reaction is added
	// and no mark is recorded. Replaces the previous unconditional
	// "eyes" reaction on targeted inbounds.
	busyReaction string
	// busyMarks is the in-memory registry of pending busy reactions,
	// keyed by (conversation id, thread key). processSlackEvent
	// records a mark when it adds the busy reaction; handlePublish
	// consumes it (and removes the reaction) when the reply lands.
	// Nil-safe: mark/take on a nil registry are no-ops reporting no
	// pending mark. Initialized in main(). hq-xizo.
	busyMarks *busyReactionRegistry
	// eventDedup is the short-TTL seen-set over Slack event_id that
	// drops Events API redeliveries before they re-forward to gc.
	// Slack retries any event delivery it considers unacknowledged
	// (up to 3 times, minutes apart) and each retry carries the same
	// event_id, so a hiccup on the first delivery otherwise turns
	// into a duplicate session notification (hw-94w5k finding #4).
	// Nil-safe: a nil cache never dedupes. Initialized in main().
	eventDedup *eventDedupCache
	// channelClaims serializes the channel-audience delivery per
	// (channel, ts) so a bot-mention twin pair (message + app_mention,
	// same ts, distinct event_ids) cannot deliver the same message id
	// twice into the bound session's turn — the event_id dedup cannot
	// collapse the pair and gc's member notification has no dedup of
	// its own (gp-ios, pc_c920ff5fe90c). Reuses the eventDedupCache
	// begin/commit/forget lifecycle keyed by channelDeliveryClaimKey;
	// see channel_claims.go. Nil-safe: a nil cache never claims, so
	// directly-constructed test configs keep the historical behavior.
	channelClaims *eventDedupCache
	// userNames caches users.info display-name lookups backing inbound
	// sender resolution, inline `<@U…>` mention rewriting, and
	// thread-context preamble author lines (hq-uxln9). nil disables
	// resolution — raw ids pass through, keeping directly-constructed
	// test configs network-inert.
	userNames *userNameCache
	// userAliases is the operator-curated slack-user-aliases.json view
	// (outbound handle -> mention). Inbound resolution consults its
	// inverse first (handleForUserID) so a curated identity renders as
	// its gc handle without any Slack call (hq-uxln9). Shares the
	// instance wired for handlePublish, so SIGHUP reloads propagate.
	// nil-safe: nil skips the curated leg.
	userAliases *userAliasMap

	// slackAppToken is the app-level token (xapp-…, scope
	// connections:write) that opens the Socket Mode WebSocket
	// (SLACK_APP_TOKEN). Empty disables the socket transport; the
	// Events API listener stays up either way (gp-3og).
	slackAppToken string
	// socketMode is the parsed SLACK_SOCKET_MODE policy: "auto" (default —
	// run the socket transport when SLACK_APP_TOKEN is set), "on" (require
	// the token; fail startup without it), "off" (never connect even with
	// a token present — the operator's rollback lever).
	socketMode string
	// socketSelfRestartAfter is how long the Socket Mode transport may
	// stay continuously dark — after having connected at least once —
	// before the adapter exits so the service supervisor restarts the
	// whole process, clearing any poisoned in-process state
	// (SLACK_SOCKET_SELF_RESTART_AFTER, default 10m; 0 disables; gp-bsk).
	socketSelfRestartAfter time.Duration
	// inboundLiveness is the process-wide inbound-liveness tracker +
	// watchdog (gp-3og). Nil-safe consumer paths; only the production
	// main() wires it.
	inboundLiveness *inboundLiveness
	// livenessStallAfter is how long the adapter tolerates zero inbound
	// events before the watchdog probes channel history for messages it
	// should have seen (SLACK_LIVENESS_STALL_AFTER, default 10m; 0
	// disables the watchdog).
	livenessStallAfter time.Duration
	// livenessChannels is the operator-pinned watched-channel list
	// (SLACK_LIVENESS_CHANNELS, comma-separated channel ids). The
	// watchdog also learns channels from live inbound traffic.
	livenessChannels []string
	// livenessAlertChannel, when set, receives a chat.postMessage alarm
	// the moment the watchdog confirms missed inbound (SLACK_LIVENESS_ALERT_CHANNEL).
	livenessAlertChannel string
	// livenessStatePath persists last-inbound + per-channel watermarks so
	// a restart can backfill the gap (SLACK_LIVENESS_STATE_PATH; defaults
	// beside the other city-rooted registries; empty string disables).
	livenessStatePath string
	// backfillMaxWindow caps how far back a reconnect/restart/watchdog
	// backfill reads channel history (SLACK_BACKFILL_MAX_WINDOW, default
	// 1h; 0 disables backfill delivery — the watchdog then only alarms).
	backfillMaxWindow time.Duration
	// peerBotsPath is the JSON file naming the allowlisted fleet apps whose
	// posts are delivered to bound sessions as tagged read-only peer
	// context (gp-kop), plus the channels configured for immediate (waking)
	// delivery. Operator-edited off-band; same SIGHUP-or-restart reload
	// contract as channelMappingPath. Sourced from SLACK_PEER_BOTS_PATH,
	// defaulting to <GC_CITY_PATH>/.gc/slack/peer_bots.json when
	// GC_CITY_PATH is set, else /tmp/gc-slack-adapter/peer_bots.json.
	peerBotsPath string
	// peerBots is the in-memory snapshot of peerBotsPath. Nil-safe: nil (or
	// an empty allowlist) keeps the legacy drop-every-bot-message behavior.
	peerBots *peerBotsRegistry
	// peerContext buffers allowlisted peer posts per channel until the next
	// inbound naturally forwarded for that channel carries them as a
	// prepended context block (the no-wake default). Nil-safe.
	peerContext *peerContextBuffer
	// peerAuthors resolves a peer post's bot_id through bots.info — the
	// company gateway's author-resolution scaffolding reused for the
	// legacy-path self-guard (a bot must never see its own posts). Nil
	// disables peer delivery entirely (fail closed).
	peerAuthors companyAuthorResolver

	// --- token-efficiency pass (gp-729) ---
	//
	// coalesceWindow is the burst-debounce for untargeted channel
	// inbounds (SLACK_COALESCE_WINDOW, default 8s; 0 restores
	// per-message immediate forwarding). coalescer owns the buffers
	// and timers; nil-safe — nil disables coalescing entirely, which
	// keeps directly-constructed test configs on the immediate path.
	coalesceWindow time.Duration
	coalescer      *inboundCoalescer
	// deliveryPolicyPath / deliveryPolicy back delivery_policy.json —
	// the per-channel immediate-vs-digest knob (item 6). Operator-
	// edited off-band; same SIGHUP-or-restart reload contract as
	// peer_bots.json. Sourced from SLACK_DELIVERY_POLICY_PATH,
	// defaulting to <GC_CITY_PATH>/.gc/slack/delivery_policy.json when
	// GC_CITY_PATH is set, else /tmp/gc-slack-adapter/delivery_policy.json.
	deliveryPolicyPath string
	deliveryPolicy     *deliveryPolicyRegistry
	// deliveredIDs tracks which message timestamps each audience has
	// already received so thread-context preambles stop re-quoting
	// them (item 2). Nil-safe: nil means never-seen → full quote.
	deliveredIDs *deliveredIDs
	// channelNames caches conversations.info name lookups backing the
	// "#name (Cid)" rendering in pack-owned wrapper blocks (item 4).
	// nil disables resolution — raw ids pass through, keeping
	// directly-constructed test configs network-inert.
	channelNames *channelNameCache
	// replyHelp remembers which channels received the full reply
	// how-to this adapter lifetime (item 3 — the per-message reminder
	// carries only the registered one-line template). Nil-safe.
	replyHelp *oncePerChannel
	// reminderTextBudget bounds one channel delivery's Text field
	// (gp-0qw + gp-9gc): boilerplate attaches only inside the budget,
	// and a body that alone overflows it is tail-trimmed behind a
	// protected head — see reminder_budget.go for the full contract.
	// SLACK_REMINDER_TEXT_BUDGET overrides; 0 disables (the value
	// directly-constructed test configs get, keeping the legacy
	// layout byte-for-byte).
	reminderTextBudget int
	// deliveryReceiptGate arms the same-ts claim's delivery-receipt gate
	// (gp-32q): a claim concludes only on a receipt that vouches the
	// complete payload reached the session. SLACK_DELIVERY_RECEIPT_GATE
	// =off disarms it back to the pre-gp-32q commit-on-2xx behavior.
	// Directly-constructed test configs get false, which keeps their
	// legacy expectations exact; a gc that emits no receipt is gated to
	// the same legacy verdict anyway (see delivery_receipt.go).
	deliveryReceiptGate bool
	// bindingCheck caches gc binding lookups backing alias-dispatch
	// turn-dedup (item 5). Nil-safe: nil preserves the historical
	// double-dispatch behavior.
	bindingCheck *bindingCheckCache
	// eventWG tracks the detached per-event processing goroutines
	// spawned by handleSlackEvents so shutdown can await them BEFORE
	// draining the coalescer: an event still in flight when flushAll
	// runs could admit a message or reaction to the buffers after the
	// drain, losing it permanently on exit (gp-9e7 fix round 1a).
	// Nil-safe: directly-constructed test configs leave it nil and
	// event goroutines run untracked.
	eventWG *sync.WaitGroup
	// draining is the shutdown admission barrier (gp-9e7 fix round
	// 2b'): main() flips it as shutdown's FIRST act, and
	// handleSlackEvents then refuses every envelope with 503 BEFORE the
	// event is acked to Slack OR the liveness watermark advances — the
	// Events API retry ladder and the un-acked Socket Mode envelope
	// keep the event server-side, and whatever the ladder gives up on
	// stays above the watermark for the startup backfill. Nil-safe:
	// directly-constructed test configs leave it nil (never draining).
	draining *atomic.Bool
	// coalesceSpoolPath / inboundSpool back the durable shutdown spool
	// (gp-9e7 fix round 2a'): coalescer batches that remain
	// undeliverable at flushAll's retry bound — already acked to Slack,
	// already below the admission-time watermark, hence invisible to
	// both Slack redelivery and the startup backfill — are spooled here
	// and re-buffered at the next startup. SLACK_COALESCE_SPOOL_PATH;
	// defaults beside the other city-rooted state; explicitly empty
	// disables spooling (residue is then LOST, logged per channel).
	coalesceSpoolPath string
	inboundSpool      *inboundSpool
}

func loadConfig() (config, error) {
	return loadConfigFromLookup(os.LookupEnv)
}

// companyStateDirDefault resolves a Phase 2 shared-state directory default:
// <GC_CITY_PATH>/.gc/slack/<leaf> when the city path is set, else
// /tmp/gc-slack-adapter/<leaf>. The env override is applied by the caller.
// This mirrors the Python company outbound module's path resolution leaf for
// leaf.
func companyStateDirDefault(cityPath, leaf string) string {
	if cityPath != "" {
		return filepath.Join(cityPath, ".gc", "slack", leaf)
	}
	return filepath.Join("/tmp/gc-slack-adapter", leaf)
}

// loadConfigFromEnv reads adapter configuration from a plain getenv
// function (os.Getenv shape: missing keys read as ""). It cannot
// distinguish set-but-empty from unset, so vars whose empty form is
// meaningful (BUSY_REACTION=) behave as unset through this entry
// point; callers that need presence information use
// loadConfigFromLookup directly.
func loadConfigFromEnv(getenv func(string) string) (config, error) {
	return loadConfigFromLookup(func(key string) (string, bool) {
		v := getenv(key)
		return v, v != ""
	})
}

// loadConfigFromLookup reads adapter configuration from a lookup function
// (os.LookupEnv shape). When $GC_SERVICE_SOCKET is set, the adapter switches
// to proxy_process mode: it binds a Unix domain socket for /publish (and
// /healthz) instead of an internal TCP listener, and registers the callback
// URL gc routes through its /svc/{name} mount. This keeps a single binary
// serving both the legacy nohup-managed deployment and the proxy_process
// deployment.
func loadConfigFromLookup(lookup func(string) (string, bool)) (config, error) {
	getenv := func(key string) string {
		v, _ := lookup(key)
		return v
	}
	envOrFn := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	cfg := config{
		publicListen:         envOrFn("LISTEN_PUBLIC", defaultPublicListen),
		internalListen:       envOrFn("LISTEN_INTERNAL", defaultInternalListen),
		serviceSocket:        getenv("GC_SERVICE_SOCKET"),
		internalCallbackURL:  strings.TrimRight(envOrFn("INTERNAL_CALLBACK_URL", defaultInternalCallback), "/"),
		gcAPIBase:            strings.TrimRight(envOrFn("GC_API_BASE_URL", "http://127.0.0.1:9443"), "/"),
		cityName:             getenv("GC_CITY_NAME"),
		provider:             envOrFn("ADAPTER_PROVIDER", "slack"),
		accountID:            getenv("SLACK_WORKSPACE_ID"),
		slackBotToken:        getenv("SLACK_BOT_TOKEN"),
		slackSigningKey:      getenv("SLACK_SIGNING_SECRET"),
		slackAppID:           getenv("SLACK_APP_ID"),
		registerOnStart:      envOrFn("REGISTER_ON_START", "true") == "true",
		identityStorePath:    envOrFn("IDENTITY_STORE_PATH", "/tmp/gc-slack-adapter/identities.json"),
		handlePrefix:         envOrFn("HANDLE_PREFIX", "@"),
		handleAliasStorePath: envOrFn("HANDLE_ALIAS_STORE_PATH", "/tmp/gc-slack-adapter/handle-aliases.json"),
		inboundFileStore:     envOrFn("INBOUND_FILE_STORE", "/tmp/gc-slack-adapter/inbound"),
		fileUploadRoot:       getenv("FILE_UPLOAD_ROOT"),
	}

	// BUSY_REACTION: emoji added to a targeted inbound while the agent
	// works, removed when the reply publishes back (hq-xizo). Read
	// through lookup (not envOrFn) because the set-but-empty form
	// (BUSY_REACTION=) means "disable the lifecycle" and must not fall
	// back to the default. Surrounding colons are stripped so operators
	// can write ":hourglass:" or "hourglass" interchangeably, matching
	// /react's emoji handling.
	if v, ok := lookup("BUSY_REACTION"); ok {
		cfg.busyReaction = strings.Trim(v, ":")
	} else {
		cfg.busyReaction = busyReactionDefault
	}

	// channelMappingPath default: prefer the city-rooted path when
	// GC_CITY_PATH is set so a single-host slack-pack deployment
	// "just works" without operator config; fall back to the legacy
	// /tmp/gc-slack-adapter/ tree otherwise. Operators can override
	// explicitly with SLACK_CHANNEL_MAPPING_PATH. Apps registry path
	// follows the same convention.
	defaultMappingPath := "/tmp/gc-slack-adapter/channel_mappings.json"
	defaultRigMappingPath := "/tmp/gc-slack-adapter/rig_mappings.json"
	defaultAppsRegistryPath := "/tmp/gc-slack-adapter/apps.json"
	defaultThreadSessionsPath := "/tmp/gc-slack-adapter/thread_sessions.json"
	defaultRoomLaunchPath := "/tmp/gc-slack-adapter/room_launch_mappings.json"
	defaultSubteamAliasPath := "/tmp/gc-slack-adapter/subteam-aliases.json"
	defaultUserAliasPath := "/tmp/gc-slack-adapter/slack-user-aliases.json"
	defaultPeerBotsPath := "/tmp/gc-slack-adapter/peer_bots.json"
	if cityPath := getenv("GC_CITY_PATH"); cityPath != "" {
		defaultMappingPath = filepath.Join(cityPath, ".gc", "slack", "channel_mappings.json")
		defaultRigMappingPath = filepath.Join(cityPath, ".gc", "slack", "rig_mappings.json")
		defaultAppsRegistryPath = filepath.Join(cityPath, ".gc", "slack", "apps.json")
		defaultThreadSessionsPath = filepath.Join(cityPath, ".gc", "slack", "thread_sessions.json")
		defaultRoomLaunchPath = filepath.Join(cityPath, ".gc", "slack", "room_launch_mappings.json")
		defaultSubteamAliasPath = filepath.Join(cityPath, ".gc", "slack", "subteam-aliases.json")
		defaultUserAliasPath = filepath.Join(cityPath, ".gc", "slack", "slack-user-aliases.json")
		defaultPeerBotsPath = filepath.Join(cityPath, ".gc", "slack", "peer_bots.json")
		cfg.cityPath = cityPath
	}
	cfg.channelMappingPath = envOrFn("SLACK_CHANNEL_MAPPING_PATH", defaultMappingPath)
	cfg.rigMappingPath = envOrFn("SLACK_RIG_MAPPING_PATH", defaultRigMappingPath)
	cfg.appsRegistryPath = envOrFn("SLACK_APPS_REGISTRY_PATH", defaultAppsRegistryPath)
	cfg.oauthClientID = getenv("SLACK_CLIENT_ID")
	cfg.oauthClientSecret = getenv("SLACK_CLIENT_SECRET")
	cfg.oauthRedirectURI = getenv("SLACK_REDIRECT_URI")
	cfg.oauthSlackBaseURL = getenv("SLACK_OAUTH_BASE_URL")
	cfg.threadSessionsStorePath = envOrFn("GC_SLACK_THREAD_SESSIONS_FILE", defaultThreadSessionsPath)
	cfg.roomLaunchPath = envOrFn("GC_SLACK_ROOM_LAUNCH_FILE", defaultRoomLaunchPath)
	cfg.subteamAliasStorePath = envOrFn("SLACK_SUBTEAM_ALIAS_FILE", defaultSubteamAliasPath)
	cfg.userAliasStorePath = envOrFn("SLACK_USER_ALIAS_FILE", defaultUserAliasPath)
	cfg.peerBotsPath = envOrFn("SLACK_PEER_BOTS_PATH", defaultPeerBotsPath)
	cfg.deliveryPolicyPath = envOrFn("SLACK_DELIVERY_POLICY_PATH", companyStateDirDefault(cfg.cityPath, "delivery_policy.json"))
	if d, err := time.ParseDuration(envOrFn("SLACK_COALESCE_WINDOW", defaultCoalesceWindow.String())); err == nil && d >= 0 {
		cfg.coalesceWindow = d
	} else {
		return cfg, fmt.Errorf("SLACK_COALESCE_WINDOW %q invalid (want a non-negative Go duration; 0 disables burst coalescing)", getenv("SLACK_COALESCE_WINDOW"))
	}

	// Head-protected reminder budget (gp-0qw + gp-9gc); see
	// reminder_budget.go for the contract. 0 disables.
	if n, err := strconv.Atoi(envOrFn("SLACK_REMINDER_TEXT_BUDGET", strconv.Itoa(defaultReminderTextBudget))); err == nil && n >= 0 {
		cfg.reminderTextBudget = n
	} else {
		return cfg, fmt.Errorf("SLACK_REMINDER_TEXT_BUDGET %q invalid (want a non-negative integer; 0 disables the reminder budget)", getenv("SLACK_REMINDER_TEXT_BUDGET"))
	}

	// Same-ts delivery-receipt gate (gp-32q). Armed by default: against
	// a gc that emits no receipt it is a no-op, so the only thing the
	// knob buys is an escape hatch if a gc build ever reports receipts
	// wrongly and the adapter starts re-posting deliveries that landed.
	switch strings.ToLower(strings.TrimSpace(envOrFn(deliveryReceiptGateEnv, "on"))) {
	case "off", "0", "false", "no":
		cfg.deliveryReceiptGate = false
	case "on", "1", "true", "yes", "":
		cfg.deliveryReceiptGate = true
	default:
		return cfg, fmt.Errorf("%s %q invalid (want on|off)", deliveryReceiptGateEnv, getenv(deliveryReceiptGateEnv))
	}

	// Socket Mode inbound transport + inbound-liveness watchdog (gp-3og).
	// SLACK_APP_TOKEN is the xapp- app-level token; SLACK_SOCKET_MODE is
	// the policy knob (auto|on|off). Liveness knobs are durations/lists
	// with conservative defaults; a malformed value is a startup error
	// (silently disabling the watchdog is exactly the failure mode it
	// exists to catch).
	cfg.slackAppToken = strings.TrimSpace(getenv("SLACK_APP_TOKEN"))
	cfg.socketMode = strings.ToLower(strings.TrimSpace(envOrFn("SLACK_SOCKET_MODE", socketModePolicyAuto)))
	switch cfg.socketMode {
	case socketModePolicyAuto, socketModePolicyOn, socketModePolicyOff:
	default:
		return cfg, fmt.Errorf("SLACK_SOCKET_MODE %q invalid (want auto|on|off)", cfg.socketMode)
	}
	if cfg.socketMode == socketModePolicyOn && cfg.slackAppToken == "" {
		return cfg, errors.New("SLACK_SOCKET_MODE=on requires SLACK_APP_TOKEN (xapp-… app-level token with connections:write)")
	}
	if cfg.slackAppToken != "" && !strings.HasPrefix(cfg.slackAppToken, "xapp-") {
		return cfg, errors.New("SLACK_APP_TOKEN must be an app-level token (xapp-…), not a bot/user token")
	}
	if d, err := time.ParseDuration(envOrFn("SLACK_SOCKET_SELF_RESTART_AFTER", "10m")); err == nil && d >= 0 {
		cfg.socketSelfRestartAfter = d
	} else {
		return cfg, fmt.Errorf("SLACK_SOCKET_SELF_RESTART_AFTER %q invalid (want a non-negative Go duration; 0 disables the self-restart)", getenv("SLACK_SOCKET_SELF_RESTART_AFTER"))
	}
	if d, err := time.ParseDuration(envOrFn("SLACK_LIVENESS_STALL_AFTER", "10m")); err == nil && d >= 0 {
		cfg.livenessStallAfter = d
	} else {
		return cfg, fmt.Errorf("SLACK_LIVENESS_STALL_AFTER %q invalid (want a non-negative Go duration; 0 disables)", getenv("SLACK_LIVENESS_STALL_AFTER"))
	}
	if d, err := time.ParseDuration(envOrFn("SLACK_BACKFILL_MAX_WINDOW", "1h")); err == nil && d >= 0 {
		cfg.backfillMaxWindow = d
	} else {
		return cfg, fmt.Errorf("SLACK_BACKFILL_MAX_WINDOW %q invalid (want a non-negative Go duration; 0 disables backfill delivery)", getenv("SLACK_BACKFILL_MAX_WINDOW"))
	}
	for _, c := range strings.Split(getenv("SLACK_LIVENESS_CHANNELS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cfg.livenessChannels = append(cfg.livenessChannels, c)
		}
	}
	cfg.livenessAlertChannel = strings.TrimSpace(getenv("SLACK_LIVENESS_ALERT_CHANNEL"))
	defaultLivenessStatePath := "/tmp/gc-slack-adapter/inbound_liveness.json"
	if cfg.cityPath != "" {
		defaultLivenessStatePath = filepath.Join(cfg.cityPath, ".gc", "slack", "inbound_liveness.json")
	}
	if v, ok := lookup("SLACK_LIVENESS_STATE_PATH"); ok {
		cfg.livenessStatePath = strings.TrimSpace(v)
	} else {
		cfg.livenessStatePath = defaultLivenessStatePath
	}
	// Shutdown spool for admitted-but-undelivered inbounds (gp-9e7 fix
	// round 2a'). Set-but-empty disables spooling deliberately.
	if v, ok := lookup("SLACK_COALESCE_SPOOL_PATH"); ok {
		cfg.coalesceSpoolPath = strings.TrimSpace(v)
	} else {
		cfg.coalesceSpoolPath = companyStateDirDefault(cfg.cityPath, "inbound_spool.jsonl")
	}
	// Dead letters for inbounds gc rejects deterministically (gp-xnc):
	// beside the spool, one JSONL file per channel.
	cfg.inboundDeadLetterDir = envOrFn("SLACK_INBOUND_DEAD_LETTER_DIR", companyStateDirDefault(cfg.cityPath, "inbound_dead_letter"))

	// Company-rooms (Phase 1) registry + ingress paths. The two JSON
	// registries follow the same city-rooted-then-/tmp default with an
	// env override as the six atomic registries; the ingress dir mirrors
	// thread_sessions.json resolution with a chat-ingress/ leaf. These
	// MUST match scripts/slack_company_directory.py file for file.
	defaultCompanyDirectoryPath := "/tmp/gc-slack-adapter/company_directory.json"
	defaultCompanyBindingsPath := "/tmp/gc-slack-adapter/company_bindings.json"
	defaultCompanyDMBindingsPath := "/tmp/gc-slack-adapter/dm_bindings.json"
	defaultCompanyAgentAppsPath := "/tmp/gc-slack-adapter/agent_apps.json"
	defaultCompanyIngressDir := "/tmp/gc-slack-adapter/chat-ingress"
	if cfg.cityPath != "" {
		defaultCompanyDirectoryPath = filepath.Join(cfg.cityPath, ".gc", "slack", "company_directory.json")
		defaultCompanyBindingsPath = filepath.Join(cfg.cityPath, ".gc", "slack", "company_bindings.json")
		defaultCompanyDMBindingsPath = filepath.Join(cfg.cityPath, ".gc", "slack", "dm_bindings.json")
		defaultCompanyAgentAppsPath = filepath.Join(cfg.cityPath, ".gc", "slack", "agent_apps.json")
		defaultCompanyIngressDir = filepath.Join(cfg.cityPath, ".gc", "slack", "chat-ingress")
	}
	cfg.companyDirectoryPath = envOrFn("SLACK_COMPANY_DIRECTORY_PATH", defaultCompanyDirectoryPath)
	cfg.companyBindingsPath = envOrFn("SLACK_COMPANY_BINDINGS_PATH", defaultCompanyBindingsPath)
	cfg.companyDMBindingsPath = envOrFn("SLACK_COMPANY_DM_BINDINGS_PATH", defaultCompanyDMBindingsPath)
	cfg.companyAgentAppsPath = envOrFn("SLACK_COMPANY_AGENT_APPS_PATH", defaultCompanyAgentAppsPath)
	cfg.companyIngressDir = envOrFn("SLACK_COMPANY_INGRESS_DIR", defaultCompanyIngressDir)
	cfg.companySelfBotUserID = getenv("SLACK_SWITCHBOARD_BOT_USER_ID")
	if raw := getenv("SLACK_COMPANY_CITY_APIS"); raw != "" {
		cfg.companyCityAPIs = make(map[string]string)
		for _, pair := range strings.Split(raw, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			name, base, ok := strings.Cut(pair, "=")
			name, base = strings.TrimSpace(name), strings.TrimRight(strings.TrimSpace(base), "/")
			if !ok || name == "" || base == "" || strings.ContainsAny(name, "/?#% \t") ||
				(!strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://")) {
				return cfg, fmt.Errorf("SLACK_COMPANY_CITY_APIS: invalid entry %q (want city=http(s)://host:port)", pair)
			}
			cfg.companyCityAPIs[name] = base
		}
	}
	// Visible-ack gate: off unless the env var is a non-empty value other
	// than "0" (the same truthiness the rest of the company config uses).
	if v := strings.TrimSpace(getenv("SLACK_COMPANY_VISIBLE_ACKS")); v != "" && v != "0" {
		cfg.companyVisibleAcks = true
	}
	// Advisory session-existence guard (Phase 4): same truthiness convention.
	// Default off — the guard must never reduce availability below flag-off.
	if v := strings.TrimSpace(getenv("SLACK_COMPANY_VERIFY_SESSIONS")); v != "" && v != "0" {
		cfg.companyVerifySessions = true
	}

	// Phase 2 shared-state directories. Same resolution precedence as the
	// registries above, with the Python leaf names (secrets/,
	// company-delegation-intents/, company-delegations/, company-current-turn/,
	// locks/). These MUST match scripts/slack_company_outbound.py file for
	// file.
	cfg.companySecretsDir = envOrFn("SLACK_COMPANY_SECRETS_DIR", companyStateDirDefault(cfg.cityPath, "secrets"))
	cfg.companyIntentsDir = envOrFn("SLACK_COMPANY_INTENTS_DIR", companyStateDirDefault(cfg.cityPath, "company-delegation-intents"))
	cfg.companyDelegationsDir = envOrFn("SLACK_COMPANY_DELEGATIONS_DIR", companyStateDirDefault(cfg.cityPath, "company-delegations"))
	cfg.companyTurnsDir = envOrFn("SLACK_COMPANY_TURNS_DIR", companyStateDirDefault(cfg.cityPath, "company-current-turn"))
	cfg.companyLocksDir = envOrFn("SLACK_COMPANY_LOCKS_DIR", companyStateDirDefault(cfg.cityPath, "locks"))

	// Retention controls. Defaults: keep inbound files for 7 days,
	// sweep every hour. Setting either to "0" disables the janitor.
	// Invalid duration strings also disable (with a fatal-config error
	// would be too aggressive — log and continue without sweeping).
	if d, err := time.ParseDuration(envOrFn("INBOUND_FILE_TTL", "168h")); err == nil {
		cfg.inboundFileTTL = d
	} else {
		log.Printf("INBOUND_FILE_TTL %q invalid: %v (janitor disabled)", getenv("INBOUND_FILE_TTL"), err)
	}
	if d, err := time.ParseDuration(envOrFn("INBOUND_FILE_SWEEP_INTERVAL", "1h")); err == nil {
		cfg.inboundFileSweepInterval = d
	} else {
		log.Printf("INBOUND_FILE_SWEEP_INTERVAL %q invalid: %v (janitor disabled)", getenv("INBOUND_FILE_SWEEP_INTERVAL"), err)
	}

	// dispatchConcurrency: bound goroutine fan-out on inbound dispatch
	// paths. Reject 0/negative/non-numeric at startup — silently
	// disabling dispatch (cap=0 -> always-drop) is almost certainly a
	// misconfiguration, and a non-numeric value usually means the
	// operator typo'd the var name. sec-S-04.
	raw := envOrFn("SLACK_DISPATCH_CONCURRENCY", "50")
	n, err := strconv.Atoi(raw)
	if err != nil {
		return cfg, fmt.Errorf("SLACK_DISPATCH_CONCURRENCY %q is not an integer: %w", raw, err)
	}
	if n <= 0 {
		return cfg, fmt.Errorf("SLACK_DISPATCH_CONCURRENCY must be > 0, got %d", n)
	}
	cfg.dispatchConcurrency = n

	// slackThreadContextLimit: cap on conversations.replies fetch when
	// seeding thread context (gc-px8.5). Reject 0/negative/non-numeric
	// at startup; an operator who typed an invalid limit almost
	// certainly didn't mean "disable thread context entirely" (silent
	// disable is a footgun). Use defaultThreadContextLimit when unset.
	rawLimit := envOrFn("SLACK_THREAD_CONTEXT_LIMIT", strconv.Itoa(defaultThreadContextLimit))
	limit, err := strconv.Atoi(rawLimit)
	if err != nil {
		return cfg, fmt.Errorf("SLACK_THREAD_CONTEXT_LIMIT %q is not an integer: %w", rawLimit, err)
	}
	if limit <= 0 {
		return cfg, fmt.Errorf("SLACK_THREAD_CONTEXT_LIMIT must be > 0, got %d", limit)
	}
	cfg.slackThreadContextLimit = limit

	if cfg.serviceSocket != "" {
		// proxy_process mode: gc reaches us via $GC_API_BASE_URL +
		// $GC_SERVICE_URL_PREFIX (e.g. http://127.0.0.1:8372/svc/slack).
		// gc's extmsg HTTP adapter appends "/publish" itself when calling,
		// so the registered base URL must NOT include /publish.
		urlPrefix := strings.TrimRight(getenv("GC_SERVICE_URL_PREFIX"), "/")
		if urlPrefix == "" {
			return cfg, errors.New("GC_SERVICE_SOCKET is set but GC_SERVICE_URL_PREFIX is empty — controller-injected env is incomplete")
		}
		if cfg.gcAPIBase == "" {
			return cfg, errors.New("GC_SERVICE_SOCKET is set but GC_API_BASE_URL is empty — cannot compute callback URL for self-registration")
		}
		cfg.internalCallbackURL = cfg.gcAPIBase + urlPrefix
	}

	var missing []string
	if cfg.accountID == "" {
		missing = append(missing, "SLACK_WORKSPACE_ID")
	}
	if cfg.slackBotToken == "" {
		missing = append(missing, "SLACK_BOT_TOKEN")
	}
	// SLACK_SIGNING_SECRET is now optional: gc-cby.16 introduced a
	// per-app apps registry (apps.json) that supplies signing secrets
	// per (workspace_id, app_id). The env var remains as a single-app
	// fallback for dev / legacy installs. lookupSigningSecrets returns
	// no candidates when both sources are empty, and the verify path
	// returns 401 — the correct fail-closed behavior.
	if cfg.cityName == "" {
		// GC_CITY_NAME is required: every inbound POST and every
		// dispatch-to-aliased-session call constructs a URL of the
		// form /v0/city/{cityName}/.... A wrong default silently
		// routes traffic to the wrong city, so fail-fast instead.
		missing = append(missing, "GC_CITY_NAME")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	// cityName is interpolated into every /v0/city/{cityName}/... URL the
	// adapter constructs. URL-significant characters (/, ?, #, %) here
	// would either change the URL's path structure or be ambiguously
	// interpreted by intermediate proxies — silently routing traffic to
	// the wrong city. cby-set-c added url.PathEscape on the session-scoped
	// dispatch paths, but other cityName interpolation sites still build
	// URLs with bare %s formatting (gc-cby.28 closes those, plus any
	// remaining sites). Until per-call escaping is uniform, this startup
	// guard is the primary defense — a legitimate city name should never
	// contain these characters, so reject them and fail fast. gc-cby.29.
	if strings.ContainsAny(cfg.cityName, "/?#%") {
		return cfg, fmt.Errorf("GC_CITY_NAME must not contain '/', '?', '#', or '%%': %q", cfg.cityName)
	}
	return cfg, nil
}

// gc-side types — mirrored from internal/extmsg/types.go to avoid coupling
// to the gc module. Wire-compatible only.

type conversationRef struct {
	ScopeID              string `json:"scope_id"`
	Provider             string `json:"provider"`
	AccountID            string `json:"account_id"`
	ConversationID       string `json:"conversation_id"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	Kind                 string `json:"kind"`
}

type publishRequest struct {
	SessionID        string            `json:"session_id"`
	Conversation     conversationRef   `json:"conversation"`
	Text             string            `json:"text"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// metadataKeySourceSessionID is the legacy metadata key gc used to
// propagate the originating session id before PublishRequest gained a
// native SessionID field (gc-kvt). Modern gc binaries write SessionID
// directly; this fallback exists only so older gc binaries publishing
// through this adapter still resolve the per-session identity record.
const metadataKeySourceSessionID = "source_session_id"

type publishReceipt struct {
	Conversation conversationRef `json:"conversation"`
	MessageID    string          `json:"message_id,omitempty"`
	Delivered    bool            `json:"delivered"`
	FailureKind  string          `json:"failure_kind,omitempty"`
}

type externalActor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

// externalAttachment mirrors extmsg.ExternalAttachment on the gc side.
// URL is a `file://` local path when the adapter has downloaded the bytes
// for inbound files (so bound sessions can read it directly without
// leaking the bot token); for outbound transcripts that originated as
// outbound files, URL is the Slack permalink.
type externalAttachment struct {
	ProviderID string `json:"provider_id"`
	URL        string `json:"url"`
	// mime_type is REQUIRED on the gc side (extmsg.ExternalAttachment):
	// never omitempty, always populated via attachmentMIMEType (gp-xnc).
	MIMEType string `json:"mime_type"`
}

type externalInboundMessage struct {
	ProviderMessageID string               `json:"provider_message_id"`
	Conversation      conversationRef      `json:"conversation"`
	Actor             externalActor        `json:"actor"`
	Text              string               `json:"text"`
	ExplicitTarget    string               `json:"explicit_target,omitempty"`
	ReplyToMessageID  string               `json:"reply_to_message_id,omitempty"`
	Attachments       []externalAttachment `json:"attachments,omitempty"`
	DedupKey          string               `json:"dedup_key,omitempty"`
	ReceivedAt        time.Time            `json:"received_at"`
}

type adapterCapabilities struct {
	SupportsChildConversations bool `json:"SupportsChildConversations"`
	SupportsAttachments        bool `json:"SupportsAttachments"`
	MaxMessageLength           int  `json:"MaxMessageLength"`
}

type adapterRegisterRequest struct {
	Provider     string              `json:"provider"`
	AccountID    string              `json:"account_id"`
	Name         string              `json:"name,omitempty"`
	CallbackURL  string              `json:"callback_url,omitempty"`
	Capabilities adapterCapabilities `json:"capabilities,omitempty"`
	// ReplyInstructions is the one-line reply-instruction template gc
	// renders into every inbound reminder in place of its generic
	// three-line reply-current fallback (extmsg
	// ReplyInstructionsProvider, gp-729 item 3). {conversation_id} is
	// substituted per reminder.
	ReplyInstructions string `json:"reply_instructions,omitempty"`
}

// Slack API types

type slackPostMessageReq struct {
	Channel   string `json:"channel"`
	Text      string `json:"text"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Username  string `json:"username,omitempty"`
	IconURL   string `json:"icon_url,omitempty"`
	IconEmoji string `json:"icon_emoji,omitempty"`
}

type slackPostMessageResp struct {
	OK      bool   `json:"ok"`
	TS      string `json:"ts,omitempty"`
	Channel string `json:"channel,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Slack files-upload-v2 API types.
//
// Slack deprecated the legacy /files.upload endpoint; the supported flow is
// the three-step v2 protocol:
//
//	1. POST /files.getUploadURLExternal (form-urlencoded) with {filename, length}
//	   → {ok, upload_url, file_id}
//	2. PUT raw bytes to the returned upload_url (no auth header — the URL is
//	   pre-signed and short-lived).
//	3. POST /files.completeUploadExternal (JSON) with {files: [{id, title}],
//	   channel_id, initial_comment, thread_ts} — channel posting happens here.
//
// The bot token requires the `files:write` scope. Without it, step 1 returns
// {ok: false, error: "missing_scope"} and the failure propagates as
// FailureKind="permanent" with the auth error logged.

type slackGetUploadURLResp struct {
	OK        bool   `json:"ok"`
	UploadURL string `json:"upload_url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type slackCompleteUploadFile struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type slackCompleteUploadReq struct {
	Files          []slackCompleteUploadFile `json:"files"`
	ChannelID      string                    `json:"channel_id,omitempty"`
	InitialComment string                    `json:"initial_comment,omitempty"`
	ThreadTS       string                    `json:"thread_ts,omitempty"`
}

type slackCompleteUploadResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Files []struct {
		ID string `json:"id"`
	} `json:"files,omitempty"`
}

// publishFileRequest is the body of POST /publish-file. Mirrors
// publishRequest but adds a file payload (path on the local filesystem
// the adapter can read). The session-id resolution precedence is the
// same: explicit SessionID wins over Metadata["source_session_id"].
type publishFileRequest struct {
	SessionID        string            `json:"session_id,omitempty"`
	Conversation     conversationRef   `json:"conversation"`
	FilePath         string            `json:"file_path"`
	Filename         string            `json:"filename,omitempty"`
	InitialComment   string            `json:"initial_comment,omitempty"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	Title            string            `json:"title,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// publishFileReceipt mirrors publishReceipt but carries the Slack file_id
// instead of a chat ts. When Delivered=true, FileID is the canonical
// reference for the uploaded file (used by tests + downstream tooling).
type publishFileReceipt struct {
	Conversation conversationRef `json:"conversation"`
	FileID       string          `json:"file_id,omitempty"`
	Delivered    bool            `json:"delivered"`
	FailureKind  string          `json:"failure_kind,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type slackReactionsAddReq struct {
	Channel   string `json:"channel"`
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
}

type slackReactionsAddResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// reactRequest is the body the slack pack POSTs to /react. The conversation
// id is the Slack channel id; the message id is the Slack ts. Emoji is the
// reaction name without colons (e.g. "eyes", not ":eyes:").
type reactRequest struct {
	Conversation conversationRef `json:"conversation"`
	MessageID    string          `json:"message_id"`
	Emoji        string          `json:"emoji"`
	// Remove selects reactions.remove instead of reactions.add
	// (hq-xizo). Absent or false preserves the historical add-only
	// behavior, so existing callers are unaffected.
	Remove bool `json:"remove,omitempty"`
}

type reactReceipt struct {
	Delivered   bool   `json:"delivered"`
	FailureKind string `json:"failure_kind,omitempty"`
}

// identityRecord is the persisted Slack identity override for a single gc
// session id. All fields are optional; an empty record means "use the default
// bot identity for any publish from this session". Slack's chat.postMessage
// requires the `chat:write.customize` scope for these fields to take effect.
type identityRecord struct {
	Username  string `json:"username,omitempty"`
	IconURL   string `json:"icon_url,omitempty"`
	IconEmoji string `json:"icon_emoji,omitempty"`
}

// identityRequest is the body of POST /identity. SessionID is required;
// every other field is optional. Posting an empty record (only session_id)
// effectively resets the session back to the default bot identity.
type identityRequest struct {
	SessionID string `json:"session_id"`
	Username  string `json:"username,omitempty"`
	IconURL   string `json:"icon_url,omitempty"`
	IconEmoji string `json:"icon_emoji,omitempty"`
}

type identityReceipt struct {
	Stored    bool   `json:"stored"`
	SessionID string `json:"session_id,omitempty"`
}

// identityDeleteReceipt is the response body of DELETE /identity. Existed
// is true when the session id was actually registered before; the call
// succeeds either way (idempotent delete).
type identityDeleteReceipt struct {
	Removed   bool   `json:"removed"`
	Existed   bool   `json:"existed"`
	SessionID string `json:"session_id,omitempty"`
}

// handleAliasRequest is the body of POST /handle-alias. Empty session_id
// removes the alias.
type handleAliasRequest struct {
	Handle    string `json:"handle"`
	SessionID string `json:"session_id"`
}

type handleAliasReceipt struct {
	Stored    bool   `json:"stored"`
	Removed   bool   `json:"removed,omitempty"`
	Handle    string `json:"handle,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// handleAliasDeleteReceipt mirrors identityDeleteReceipt for the alias
// registry. Existed is true iff the handle was actually registered.
type handleAliasDeleteReceipt struct {
	Removed bool   `json:"removed"`
	Existed bool   `json:"existed"`
	Handle  string `json:"handle,omitempty"`
}

// gcSessionMessageRequest mirrors handler_session_interaction.go's
// sessionMessageRequest. We POST it to gc /v0/session/{id}/messages to
// inject a system reminder into a session that has no binding for the
// originating Slack conversation.
type gcSessionMessageRequest struct {
	Message string `json:"message"`
}

type slackEventEnvelope struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	APIAppID  string `json:"api_app_id,omitempty"`
	// EventID is Slack's unique id for one event (Ev…). Retried
	// deliveries of the same event carry the same event_id, which is
	// what the redelivery seen-set keys on (hw-94w5k finding #4).
	EventID string          `json:"event_id,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"`
	// Authorizations names the app installation this delivery is for.
	// For a bot-token install the entry's user_id is the app's OWN bot
	// user id — the `<@U…>` id Slack's @-autocomplete inserts when a
	// human tags the bot — which lets processSlackEvent recognize a
	// message tagging the bot without an auth.test round trip (gp-4vq).
	Authorizations []slackEventAuthorization `json:"authorizations,omitempty"`
}

// slackEventAuthorization is one entry of an event envelope's
// authorizations array.
type slackEventAuthorization struct {
	UserID string `json:"user_id,omitempty"`
	IsBot  bool   `json:"is_bot,omitempty"`
}

// botUserID returns the adapter's own bot user id as carried in the
// envelope's authorizations, or "" when the delivery names none. The
// is_bot gate matters: a user-token install's authorization carries a
// HUMAN user id, and matching mentions of that human would busy-mark
// messages that never addressed the bot.
func (env slackEventEnvelope) botUserID() string {
	for _, a := range env.Authorizations {
		if a.IsBot && a.UserID != "" {
			return a.UserID
		}
	}
	return ""
}

// slackFile is a subset of Slack's file object, just the fields we need
// to download the bytes and pass useful metadata up to gc. Filetype, Size,
// and URLPrivateDownload feed the company file-hydration path (snippet
// content inlined into the frozen reminder); the download URL is preferred
// over url_private for content fetches because Slack marks it with the
// Content-Disposition that yields the raw bytes rather than an HTML wrapper.
type slackFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	Title              string `json:"title,omitempty"`
	URLPrivate         string `json:"url_private,omitempty"`
	URLPrivateDownload string `json:"url_private_download,omitempty"`
	MIMEType           string `json:"mimetype,omitempty"`
	Filetype           string `json:"filetype,omitempty"`
	// Subtype is set for recordings made inside Slack ("slack_audio"
	// voice clips, "slack_video"), which carry NO mimetype/filetype —
	// the only hint attachmentMIMEType has for an extension-less one.
	Subtype string `json:"subtype,omitempty"`
	Size    int    `json:"size,omitempty"`
}

type slackMessageEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	User    string `json:"user,omitempty"`
	BotID   string `json:"bot_id,omitempty"`
	Text    string `json:"text,omitempty"`
	Channel string `json:"channel,omitempty"`
	TS      string `json:"ts,omitempty"`
	// DeletedTS names the removed message on a subtype=message_deleted
	// event — the only field of that event this adapter consumes
	// (gp-0qw item 3: a buffered copy of the deleted ts is replaced
	// with an explicit deletion notice before delivery).
	DeletedTS   string          `json:"deleted_ts,omitempty"`
	ThreadTS    string          `json:"thread_ts,omitempty"`
	EventTS     string          `json:"event_ts,omitempty"`
	ChannelType string          `json:"channel_type,omitempty"`
	Files       []slackFile     `json:"files,omitempty"`
	Blocks      json.RawMessage `json:"blocks,omitempty"`
	// AppID / BotProfile corroborate a bot author's bots.info resolution
	// (Phase 2c); Metadata carries delegation / result correlation
	// breadcrumbs on company posts.
	AppID      string          `json:"app_id,omitempty"`
	BotProfile json.RawMessage `json:"bot_profile,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	// Initialize the shared dispatch semaphore on the cfg value before
	// any handler closes over it. cap is a fixed positive int —
	// loadConfig rejected 0/negative. sec-S-04. gc-px8.7.
	cfg.dispatchSem = make(chan struct{}, cfg.dispatchConcurrency)
	// Wire the process-wide thread-context cache. Nil-safe consumer
	// path; only the production main() initializes it. gc-px8.5.
	cfg.threadContextCache = newThreadContextCache()
	// Wire the busy-reaction registry before the event and publish
	// handlers close over cfg — both sides of the add-on-dispatch /
	// remove-on-reply lifecycle must see the same map. Nil-safe
	// consumer path (a nil registry just disables removal). hq-xizo.
	cfg.busyMarks = newBusyReactionRegistry()
	// Wire the Events API redelivery seen-set before handleSlackEvents
	// closes over cfg. Nil-safe (nil disables dedup). hw-94w5k #4.
	cfg.eventDedup = newEventDedupCache(eventDedupTTL)
	// Wire the per-(channel, ts) channel-audience delivery claims that
	// keep a bot-mention twin pair (message + app_mention, same ts)
	// from delivering the same message id twice into one session turn
	// (gp-ios, pc_c920ff5fe90c). Nil-safe (nil disables claiming).
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	// Wire the users.info display-name cache. Nil-safe consumer path
	// (nil disables resolution — raw ids pass through). hq-uxln9.
	cfg.userNames = newUserNameCache()
	// Wire the inbound-liveness tracker before handleSlackEvents closes
	// over cfg: every verified event_callback (either transport) feeds
	// it, and the watchdog/backfill read from it. The deliver + alert
	// hooks are attached below once the events handler exists. gp-3og.
	cfg.inboundLiveness = newInboundLiveness(cfg, newSlackHistoryClient(cfg.slackBotToken))
	livenessHealth.Store(cfg.inboundLiveness)
	internalDescr := cfg.internalListen
	if cfg.serviceSocket != "" {
		internalDescr = "uds:" + cfg.serviceSocket
	}
	log.Printf("starting gc-slack-adapter public=%s internal=%s gc=%s city=%s dispatch_concurrency=%d",
		cfg.publicListen, internalDescr, cfg.gcAPIBase, cfg.cityName, cfg.dispatchConcurrency)

	// Tighten any pre-existing /tmp/gc-slack-adapter/* state from
	// pre-fix installs to 0o700 dirs / 0o600 files. Must run BEFORE
	// the public listener binds and BEFORE concurrent writers
	// (registries on first save, janitor goroutine) start, so there's
	// no race with other writers in this process. gc-ywe.6.
	tightenStorePermissions(cfg)

	// Best-effort sweep of orphaned atomic-write .tmp files left over
	// from a previous crashed run. Runs before any registry constructor
	// so a follow-up first save cannot collide with a stale tmp name.
	// Errors are logged inside the helper; only directory-listing
	// failures bubble up here (treated as non-fatal — the registry will
	// still load from <diskPath>).
	for _, p := range []string{
		cfg.identityStorePath,
		cfg.handleAliasStorePath,
		cfg.channelMappingPath,
		cfg.rigMappingPath,
		cfg.appsRegistryPath,
		cfg.threadSessionsStorePath,
		cfg.roomLaunchPath,
	} {
		if err := sweepOrphanTmpFiles(p); err != nil {
			log.Printf("orphan-tmp sweep: %v", err)
		}
	}

	identityReg, err := newIdentityRegistry(cfg.identityStorePath)
	if err != nil {
		log.Fatalf("identity registry: %v", err)
	}
	log.Printf("identity registry: store=%s", cfg.identityStorePath)

	aliasReg, err := newHandleAliasRegistry(cfg.handleAliasStorePath)
	if err != nil {
		log.Fatalf("handle alias registry: %v", err)
	}
	log.Printf("handle alias registry: store=%s", cfg.handleAliasStorePath)

	threadReg, err := newThreadSessionRegistry(cfg.threadSessionsStorePath)
	if err != nil {
		log.Fatalf("thread session registry: %v", err)
	}
	log.Printf("thread session registry: store=%s", cfg.threadSessionsStorePath)

	threadHandleSticky := newThreadHandleStickiness()
	log.Printf("thread handle stickiness: in-memory only (no persistence in v1)")

	roomLaunchReg, err := newRoomLaunchMappingRegistry(cfg.roomLaunchPath)
	if err != nil {
		log.Fatalf("room launch mapping registry: %v", err)
	}
	log.Printf("room launch mapping registry: store=%s (read-only; SIGHUP or restart to reload)",
		cfg.roomLaunchPath)

	subteamAliases, err := newSubteamAliasMap(cfg.subteamAliasStorePath)
	if err != nil {
		log.Fatalf("subteam alias map: %v", err)
	}
	log.Printf("subteam alias map: store=%s entries=%d (read-only; SIGHUP or restart to reload)",
		cfg.subteamAliasStorePath, subteamAliases.Len())

	userAliases, err := newUserAliasMap(cfg.userAliasStorePath)
	if err != nil {
		log.Fatalf("user alias map: %v", err)
	}
	log.Printf("user alias map: store=%s entries=%d (read-only; SIGHUP or restart to reload)",
		cfg.userAliasStorePath, userAliases.Len())
	// Inbound display-name resolution consults the curated map's inverse
	// before users.info (hq-uxln9). Must be set on cfg before
	// handleSlackEvents closes over the cfg value below; sharing the
	// instance keeps SIGHUP reloads visible to inbound rendering.
	cfg.userAliases = userAliases

	peerBots, err := newPeerBotsRegistry(cfg.peerBotsPath)
	if err != nil {
		log.Fatalf("peer bots registry: %v", err)
	}
	log.Printf("peer bots registry: store=%s peers=%d (read-only; SIGHUP or restart to reload)",
		cfg.peerBotsPath, peerBots.Len())
	// Peer-bot visibility (gp-kop): the registry, the per-channel context
	// buffer, and the bots.info author resolver (reusing the company
	// gateway's resolver type for the never-deliver-own-posts self-guard)
	// must all be on cfg before handleSlackEvents closes over it.
	cfg.peerBots = peerBots
	cfg.peerContext = newPeerContextBuffer()
	cfg.peerAuthors = newBotInfoResolver(cfg.slackBotToken)

	channelMapReg, err := newChannelMappingRegistry(cfg.channelMappingPath)
	if err != nil {
		log.Fatalf("channel mapping registry: %v", err)
	}
	log.Printf("channel mapping registry: store=%s entries=%d (read-only; SIGHUP or restart to reload)",
		cfg.channelMappingPath, channelMapReg.Len())

	rigMapReg, err := newRigMappingRegistry(cfg.rigMappingPath)
	if err != nil {
		log.Fatalf("rig mapping registry: %v", err)
	}
	log.Printf("rig mapping registry: store=%s entries=%d (read-only; SIGHUP or restart to reload)",
		cfg.rigMappingPath, rigMapReg.Len())

	appsReg, err := newAppsRegistry(cfg.appsRegistryPath)
	if err != nil {
		log.Fatalf("apps registry: %v", err)
	}
	cfg.appsRegistry = appsReg
	log.Printf("apps registry: store=%s entries=%d (read-only; SIGHUP or restart to reload)",
		cfg.appsRegistryPath, appsReg.Len())
	if appsReg.Len() == 0 && cfg.slackSigningKey == "" {
		log.Printf("WARN: apps registry is empty and SLACK_SIGNING_SECRET is unset — all inbound Slack requests will be rejected with 401 until an app is imported (gc slack import-app + OAuth) or the env var is set")
	}

	// Cross-store overlap WARN: surface contradictory bindings (cby.3
	// channel mapping vs cby.4 rig mapping pointing at different rigs
	// for the same channel) at startup so operators see them in
	// adapter logs. resolveChannelTarget always lets channel mapping
	// win at runtime — this is purely observability.
	logCrossStoreOverlapWarnings(channelMapReg, rigMapReg)

	// Company-rooms (Slack company-rooms Phase 1) wiring. The two CLI-
	// written registries load with the never-fatal contract (a corrupt or
	// invalid file installs a nil snapshot and disables company routing
	// while legacy traffic keeps flowing). The durable ingress store is a
	// hard prerequisite for admission, but its construction failure is NOT
	// a legacy fallthrough: the gateway is wired even when the store cannot
	// be created, and runs degraded (barrier stays closed, company-room
	// admissible events get 503 without x-slack-no-retry, /healthz reports
	// the store error, startRecovery retries construction). companyGW must
	// be set on cfg BEFORE handleSlackEvents closes over the cfg value below.
	companyDirStore := &companyDirectoryStore{}
	if err := companyDirStore.Load(cfg.companyDirectoryPath); err != nil {
		log.Printf("company directory: initial load surfaced %v (routing disabled until a valid file is imported)", err)
	}
	companyBindStore := &companyBindingsStore{}
	if err := companyBindStore.Load(cfg.companyBindingsPath, companyDirStore.Snapshot()); err != nil {
		log.Printf("company bindings: initial load surfaced %v", err)
	}
	receipts, rerr := NewIngressReceiptStore(cfg.companyIngressDir)
	if rerr != nil {
		log.Printf("WARN: company ingress store %q: %v — gateway starting DEGRADED (company events 503, never legacy; construction retried)",
			cfg.companyIngressDir, rerr)
	}
	companyGW := newCompanyGateway(cfg, companyDirStore, companyBindStore, receipts)
	if rerr != nil {
		companyGW.setStoreError(rerr)
	}
	cfg.companyGateway = companyGW
	companyHealthStatus.Store(companyGW)
	log.Printf("company gateway: directory=%s bindings=%s dm_bindings=%s agent_apps=%s ingress=%s self_bot=%q dir_loaded=%v bindings_loaded=%v dm_bindings_loaded=%v registered_agent_apps=%d verify_sessions=%v store_ready=%v",
		cfg.companyDirectoryPath, cfg.companyBindingsPath, cfg.companyDMBindingsPath, cfg.companyAgentAppsPath, cfg.companyIngressDir, cfg.companySelfBotUserID,
		companyDirStore.Snapshot() != nil, companyBindStore.Snapshot() != nil,
		companyGW.dmBindStore.Snapshot() != nil, companyGW.agentApps.Snapshot().Len(), cfg.companyVerifySessions, receipts != nil)

	// Token-efficiency pass (gp-729): the per-channel delivery-policy
	// registry, the burst coalescer, and the supporting caches must all
	// be on cfg before handleSlackEvents closes over the cfg value
	// below. The coalescer's deliver closure captures the completed cfg
	// copy — set immediately after the last cfg field assignment and
	// before the listener starts, so no event can race an unset deliver.
	deliveryPolicy, err := newDeliveryPolicyRegistry(cfg.deliveryPolicyPath)
	if err != nil {
		log.Fatalf("delivery policy registry: %v", err)
	}
	log.Printf("delivery policy registry: store=%s channels=%d (read-only; SIGHUP or restart to reload)",
		cfg.deliveryPolicyPath, deliveryPolicy.Len())
	cfg.deliveryPolicy = deliveryPolicy
	cfg.deliveredIDs = newDeliveredIDs()
	cfg.channelNames = newChannelNameCache()
	cfg.replyHelp = newOncePerChannel()
	cfg.bindingCheck = newBindingCheckCache()
	cfg.coalescer = newInboundCoalescer(cfg.coalesceWindow, deliveryPolicy)
	// Shutdown must await in-flight event goroutines before draining the
	// coalescer (gp-9e7 fix round 1a) — wired before the handlers below
	// close over the cfg value.
	cfg.eventWG = &sync.WaitGroup{}
	// Shutdown admission barrier + durable spool (gp-9e7 fix round
	// 2a'/2b') — wired before the handlers close over cfg. The spool
	// receives what the shutdown drain cannot deliver; with no spool
	// (SLACK_COALESCE_SPOOL_PATH explicitly empty) spill stays nil and
	// the coalescer logs that residue as LOST instead.
	cfg.draining = &atomic.Bool{}
	cfg.inboundSpool = newInboundSpool(cfg.coalesceSpoolPath)
	if cfg.inboundSpool != nil {
		cfg.coalescer.spill = cfg.inboundSpool.spillBatch
		// Deletions persist alongside spilled entries so a restart still
		// replays a deleted message as its notice (gp-0qw) — but ONLY
		// while draining: the spool exists solely across a
		// shutdown→startup boundary, and a record per deletion through
		// normal uptime would grow the file unbounded toward the replay
		// cap (codex round-5 finding 3). A deletion processed before the
		// drain is an in-memory tombstone every drain-time producer
		// consults; one processed during the drain lands here.
		cfg.coalescer.persistDeletion = func(channel, ts string) {
			if cfg.draining != nil && cfg.draining.Load() {
				cfg.inboundSpool.recordDeletion(channel, ts)
			}
		}
	}
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}
	// gp-xnc: a message gc rejects deterministically is written here
	// after maxCoalesceDeliveryAttempts instead of blocking its channel
	// forever; the record carries the full envelope for a manual re-post.
	// Pre-create the directory so the write-time path only ever has to
	// confirm one directory entry (its durability contract, codex r3).
	if err := os.MkdirAll(cfg.inboundDeadLetterDir, 0o700); err != nil {
		log.Printf("inbound dead-letter dir %q could not be created at startup (%v) — writes will retry creating it", cfg.inboundDeadLetterDir, err)
	}
	cfg.coalescer.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		path, err := writeInboundDeadLetter(deliverCfg.inboundDeadLetterDir, channel, batch, cause)
		if err != nil {
			log.Printf("coalesce: chan=%s dead-letter write FAILED (dir %q) — %d message(s) stay buffered and the write retries next window: %v",
				channel, deliverCfg.inboundDeadLetterDir, len(batch), err)
			return false
		}
		log.Printf("coalesce: chan=%s %d message(s) written to dead-letter file %s — inspect and re-post by hand once the rejection cause is fixed",
			channel, len(batch), path)
		return true
	}
	log.Printf("inbound coalescer: window=%s (0 disables) policy_channels=%d spool=%q dead_letter_dir=%q",
		cfg.coalesceWindow, deliveryPolicy.Len(), cfg.coalesceSpoolPath, cfg.inboundDeadLetterDir)

	// Public mux: only /slack/events + /slack/interactions
	// (HMAC-verified) and /healthz. Bound to 0.0.0.0 by default so
	// Tailscale Funnel can reach it.
	publicMux := http.NewServeMux()
	eventsHandler := handleSlackEvents(cfg, aliasReg, threadReg, roomLaunchReg, subteamAliases, threadHandleSticky)
	interactionsHandler := handleSlackInteractions(cfg, channelMapReg, rigMapReg)
	publicMux.HandleFunc("/slack/events", eventsHandler)
	publicMux.HandleFunc("/slack/interactions", interactionsHandler)
	// Backfill replays and the stall alarm ride on the same handlers /
	// bot token. gp-3og.
	cfg.inboundLiveness.deliver = deliverViaHandler(eventsHandler)
	cfg.inboundLiveness.alert = slackAlertPoster(cfg.slackBotToken, cfg.livenessAlertChannel)
	log.Printf("inbound liveness: watchdog stall_after=%s backfill_window=%s pinned_channels=%d alert_channel=%q state=%s",
		cfg.livenessStallAfter, cfg.backfillMaxWindow, len(cfg.livenessChannels), cfg.livenessAlertChannel, cfg.livenessStatePath)
	registerOAuthHandlers(publicMux, cfg, appsReg)
	publicMux.HandleFunc("/healthz", handleHealthz)
	publicMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// Internal mux: /publish (gc-only). Served either on a UDS that gc
	// proxies through /svc/{name}/ (proxy_process mode), or on a
	// 127.0.0.1 TCP listener (legacy nohup mode).
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/publish", handlePublish(cfg, identityReg, userAliases, newPublishDedupCache(publishDedupTTL)))
	internalMux.HandleFunc("/publish-file", handlePublishFile(cfg, identityReg))
	internalMux.HandleFunc("/react", handleReact(cfg))
	internalMux.HandleFunc("POST /identity", handleIdentity(identityReg))
	internalMux.HandleFunc("DELETE /identity", handleIdentityDelete(identityReg))
	internalMux.HandleFunc("POST /handle-alias", handleHandleAlias(aliasReg))
	internalMux.HandleFunc("DELETE /handle-alias", handleHandleAliasDelete(aliasReg))
	// Company-rooms operator surface: the receipt listing + redrive endpoints
	// (Phase 3b) backing the `gc slack company-status` / `company-redrive` verbs,
	// plus the Phase 5 body-redaction hook (`gc slack company-redact`).
	// Registered only when the company gateway is wired.
	if cfg.companyGateway != nil {
		internalMux.HandleFunc("/internal/company/receipts", cfg.companyGateway.handleCompanyReceipts)
		internalMux.HandleFunc("/internal/company/redrive", cfg.companyGateway.handleCompanyRedrive)
		internalMux.HandleFunc("/internal/company/redact", cfg.companyGateway.handleCompanyRedact)
	}
	internalMux.HandleFunc("/healthz", handleHealthz)

	publicSrv := &http.Server{
		Addr:              cfg.publicListen,
		Handler:           publicMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	internalSrv := &http.Server{
		Handler:           internalMux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.registerOnStart {
		if err := registerAdapter(cfg); err != nil {
			// A gc predating extmsg reply_instructions (hq-fh9,
			// 2026-07-06) may reject the unknown field outright.
			// The template is a token nicety — never worth failing
			// startup over — so retry once without it before the
			// fatal verdict; gc then keeps its generic fallback text.
			log.Printf("WARN: register adapter with reply_instructions failed (%v); retrying without the template", err)
			if err := registerAdapterWithoutReplyInstructions(cfg); err != nil {
				log.Fatalf("register adapter: %v", err)
			}
		}
		mode := "LOCALHOST ONLY"
		if cfg.serviceSocket != "" {
			mode = "via gc /svc proxy"
		}
		log.Printf("registered with gc as provider=%s account=%s callback=%s/publish (%s)",
			cfg.provider, cfg.accountID, cfg.internalCallbackURL, mode)
	}

	// Replay the previous shutdown's spool (gp-9e7 fix round 2a'):
	// batches that shutdown could not deliver re-enter the coalescer's
	// normal buffers — messages deliver on the coalesce window's timer,
	// reactions rejoin the no-wake side lane. The watermark advanced at
	// their ADMISSION, so the startup watermark backfill can never
	// re-fetch these; the spool is their only redelivery path. Runs
	// before the listeners start, after gc registration.
	if n := cfg.inboundSpool.replayInto(cfg.coalescer); n > 0 {
		log.Printf("inbound spool: re-buffered %d item(s) the previous shutdown could not deliver", n)
	}

	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go runInboundFileJanitor(janitorCtx, cfg)
	go runDispatchDropSummary(janitorCtx, dispatchDropSummaryInterval, cfg.dispatchConcurrency)
	go cfg.inboundLiveness.runWatchdog(janitorCtx)

	// Socket Mode inbound transport (gp-3og). Runs alongside the public
	// listener; Slack routes each app's events to exactly one of the two
	// per the app's "Socket Mode" toggle, so the operator's cutover is a
	// UI flip with the HTTP path kept for rollback.
	// The socket transport gets its own context + done channel so
	// shutdown can stop envelope intake and wait for in-flight envelope
	// handlers BEFORE the coalescer drain (gp-9e7 fix round 1a): the
	// janitor context is cancelled only by main's deferred cancel, which
	// runs after flushAll — too late to stop admissions into the drain.
	socketCtx, socketCancel := context.WithCancel(context.Background())
	defer socketCancel()
	socketDone := make(chan struct{})
	var socketRunner *socketModeRunner
	// selfRestart carries the exit code of a gp-bsk socket self-restart
	// request into the shutdown select below; nil (never fires) when
	// Socket Mode is disabled.
	var selfRestart <-chan int
	if socketModeEnabled(cfg) {
		socketRunner = newSocketModeRunner(cfg, eventsHandler, interactionsHandler, cfg.inboundLiveness)
		selfRestart = selfRestartRequests(socketRunner)
		socketModeHealth.Store(socketRunner)
		log.Printf("socket mode: enabled (policy=%s) — dialing Slack with the app-level token; Events API listener stays up for rollback/other apps", cfg.socketMode)
		go func() {
			defer close(socketDone)
			socketRunner.run(socketCtx)
		}()
	} else {
		close(socketDone)
		log.Printf("socket mode: disabled (policy=%s app_token_set=%v) — inbound relies on the Events API Request URL", cfg.socketMode, cfg.slackAppToken != "")
	}

	// Thread-binding teardown subscriber (cby.5.4): listens to gc's
	// city-scoped event stream for terminal session lifecycle events
	// (session.stopped, session.crashed) and drops the corresponding
	// thread→session binding plus any handle aliases the launcher
	// bootstrapped on spawn. Best-effort: a missing gcAPIBase or
	// cityName disables the goroutine cleanly.
	go runThreadTeardownSubscriber(janitorCtx, teardownSubscriberConfig{
		gcAPIBase: cfg.gcAPIBase,
		cityName:  cfg.cityName,
	}, threadReg, aliasReg)

	// Company-rooms startup recovery barrier + periodic sweep. Until the
	// barrier opens, company-admissible events receive 503 (retryable);
	// legacy routes, /healthz, interactions, and the internal listener
	// serve immediately (they never consulted the gateway). The recovery
	// pass and sweep are no-ops when the gateway is nil.
	companyGW.startRecovery(janitorCtx)

	errCh := make(chan error, 2)
	go func() {
		log.Printf("public listener serving on %s (Slack events)", cfg.publicListen)
		errCh <- publicSrv.ListenAndServe()
	}()
	go func() {
		if cfg.serviceSocket != "" {
			log.Printf("internal listener serving on UDS %s (gc proxy_process)", cfg.serviceSocket)
			lis, err := listenUDS(cfg.serviceSocket)
			if err != nil {
				errCh <- fmt.Errorf("listen unix %s: %w", cfg.serviceSocket, err)
				return
			}
			errCh <- internalSrv.Serve(lis)
		} else {
			internalSrv.Addr = cfg.internalListen
			log.Printf("internal listener serving on %s (gc publish only)", cfg.internalListen)
			errCh <- internalSrv.ListenAndServe()
		}
	}()

	// SIGHUP-driven reload of the four CLI-written registry files
	// (apps, channel mappings, rig mappings, room launch mappings) —
	// gc-cby.23. Buffer-size-1 + a separate Notify channel from `stop`
	// so SIGHUP cannot trigger shutdown. reloadStop is closed by the
	// trailing defer below alongside janitorCancel, so the goroutine
	// exits cleanly during shutdown.
	reloadStop := make(chan struct{})
	defer close(reloadStop)
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	go runReloadLoop(reloadStop, hupCh, func() {
		logReloadOutcome(appsReg, channelMapReg, rigMapReg, roomLaunchReg, subteamAliases, userAliases, peerBots, deliveryPolicy)
		// Company stores reload on the same SIGHUP but OUTSIDE the atomic
		// six-registry set: a stale/invalid company file retains its own
		// last-known-good snapshot (handled inside StageReload) and never
		// blocks the six above, which have already committed.
		companyGW.reloadOnSIGHUP()
		// Re-arm coalescer timers against the (possibly changed)
		// delivery policy: a channel flipped digest→immediate must not
		// keep waiting out a stale two-hour window (gp-729 item 6).
		cfg.coalescer.reconcileTimers()
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	exitCode := 0
	select {
	case <-stop:
		log.Println("shutting down (signal)")
	case code := <-selfRestart:
		// gp-bsk socket self-restart: the SAME orderly drain as a signal
		// (admitted-but-undelivered inbounds reach gc or the spool), then
		// a non-zero exit so the service supervisor restarts the process.
		log.Printf("shutting down (socket self-restart) — orderly drain first, then exit %d for the service supervisor", code)
		exitCode = code
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("listener error: %v", err)
		}
	}
	// 0. Admission barrier FIRST (gp-9e7 fix round 2b'): from here every
	//    event envelope is refused with 503 BEFORE it is acked to Slack
	//    or advances the liveness watermark — on the Events API the
	//    retry ladder redelivers (and whatever it gives up on stays
	//    above the watermark for the startup backfill); on Socket Mode
	//    the 503 leaves the envelope un-acked, same effect. The bounded
	//    waits below (socket stop, eventWG) therefore only govern events
	//    admitted BEFORE this flip, and any straggler outliving them
	//    hits the coalescer's post-drain closed gate and spools instead
	//    of being lost.
	cfg.draining.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = publicSrv.Shutdown(ctx)
	_ = internalSrv.Shutdown(ctx)
	// Loss-safe drain ordering (gp-9e7 fix round 1a): first stop every
	// event source, then await the detached event goroutines those
	// sources spawned — each can still admit messages/reactions to the
	// coalescer — and only then drain the buffers. An admission landing
	// after the drain would otherwise be permanently lost on exit: Slack
	// got its 200 long ago and will never redeliver.
	//
	// 1. HTTP listeners are down (Shutdown above waits for in-flight
	//    handlers, whose eventWG.Add precedes their return).
	// 2. Socket Mode: cancel its context and wait for run() — which
	//    itself awaits its in-flight envelope handlers — so no new
	//    envelope reaches handleSlackEvents after this point. The
	//    janitors stop with it: the liveness watchdog can inject
	//    backfill events through the same handler, and a backfill
	//    starting during the drain would race the loss-safety this
	//    ordering exists for (the deferred cancel ran only after
	//    flushAll).
	janitorCancel()
	socketCancel()
	select {
	case <-socketDone:
	case <-time.After(shutdownSocketStopTimeout):
		log.Printf("shutdown: socket mode runner still stopping after %s — proceeding", shutdownSocketStopTimeout)
	}
	// 3. Await the detached per-event goroutines (bounded: every path
	//    through processSlackEvent concludes on gc-forward timeouts).
	if !awaitWaitGroup(cfg.eventWG, shutdownEventDrainTimeout) {
		log.Printf("shutdown: in-flight event goroutines still running after %s — proceeding to coalescer drain (a straggler admission lands in the spool via the closed gate, not in memory)", shutdownEventDrainTimeout)
	}
	// 4. Buffered coalesced messages were already acked to Slack — drain
	//    them to gc (to a fixpoint: flushAll also waits out in-flight
	//    timer/swept deliveries and re-drains what failed ones restore)
	//    so a normal shutdown never loses a window's worth of chatter
	//    (gp-729; fixpoint per gp-9e7 fix round 1a/2b). flushAll closes
	//    admission atomically with its final snapshot and spools any
	//    undeliverable residue for startup replay (fix round 2a'/2b') —
	//    the watermark advanced at admission, so nothing else could
	//    ever redeliver it.
	cfg.coalescer.flushAll()
	// 5. Seal the spool as the very last step (gp-9e7 round 3, 2c):
	//    every spool write runs entirely under the spool mutex, so the
	//    seal JOINS any write still in flight — e.g. a straggler event
	//    goroutine that outlived the bounded eventWG wait and spilled
	//    through the closed gate — and refuses anything later. No spool
	//    write can therefore race process exit and be torn by it; a
	//    post-seal straggler degrades to the loud LOSS log instead of a
	//    corrupt spool.
	cfg.inboundSpool.seal()
	if exitCode != 0 {
		// Past the seal nothing else in this process can touch the spool
		// or the liveness state; exit with the self-restart's code so the
		// service supervisor restarts the adapter (gp-bsk).
		log.Printf("shutdown: drain complete — exiting %d (socket self-restart; the service supervisor restarts the adapter)", exitCode)
		os.Exit(exitCode)
	}
}

// shutdownSocketStopTimeout bounds the wait for the Socket Mode runner
// to stop consuming envelopes at shutdown; shutdownEventDrainTimeout
// bounds the wait for detached event goroutines. Both are backstops —
// the underlying work concludes on its own gc/Slack HTTP timeouts —
// so a wedged dependency degrades to today's best-effort exit instead
// of hanging the process.
const (
	shutdownSocketStopTimeout = 10 * time.Second
	shutdownEventDrainTimeout = 20 * time.Second
)

// selfRestartRequests routes the Socket Mode runner's self-restart exit
// hook (gp-bsk) through main's orderly shutdown instead of os.Exit.
//
// A bare os.Exit skips the gp-9e7 drain, and that is a loss path: the
// inbound-liveness watermark advances at ADMISSION, so any message
// sitting in the coalescer's buffers at exit is below the persisted
// watermark with Slack's 200 long since sent — nothing ever redelivers
// it. That is not a corner case for the self-restart specifically: the
// liveness watchdog's probe ALARMS first (which escalates a down runner
// into the aggressive reconnect whose prompt failure is what trips
// maybeSelfRestart) and only then backfills the missed messages through
// the events handler into those very buffers.
//
// The returned channel carries the requested exit code once; main's
// shutdown select drains → spools → seals and then exits with it. The
// hook never blocks the runner (it is called from inside the client
// loop) and later calls are no-ops.
//
// Deliberately NO preemptive hard-exit timer (codex gate, gp-ps4): the
// drain is finite by construction — listener Shutdown (5s), socket stop
// (shutdownSocketStopTimeout), eventWG (shutdownEventDrainTimeout),
// flushAll (maxFlushAllPasses passes, each gc POST on its own timeout),
// seal (joins one in-flight write) — but its length scales with the
// number of channels holding residue, so any fixed timer either fires
// before the seal on a slow-but-healthy drain (dropping exactly the
// admitted messages this exists to save) or is too long to mean
// anything. The self-restart therefore takes the same bounded exit as
// SIGTERM, and the service supervisor's own kill policy stays the
// backstop of last resort, as it is for every shutdown.
func selfRestartRequests(r *socketModeRunner) <-chan int {
	requests := make(chan int, 1)
	var once sync.Once
	r.exit = func(code int) {
		once.Do(func() { requests <- code }) // buffered; the single send can never block
	}
	return requests
}

// awaitWaitGroup waits for wg with a timeout; true when the group
// settled, false on timeout (or trivially true on a nil group).
func awaitWaitGroup(wg *sync.WaitGroup, timeout time.Duration) bool {
	if wg == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// listenUDS binds a Unix domain socket at path, removing any stale entry
// first so restarts succeed. The socket file is left in place on shutdown
// — the controller's proxy_process supervisor cleans it up via
// cleanupProxyProcessSocketPath when the service is closed.
func listenUDS(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	// Defense-in-depth: the controller-managed parent at
	// /tmp/gcsvc-<uid>/<hash>/ is already 0o700 so the socket is
	// unreachable to other UIDs via parent-dir traversal-deny, but
	// chmod the socket itself too in case the parent ever loosens.
	// gc-ywe.6.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod uds: %w", err)
	}
	return lis, nil
}

func registerAdapter(cfg config) error {
	return registerAdapterRequest(cfg, slackReplyInstructionsTemplate)
}

// registerAdapterWithoutReplyInstructions is the compatibility retry for
// a gc predating the reply_instructions registration field (hq-fh9).
func registerAdapterWithoutReplyInstructions(cfg config) error {
	return registerAdapterRequest(cfg, "")
}

func registerAdapterRequest(cfg config, replyInstructions string) error {
	body, _ := json.Marshal(adapterRegisterRequest{
		Provider:    cfg.provider,
		AccountID:   cfg.accountID,
		Name:        "slack-adapter",
		CallbackURL: cfg.internalCallbackURL,
		Capabilities: adapterCapabilities{
			SupportsChildConversations: false,
			SupportsAttachments:        true,
			MaxMessageLength:           40000, // Slack's chat.postMessage limit
		},
		ReplyInstructions: replyInstructions,
	})
	// PathEscape cityName so URL-significant characters cannot alter
	// routing on the gc API side (sec-S-06). cityName is operator-supplied
	// via GC_CITY_NAME and gc-cby.29 rejects /?#% at startup, but the
	// per-call escape keeps the wire format correct regardless and matches
	// the dispatch paths that cby-set-c hardened. gc-cby.28.
	target := fmt.Sprintf("%s/v0/city/%s/extmsg/adapters", cfg.gcAPIBase, url.PathEscape(cfg.cityName))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "gc-slack-adapter")
	resp, err := gcForwardClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("register failed: %s — %s", resp.Status, string(respBody))
	}
	return nil
}

// publishDedupTTL bounds how long a delivered receipt is remembered for
// idempotent replay. It only needs to span the retry-after-timeout window:
// the pack's HTTP client times out at 30s and an agent retry follows shortly
// after, so a couple of minutes comfortably covers the reported failure mode
// (gpk-lbhl) while staying short enough that an intentional identical resend
// minutes later is not silently swallowed.
const publishDedupTTL = 2 * time.Minute

// publishDedupCache remembers delivered publish receipts keyed by the
// caller-supplied idempotency key, so a retry after a delivered-but-
// timed-out POST returns the original receipt instead of posting a second
// Slack message (gpk-lbhl). Only delivered receipts are cached: a retry
// after a genuine (non-delivered) failure must still re-attempt delivery,
// so failures are never remembered. An empty idempotency key disables
// dedup for that call.
type publishDedupCache struct {
	mu      sync.Mutex
	entries map[string]publishDedupEntry
	ttl     time.Duration
	now     func() time.Time
}

type publishDedupEntry struct {
	receipt   publishReceipt
	expiresAt time.Time
}

func newPublishDedupCache(ttl time.Duration) *publishDedupCache {
	return &publishDedupCache{
		entries: make(map[string]publishDedupEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Get returns the cached receipt for key when one is present and unexpired.
func (c *publishDedupCache) Get(key string) (publishReceipt, bool) {
	if key == "" {
		return publishReceipt{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return publishReceipt{}, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.entries, key)
		return publishReceipt{}, false
	}
	return e.receipt, true
}

// Put records a delivered receipt under key and sweeps expired entries so
// the map stays bounded under churn. Empty keys and non-delivered receipts
// are ignored.
func (c *publishDedupCache) Put(key string, receipt publishReceipt) {
	if key == "" || !receipt.Delivered {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.entries[key] = publishDedupEntry{receipt: receipt, expiresAt: now.Add(c.ttl)}
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

func handlePublish(cfg config, reg *identityRegistry, userAliases *userAliasMap, dedup *publishDedupCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req publishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}

		// SessionID precedence: explicit field wins (used by direct-to-adapter
		// callers like smoke tests). Otherwise fall back to the wire-metadata
		// key gc populates when forwarding from /v0/city/.../extmsg/outbound.
		identitySessionID := req.SessionID
		if identitySessionID == "" {
			identitySessionID = req.Metadata[metadataKeySourceSessionID]
		}
		// Fail closed on identity-less publishes (gpk-jqou). With neither the
		// native session_id nor the legacy metadata.source_session_id set, the
		// adapter would otherwise post at channel root under the default bot
		// identity (as=""), which surfaced as the spurious "gc oversight PL
		// replied in channel" anomaly on 2026-05-25. Every legitimate /publish
		// caller carries a session — the gc HandleOutbound path and the
		// publish / publish-to-channel / reply-current CLIs all resolve one (or
		// fail closed) before reaching here, and system bot notifications use
		// `gc slack post-message`, which posts to Slack directly and never
		// traverses this endpoint. So rejecting attribution-less requests has
		// no legitimate caller to break; it only closes the regression door.
		if identitySessionID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "publish requires session attribution: provide session_id or metadata.source_session_id",
			})
			return
		}

		// Rewrite outbound @handle body mentions to Slack mention syntax
		// for handles the operator has mapped (gpk-uha7). Unmapped handles
		// and a nil/empty map leave the text untouched, so this is a no-op
		// for installs that haven't curated slack-user-aliases.json.
		rewrittenText := userAliases.rewrite(req.Text)
		post := slackPostMessageReq{
			Channel:  req.Conversation.ConversationID,
			Text:     rewrittenText,
			ThreadTS: req.ReplyToMessageID,
		}
		identityApplied := ""
		if reg != nil {
			if rec, ok := reg.Get(identitySessionID); ok {
				post.Username = rec.Username
				post.IconURL = rec.IconURL
				post.IconEmoji = rec.IconEmoji
				identityApplied = rec.Username
			}
		}
		log.Printf("publish: conv=%s text=%dch reply_to=%s idem=%s session=%s as=%q mentions_rewritten=%t",
			req.Conversation.ConversationID, len(req.Text), req.ReplyToMessageID,
			req.IdempotencyKey, identitySessionID, identityApplied, rewrittenText != req.Text)

		// Idempotent replay: if this idempotency key already produced a
		// delivered receipt, return it without re-posting. This is the
		// chokepoint that absorbs a retry after a delivered-but-timed-out
		// POST (gpk-lbhl) — the original Slack message stands, no duplicate.
		if cached, ok := dedup.Get(req.IdempotencyKey); ok {
			log.Printf("publish: dedup hit idem=%s conv=%s -> returning cached receipt (no re-post)",
				req.IdempotencyKey, req.Conversation.ConversationID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cached)
			return
		}

		slackResp, err := postToSlack(cfg.slackBotToken, post)
		receipt := publishReceipt{Conversation: req.Conversation}
		switch {
		case err != nil:
			log.Printf("slack POST error: %v", err)
			receipt.Delivered = false
			receipt.FailureKind = "transient"
		case !slackResp.OK:
			log.Printf("slack returned error: %s", slackResp.Error)
			receipt.Delivered = false
			switch slackResp.Error {
			case "channel_not_found", "not_in_channel":
				receipt.FailureKind = "not_found"
			case "invalid_auth", "not_authed", "token_revoked":
				receipt.FailureKind = "auth"
			case "rate_limited":
				receipt.FailureKind = "rate_limited"
			default:
				receipt.FailureKind = "permanent"
			}
		default:
			receipt.Delivered = true
			receipt.MessageID = slackResp.TS
		}
		// Busy-reaction lifecycle, remove side (hq-xizo): a delivered
		// publish into a conversation/thread carrying a pending busy
		// mark means the agent's reply has landed — clear the busy
		// emoji. The dedup-replay path above returns earlier without
		// re-checking: the original delivery already consumed the mark.
		if receipt.Delivered {
			clearBusyReaction(cfg, req.Conversation.ConversationID, req.ReplyToMessageID)
		}
		// Remember delivered receipts so a subsequent retry with the same
		// idempotency key replays this receipt instead of re-posting. Put
		// ignores empty keys and non-delivered receipts.
		dedup.Put(req.IdempotencyKey, receipt)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(receipt)
	}
}

// handlePublishFile serves POST /publish-file. It uploads the file at
// req.FilePath to Slack via the files-upload-v2 protocol and posts it to
// req.Conversation.ConversationID, optionally threaded under
// req.ReplyToMessageID. The bot token requires the `files:write` scope —
// without it, Slack returns {ok: false, error: "missing_scope"} and the
// receipt's FailureKind is "permanent".
//
// Slack's files.completeUploadExternal does NOT accept chat:write.customize
// username/icon overrides, so file posts appear under the default bot
// identity even when an identity record is registered for the source
// session. This is a Slack platform limitation, not an adapter bug.
// The identity lookup still happens for log parity with /publish.
func handlePublishFile(cfg config, reg *identityRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req publishFileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.FilePath) == "" {
			http.Error(w, "file_path is required", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Conversation.ConversationID) == "" {
			http.Error(w, "conversation.conversation_id is required", http.StatusBadRequest)
			return
		}
		// Confinement gate: the adapter only reads files under the
		// configured FILE_UPLOAD_ROOT. Without it, /publish-file is a
		// host-wide arbitrary-read primitive for anyone on the
		// internal mux. Fail-closed when unset rather than silently
		// allowing everything.
		if cfg.fileUploadRoot == "" {
			http.Error(w, "file upload disabled: FILE_UPLOAD_ROOT not configured", http.StatusServiceUnavailable)
			return
		}
		resolvedPath, err := confineFileUploadPath(cfg.fileUploadRoot, req.FilePath)
		if err != nil {
			// Use the request path verbatim in the error so operators
			// can correlate logs without leaking the canonicalized
			// (post-symlink) target.
			http.Error(w, fmt.Sprintf("file_path %q outside FILE_UPLOAD_ROOT: %v", req.FilePath, err), http.StatusForbidden)
			return
		}
		fi, err := os.Stat(resolvedPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("file_path: %v", err), http.StatusBadRequest)
			return
		}
		if fi.IsDir() {
			http.Error(w, "file_path is a directory", http.StatusBadRequest)
			return
		}
		// Symlink escape gate: now that os.Stat confirmed the path
		// exists, resolve symlinks and re-check the in-root invariant
		// so an attacker who plants a symlink inside the root cannot
		// pivot to an arbitrary host file.
		realPath, err := filepath.EvalSymlinks(resolvedPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("file_path: %v", err), http.StatusBadRequest)
			return
		}
		if _, err := confineFileUploadPath(cfg.fileUploadRoot, realPath); err != nil {
			http.Error(w, fmt.Sprintf("file_path %q resolves outside FILE_UPLOAD_ROOT: %v", req.FilePath, err), http.StatusForbidden)
			return
		}
		fileBytes, err := readConfinedFile(cfg.fileUploadRoot, realPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("read file_path: %v", err), http.StatusInternalServerError)
			return
		}
		filename := req.Filename
		if filename == "" {
			filename = filepath.Base(req.FilePath)
		}
		title := req.Title
		if title == "" {
			title = filename
		}

		// Identity lookup: same precedence as /publish. Logged for parity
		// even though Slack's file-upload API ignores chat:write.customize
		// overrides.
		identitySessionID := req.SessionID
		if identitySessionID == "" {
			identitySessionID = req.Metadata[metadataKeySourceSessionID]
		}
		identityApplied := ""
		if reg != nil && identitySessionID != "" {
			if rec, ok := reg.Get(identitySessionID); ok {
				identityApplied = rec.Username
			}
		}
		log.Printf("publish-file: conv=%s file=%s size=%d reply_to=%s session=%s as=%q",
			req.Conversation.ConversationID, filename, len(fileBytes),
			req.ReplyToMessageID, identitySessionID, identityApplied)

		receipt := publishFileReceipt{Conversation: req.Conversation}

		// Step 1: get a pre-signed upload URL.
		urlResp, err := slackGetUploadURL(cfg.slackBotToken, filename, len(fileBytes))
		if err != nil {
			log.Printf("slack files.getUploadURLExternal error: %v", err)
			receipt.FailureKind = "transient"
			receipt.Error = err.Error()
			writeJSON(w, receipt)
			return
		}
		if !urlResp.OK {
			log.Printf("slack files.getUploadURLExternal returned error: %s", urlResp.Error)
			receipt.FailureKind = mapSlackError(urlResp.Error)
			receipt.Error = urlResp.Error
			writeJSON(w, receipt)
			return
		}

		// Step 2: POST bytes (multipart) to the pre-signed URL.
		if err := slackPutFileBytes(urlResp.UploadURL, filename, fileBytes); err != nil {
			log.Printf("slack file upload error: %v", err)
			receipt.FailureKind = "transient"
			receipt.Error = err.Error()
			writeJSON(w, receipt)
			return
		}

		// Step 3: complete the upload — channel posting happens here.
		completeReq := slackCompleteUploadReq{
			Files:          []slackCompleteUploadFile{{ID: urlResp.FileID, Title: title}},
			ChannelID:      req.Conversation.ConversationID,
			InitialComment: req.InitialComment,
			ThreadTS:       req.ReplyToMessageID,
		}
		completeResp, err := slackCompleteUpload(cfg.slackBotToken, completeReq)
		if err != nil {
			log.Printf("slack files.completeUploadExternal error: %v", err)
			receipt.FailureKind = "transient"
			receipt.Error = err.Error()
			writeJSON(w, receipt)
			return
		}
		if !completeResp.OK {
			log.Printf("slack files.completeUploadExternal returned error: %s", completeResp.Error)
			receipt.FailureKind = mapSlackError(completeResp.Error)
			receipt.Error = completeResp.Error
			writeJSON(w, receipt)
			return
		}

		receipt.Delivered = true
		receipt.FileID = urlResp.FileID
		// Busy-reaction lifecycle, remove side: a threaded file upload
		// may be the agent's entire reply — it must clear the pending
		// busy mark exactly like a text publish (hw-94w5k codex r1).
		clearBusyReaction(cfg, req.Conversation.ConversationID, req.ReplyToMessageID)
		writeJSON(w, receipt)
	}
}

// confineFileUploadPath validates that path is inside root and returns
// the cleaned absolute form on success.
//
// Both root and path are canonicalized with filepath.Abs +
// filepath.Clean. Root is additionally run through EvalSymlinks
// (best-effort) so an operator-configured root that is itself a
// symlink (macOS /var → /private/var, etc.) lines up with paths the
// caller later resolves via EvalSymlinks. The path argument is NOT
// EvalSymlinks'd — the caller is responsible for re-invoking this
// helper on the EvalSymlinks-resolved path once os.Stat has confirmed
// existence (handlePublishFile does this to defeat symlink escape).
//
// Returns an error when:
//   - root or path is empty
//   - root is not absolute (a relative root would be silently
//     resolved against the adapter's cwd, which is a footgun for
//     operators who expect FILE_UPLOAD_ROOT to be a fixed prefix)
//   - either path can't be made absolute
//   - cleaned path is equal to root itself (the root is not an
//     uploadable file even when downstream IsDir would later reject it)
//   - cleaned path is not a strict descendant of root
//
// The returned path is the cleaned absolute form, suitable for passing
// to os.Stat / os.ReadFile.
func confineFileUploadPath(root, path string) (string, error) {
	_, pathAbs, _, err := confinedUploadPath(root, path)
	return pathAbs, err
}

// confinedUploadPath is confineFileUploadPath's full-detail form: it
// additionally returns the canonical root and the root-relative path,
// which openBeneath needs for its component walk.
func confinedUploadPath(root, path string) (rootAbs, pathAbs, rel string, err error) {
	if root == "" {
		return "", "", "", errors.New("FILE_UPLOAD_ROOT is empty")
	}
	if !filepath.IsAbs(root) {
		return "", "", "", fmt.Errorf("FILE_UPLOAD_ROOT %q is not absolute", root)
	}
	if strings.TrimSpace(path) == "" {
		return "", "", "", errors.New("path is empty")
	}
	rootAbs, err = filepath.Abs(root)
	if err != nil {
		return "", "", "", fmt.Errorf("resolving root: %w", err)
	}
	rootAbs = filepath.Clean(rootAbs)
	// Best-effort symlink resolution on the root: if the operator
	// configured a symlinked root (e.g. /var on macOS) the canonical
	// form is what later EvalSymlinks calls on a path will return.
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	pathAbs, err = filepath.Abs(path)
	if err != nil {
		return "", "", "", fmt.Errorf("resolving path: %w", err)
	}
	pathAbs = filepath.Clean(pathAbs)
	rel, err = filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return "", "", "", fmt.Errorf("computing relative path: %w", err)
	}
	// Reject the root itself: the helper's contract is "file inside
	// root", and any caller that later treats the returned path as a
	// regular file would otherwise be set up for surprise on
	// directory-typed paths. Anything starting with ".." has escaped.
	// An absolute rel (Windows volume crossing) is also out of bounds.
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", "", fmt.Errorf("path %q is outside root %q", pathAbs, rootAbs)
	}
	return rootAbs, pathAbs, rel, nil
}

// readConfinedFile reads realPath after re-asserting that it lies under
// root, then opens it with openBeneath's component-wise walk so neither
// a leaf symlink nor a parent directory swapped for a symlink in the
// TOCTOU window between the caller's EvalSymlinks resolution and the
// read can redirect the open outside root (gc-cby.10; the parent-swap
// residual race was gpk-1ta4).
//
// realPath should be the filepath.EvalSymlinks-resolved canonical path
// the caller has already verified with confineFileUploadPath; the
// internal re-check makes the safe path the only path so a future call
// site cannot regress arbitrary-read safety by skipping confinement.
func readConfinedFile(root, realPath string) ([]byte, error) {
	rootAbs, _, rel, err := confinedUploadPath(root, realPath)
	if err != nil {
		return nil, err
	}
	f, err := openBeneath(rootAbs, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// writeJSON writes the receipt as a JSON response. Errors during encoding
// are logged but not surfaced — the receipt body is best-effort and the
// caller has the HTTP status anyway.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode error: %v", err)
	}
}

// mapSlackError maps a Slack error code to a publishReceipt failure kind.
// Shared between /publish and /publish-file so the contract is consistent.
func mapSlackError(slackErr string) string {
	switch slackErr {
	case "channel_not_found", "not_in_channel", "file_not_found":
		return "not_found"
	case "invalid_auth", "not_authed", "token_revoked", "missing_scope", "no_permission":
		return "auth"
	case "rate_limited", "ratelimited":
		return "rate_limited"
	case "":
		return ""
	default:
		return "permanent"
	}
}

// slackGetUploadURL calls files.getUploadURLExternal. Slack accepts both
// form-urlencoded body and query string for this endpoint; we use form
// to keep secrets out of access logs. The returned upload_url is a
// pre-signed URL valid for ~10 minutes.
func slackGetUploadURL(token, filename string, length int) (*slackGetUploadURLResp, error) {
	form := url.Values{}
	form.Set("filename", filename)
	form.Set("length", strconv.Itoa(length))
	httpReq, err := http.NewRequest(http.MethodPost,
		slackAPIBase+"/files.getUploadURLExternal",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpResp, err := slackAPIClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var sr slackGetUploadURLResp
	if err := json.Unmarshal(respBody, &sr); err != nil {
		// Do not embed respBody: a truncated/partial response can still
		// carry upload_url — pre-signed token included — and this error
		// reaches both log.Printf and the HTTP receipt body upstream.
		// Status, size, and the unmarshal error (which reports offsets,
		// not content) are enough to diagnose a malformed response.
		// gpk-la1y.
		return nil, fmt.Errorf("decode slack getUploadURLExternal response (status %s, %d bytes): %w",
			httpResp.Status, len(respBody), err)
	}
	return &sr, nil
}

// slackPutFileBytes POSTs the file contents to a pre-signed Slack upload
// URL using multipart/form-data with a single “filename“ field. The URL
// itself encodes auth — no Bearer header needed. Slack returns 200 OK with
// "OK - <bytes>" on success; we treat any non-2xx as a transport failure.
//
// History: an earlier revision used PUT with Content-Type:
// application/octet-stream. Slack accepted the bytes (returns 200 OK) and
// files.completeUploadExternal returned ok:true with a file_id, but the
// resulting file had empty mimetype/filetype and never actually appeared
// in the channel — files.info reported `shares: {}` and conversations.history
// did not contain the post. The pre-signed URL evidently treats the
// multipart-with-filename pattern as the canonical shape; raw PUT silently
// degrades to a "ghost upload" the channel post step can't bind to.
func slackPutFileBytes(uploadURL string, filename string, body []byte) error {
	// uploadURL is a pre-signed, token-bearing URL (see doc above). Every
	// error below reports safeURL — never the raw uploadURL — so its embedded
	// auth never reaches log.Printf or the HTTP receipt at the publishFile
	// handler's sink. This mirrors slackDownloadToFile's redaction; the two
	// sibling upload steps (slackGetUploadURL, slackCompleteUpload) already
	// redact, and this one must too. gpk-la1y.
	safeURL := redactSlackURL(uploadURL)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("filename", filename)
	if err != nil {
		return fmt.Errorf("create multipart form file: %w", err)
	}
	if _, err := part.Write(body); err != nil {
		return fmt.Errorf("write multipart body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, uploadURL, &buf)
	if err != nil {
		return redactTransportError("build upload request to", safeURL, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := slackUploadClient.Do(req)
	if err != nil {
		return redactTransportError("upload POST to", safeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// respBody is untrusted origin content: sanitize control chars
		// (log-line injection) and scrub any reflected URL/token. gpk-la1y.
		return fmt.Errorf("upload POST %s: %s — %s", safeURL, resp.Status, sanitizeSlackErrorBody(respBody))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// slackCompleteUpload calls files.completeUploadExternal with a JSON body.
// Channel posting (and threading via thread_ts) happens here, not in a
// separate chat.postMessage call.
func slackCompleteUpload(token string, req slackCompleteUploadReq) (*slackCompleteUploadResp, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost,
		slackAPIBase+"/files.completeUploadExternal",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpResp, err := slackAPIClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var sr slackCompleteUploadResp
	if err := json.Unmarshal(respBody, &sr); err != nil {
		// Do not embed respBody: completeUploadExternal responses carry
		// full file objects (url_private, url_private_download) whose
		// URLs can bear auth tokens, and this error reaches log.Printf
		// and the HTTP receipt body upstream. gpk-la1y.
		return nil, fmt.Errorf("decode slack completeUploadExternal response (status %s, %d bytes): %w",
			httpResp.Status, len(respBody), err)
	}
	return &sr, nil
}

// handleReact serves POST /react. It maps reactRequest → Slack
// reactions.add, or reactions.remove when the request sets
// remove:true (hq-xizo). Emoji name is forwarded verbatim minus
// surrounding colons (clients can send "eyes" or ":eyes:").
func handleReact(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req reactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		emoji := strings.Trim(req.Emoji, ":")
		if emoji == "" || req.Conversation.ConversationID == "" || req.MessageID == "" {
			http.Error(w, "conversation.conversation_id, message_id, and emoji are required", http.StatusBadRequest)
			return
		}
		// Benign no-op errors mirror each other across the two ops:
		// adding an emoji that is already on the message
		// ("already_reacted") and removing one that is not there
		// ("no_reaction") both leave the message in the requested
		// state, so both count as delivered.
		method, benignErr := "reactions.add", "already_reacted"
		if req.Remove {
			method, benignErr = "reactions.remove", "no_reaction"
		}
		log.Printf("react: op=%s conv=%s ts=%s emoji=%s", method, req.Conversation.ConversationID, req.MessageID, emoji)

		slackResp, err := callSlackReactions(cfg.slackBotToken, method, slackReactionsAddReq{
			Channel:   req.Conversation.ConversationID,
			Name:      emoji,
			Timestamp: req.MessageID,
		})
		receipt := reactReceipt{}
		switch {
		case err != nil:
			log.Printf("slack %s error: %v", method, err)
			receipt.FailureKind = "transient"
		case !slackResp.OK:
			if slackResp.Error == benignErr {
				receipt.Delivered = true
			} else {
				log.Printf("slack %s returned error: %s", method, slackResp.Error)
				switch slackResp.Error {
				case "channel_not_found", "not_in_channel", "message_not_found":
					receipt.FailureKind = "not_found"
				case "invalid_auth", "not_authed", "token_revoked":
					receipt.FailureKind = "auth"
				case "rate_limited":
					receipt.FailureKind = "rate_limited"
				default:
					receipt.FailureKind = "permanent"
				}
			}
		default:
			receipt.Delivered = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(receipt)
	}
}

// clearBusyReaction consumes pending busy mark(s) for a delivered
// reply into conversationID and removes the busy emoji from the
// marked message(s). threadKey is the reply's reply_to_message_id,
// which matches the registry key in both inbound shapes: a
// thread-reply inbound was marked under its thread_ts AND its own ts,
// and a channel-root inbound under its own ts. An EMPTY threadKey is
// the documented default `gc slack reply-current` shape — a
// channel-root reply — and clears every pending mark in the
// conversation (codex r3): the agent answered in-channel, and a busy
// emoji nothing will ever remove is worse than clearing a sibling
// thread's affordance early. Shared by handlePublish and
// handlePublishFile — either reply shape (text or file) is the
// agent's answer (hw-94w5k codex r1).
//
// Each removal waits for its mark's reactions.add to finish first
// (bounded by busyReactionAddWait): a fast reply otherwise races the
// in-flight add, Slack applies the delayed add after the remove, and
// the busy emoji sticks forever. Best-effort and async — failures are
// logged and never affect the caller's receipt.
func clearBusyReaction(cfg config, conversationID, threadKey string) {
	if cfg.busyReaction == "" || conversationID == "" {
		return
	}
	var taken []busyTaken
	if threadKey == "" {
		taken = cfg.busyMarks.takeConversation(conversationID)
	} else {
		taken = cfg.busyMarks.take(conversationID, threadKey)
	}
	for _, tk := range taken {
		go removeBusyReaction(cfg, conversationID, tk)
	}
}

// removeBusyReaction removes the busy emoji from one marked message,
// after its reactions.add has finished (bounded wait — see
// clearBusyReaction). Also used to clean up marks superseded by a
// re-target of the same thread (codex r3). Best-effort: errors are
// logged only, and "no_reaction" is benign — the emoji already came
// off (human removed it, or the add never landed).
func removeBusyReaction(cfg config, channel string, tk busyTaken) {
	if tk.addDone != nil {
		select {
		case <-tk.addDone:
		case <-time.After(busyReactionAddWait):
			log.Printf("busy reaction remove: chan=%s ts=%s: add still in flight after %v, removing anyway", channel, tk.messageTS, busyReactionAddWait)
		}
	}
	resp, err := removeReactionFromSlack(cfg.slackBotToken, slackReactionsAddReq{
		Channel:   channel,
		Name:      cfg.busyReaction,
		Timestamp: tk.messageTS,
	})
	if err != nil {
		log.Printf("busy reaction remove failed: chan=%s ts=%s emoji=%s: %v", channel, tk.messageTS, cfg.busyReaction, err)
		return
	}
	if !resp.OK && resp.Error != "no_reaction" {
		log.Printf("busy reaction remove: chan=%s ts=%s emoji=%s: slack error=%s", channel, tk.messageTS, cfg.busyReaction, resp.Error)
	}
}

// postReactionToSlack calls reactions.add for req over the timeout-bounded
// slackAPIClient. The busy-reaction lifecycle orders its remove after this
// add with a wait derived from slackAPIClient's timeout (hq-xizo), so the
// add must stay on a bounded client.
func postReactionToSlack(token string, req slackReactionsAddReq) (*slackReactionsAddResp, error) {
	return callSlackReactions(token, "reactions.add", req)
}

// removeReactionFromSlack calls reactions.remove for req — same
// request shape, auth, and slackAPIBase as reactions.add (hq-xizo).
func removeReactionFromSlack(token string, req slackReactionsAddReq) (*slackReactionsAddResp, error) {
	return callSlackReactions(token, "reactions.remove", req)
}

// callSlackReactions posts req to the given Slack reactions method
// ("reactions.add" or "reactions.remove") over the timeout-bounded
// slackAPIClient; the two endpoints share a request and response wire
// shape.
func callSlackReactions(token, method string, req slackReactionsAddReq) (*slackReactionsAddResp, error) {
	return postReactionMethod(slackAPIClient, token, method, req)
}

// postReactionMethod is the single Slack reactions POST path, parameterized by
// method ("reactions.add" | "reactions.remove") and HTTP client. The legacy
// paths (handleReact, busy-reaction lifecycle — via callSlackReactions over
// slackAPIClient) and the company visible-ack path (add/remove over the
// gateway's timeout-bounded client) all route through here, so there is no
// second reactions POST implementation.
func postReactionMethod(client *http.Client, token, method string, req slackReactionsAddReq) (*slackReactionsAddResp, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost, slackAPIBase+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var sr slackReactionsAddResp
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return nil, fmt.Errorf("decode slack: %w (body=%s)", err, string(respBody))
	}
	return &sr, nil
}

func postToSlack(token string, req slackPostMessageReq) (*slackPostMessageResp, error) {
	return postMessageWithClient(http.DefaultClient, token, req)
}

// postMessageWithClient is the single Slack chat.postMessage path, parameterized
// by HTTP client. postToSlack (DefaultClient) and the company visible-ack failure
// reply (the gateway's timeout-bounded client) both route through here, so there
// is no second chat.postMessage implementation.
func postMessageWithClient(client *http.Client, token string, req slackPostMessageReq) (*slackPostMessageResp, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest(http.MethodPost, slackAPIBase+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	var sr slackPostMessageResp
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return nil, fmt.Errorf("decode slack: %w (body=%s)", err, string(respBody))
	}
	return &sr, nil
}

func handleSlackEvents(cfg config, aliasReg *handleAliasRegistry, threadReg *threadSessionRegistry, roomLaunchReg *roomLaunchMappingRegistry, subteamMap *subteamAliasMap, threadHandleSticky *threadHandleStickiness) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The shutdown WaitGroup covers this handler from ENTRY — before
		// any state movement, watermark advance, or ack (gp-9e7 round 3,
		// 2b): main's bounded eventWG wait therefore joins every handler
		// that could possibly admit, not just the detached goroutines
		// spawned at the end. The detached event goroutine below takes
		// its own Add before this one is released.
		if cfg.eventWG != nil {
			cfg.eventWG.Add(1)
			defer cfg.eventWG.Done()
		}
		// Shutdown admission barrier (gp-9e7 fix round 2b'): refuse the
		// event BEFORE it is acked to Slack and BEFORE the liveness
		// watermark advances (noteInboundEnvelope below), so the event
		// stays recoverable server-side — the Events API retry ladder /
		// un-acked Socket Mode envelope redelivers it, and anything the
		// ladder gives up on remains above the persisted watermark for
		// the startup backfill. An admission after this point rides the
		// eventWG/closed-gate machinery instead.
		if cfg.draining != nil && cfg.draining.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		// Resolve the signing secret(s) BEFORE HMAC. Body is unsigned bytes by
		// definition until verified, so we parse only the small type +
		// api_app_id + team_id head to pick the Phase 4 verification path (env
		// secret for the switchboard/legacy, the app's OWN secret for a
		// registered agent app, trial for the url_verification handshake).
		head := parseEventHead(body)
		ts := r.Header.Get("X-Slack-Request-Timestamp")
		sig := r.Header.Get("X-Slack-Signature")
		// Resolve the agent-apps registration snapshot ONCE per request so the
		// HMAC decision and the DM admission gate agree even if a SIGHUP swaps
		// the registry between them (m7): a mid-request register/deregister can
		// no longer route an event verified as an agent-app DM into the legacy
		// dispatcher, nor admit a legacy-trial-verified event as an owner DM.
		agentApps := cfg.companyGateway.agentAppsSnapshot()
		// A Socket Mode envelope arrives over the app-level-token
		// WebSocket, not the public listener, and carries no HMAC —
		// the transport itself is the authentication. The socket
		// runner marks its in-process synthetic request as trusted
		// (gp-3og); every network request fails this check and is
		// verified exactly as before.
		if !isTrustedTransportRequest(r) && !verifyInboundEvent(cfg, agentApps, head, body, ts, sig) {
			log.Printf("slack signature verify FAILED type=%q api_app_id=%q team_id=%q",
				clipTeamIDForLog(head.Type), clipTeamIDForLog(head.APIAppID), clipTeamIDForLog(head.TeamID))
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		var env slackEventEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}

		// URL verification challenge.
		if env.Type == "url_verification" && env.Challenge != "" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(env.Challenge))
			return
		}

		// Inbound-liveness bookkeeping (gp-3og): every verified
		// event_callback, on either transport, refreshes the "last
		// inbound" clock and records the message origin so the
		// watchdog and the reconnect backfill can tell a message the
		// adapter already saw from one it missed. A message the
		// backfill already synthesized and delivered is dropped here
		// — the late live copy would otherwise double-deliver. Nil-safe.
		if env.Type == "event_callback" {
			// Re-check the shutdown admission barrier at the ADMISSION
			// POINT (gp-9e7 round 3, 2a): the entry check races the
			// request body read — a handler that passed it can stall in
			// the read while draining flips, then advance the watermark
			// below for an event the drain will never deliver. Refuse
			// pre-ack exactly like the entry check: no watermark move, no
			// 200, so the retry ladder / un-acked socket envelope (or the
			// startup backfill) still owns the event.
			if cfg.draining != nil && cfg.draining.Load() {
				http.Error(w, "shutting down", http.StatusServiceUnavailable)
				return
			}
			if cfg.inboundLiveness.noteInboundEnvelope(env, trustedTransportName(r)) == inboundOriginBackfilled {
				w.WriteHeader(http.StatusOK)
				log.Printf("slack event: dropping live copy of a backfilled message event_id=%s team_id=%q",
					env.EventID, clipTeamIDForLog(env.TeamID))
				return
			}
		}

		// Company-rooms durable admission (Slack company-rooms Phase 1d).
		// When the event targets an imported company room, the gateway
		// owns the HTTP response (200 on admit/duplicate/non-admissible,
		// 503 without x-slack-no-retry on store failure or a closed
		// startup barrier) and delivery proceeds asynchronously. Every
		// other event — no gateway, no directory, non-company channel,
		// non-message type — falls through to the legacy path below
		// byte-for-byte.
		if cfg.companyGateway.tryHandleEvent(w, r, env, agentApps) {
			return
		}

		// Process event_callback. Always 200 quickly to avoid Slack retries.
		w.WriteHeader(http.StatusOK)
		// Dedup Events API redeliveries. Slack retries any delivery it
		// considers unacknowledged (network hiccup, slow first ack, its
		// own read timeout) with the SAME event_id, and every retry that
		// slips through forwards a duplicate notification into the bound
		// session (hw-94w5k finding #4: byte-identical inbound log pairs).
		// The 200 above already acknowledged this delivery, so a drop
		// below is safe. Keyed on event_id only — it is unique per
		// event and stable across retries. Envelopes without an
		// event_id (not event_callback shaped) are never deduped.
		//
		// The claim is taken SYNCHRONOUSLY, before load shedding
		// (codex r9): two simultaneous deliveries of one event both
		// racing an async claim could otherwise each read "unknown",
		// with the loser dropped at the queue-full check — and if the
		// winner's forward then failed, that dropped, already-acked
		// copy was the event's only recovery. With the claim taken
		// here, exactly one delivery owns the event; every other copy
		// either drops (committed) or parks slotless (in flight).
		// Cheap in-memory op — the handler still acks promptly.
		retryNum := r.Header.Get("X-Slack-Retry-Num")
		proceed, wait := cfg.eventDedup.begin(env.EventID)
		if !proceed && wait == nil {
			log.Printf("slack event dedup: dropping redelivery event_id=%s retry_num=%q team_id=%q",
				env.EventID, retryNum, clipTeamIDForLog(env.TeamID))
			return
		}
		var release func()
		if proceed {
			// This delivery owns the event: contend at the nonblocking
			// load-shed. On queue-full the claim is forgotten so a
			// parked or future copy can take over — the event is only
			// lost if no other copy exists, which is the accepted
			// saturation-shedding behavior for unowned events too.
			var capacity int
			var ok bool
			release, capacity, ok = cfg.acquireDispatchSlot()
			if !ok {
				cfg.eventDedup.forget(env.EventID)
				log.Printf("slack adapter: dispatch queue full (cap=%d), dropping slack event type=%q event_id=%q",
					capacity, env.Type, env.EventID)
				return
			}
		}
		// A redelivery that races the FIRST delivery's still-running
		// forward must not be discarded: if that forward then fails,
		// the discarded retry was the message's last chance (Slack got
		// a 200 for it and stops the ladder). It parks — slotless, in
		// a goroutine, so the handler returns and net/http FINISHES
		// the 200 before any waiting happens (codex r3 P2) — until the
		// in-flight claim concludes: commit → drop, forget → take over
		// (blocking-acquiring a fresh slot; codex r5). Parked
		// goroutines cannot leak because every claim concludes
		// (bounded gc forwards + conclude-on-every-return in
		// processSlackEvent).
		//
		// Slot ownership transfers to processSlackEvent, which either
		// releases on its own return path or hands the slot to its
		// alias-dispatch goroutine. This avoids double-counting against
		// cfg.dispatchSem when an inbound triggers an alias dispatch (which
		// would otherwise hold two slots concurrently — see gc-cby.26
		// Phase 4 review fix). The drop paths below release explicitly.
		// Tracked in cfg.eventWG so shutdown can await every detached
		// event goroutine before the coalescer drain — an event still
		// running there can admit to the buffers, and an admission after
		// flushAll would be lost on exit (gp-9e7 fix round 1a). This is
		// the goroutine's OWN count; the handler has held a count of its
		// own since ENTRY (round 3, 2b — before any state movement), so
		// the group covers admission end to end and never touches zero
		// between the handler's entry and the goroutine's Done.
		if cfg.eventWG != nil {
			cfg.eventWG.Add(1)
		}
		go func() {
			if cfg.eventWG != nil {
				defer cfg.eventWG.Done()
			}
			ownsSlot := release != nil
			for !proceed {
				for parked := true; parked; {
					select {
					case <-wait:
						parked = false
					case <-time.After(eventDedupParkLogInterval):
						log.Printf("slack event dedup: redelivery of event_id=%s still parked behind an in-flight delivery (retry_num=%q)",
							env.EventID, retryNum)
					}
				}
				proceed, wait = cfg.eventDedup.begin(env.EventID)
				if !proceed && wait == nil {
					log.Printf("slack event dedup: dropping redelivery event_id=%s retry_num=%q team_id=%q",
						env.EventID, retryNum, clipTeamIDForLog(env.TeamID))
					return
				}
			}
			if !ownsSlot {
				// Taking over after a parked wait: the original slot
				// went back to the pool, so contend for a fresh one —
				// BLOCKING, unlike the handler's entry check. This
				// delivery already got its 200 and may be the event's
				// only remaining copy, so a queue-full drop here would
				// lose it permanently (codex r5). The blocked
				// goroutine holds no slot and every slot holder
				// releases in bounded time, so this always makes
				// progress; new deliveries still shed load at the
				// handler's nonblocking check.
				cfg.dispatchSem <- struct{}{}
				release = func() { <-cfg.dispatchSem }
			}
			processSlackEvent(cfg, aliasReg, threadReg, roomLaunchReg, subteamMap, threadHandleSticky, env, release)
		}()
	}
}

// parseTeamIDFromEventsBody extracts the JSON `team_id` field from a
// Slack /slack/events POST body. The body is unsigned at this point in
// the pipeline, so this is intentionally minimal — no error
// propagation, no full envelope decode. Returns "" on any decode
// failure or missing field; the caller treats "" as "fall through to
// env fallback" inside lookupSigningSecrets.
//
// Body size is already capped upstream at 1 MiB by io.LimitReader.
func parseTeamIDFromEventsBody(body []byte) string {
	var head struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return ""
	}
	return head.TeamID
}

// eventHead is the minimal pre-HMAC view of a /slack/events body: the fields
// that select the Phase 4 verification path. Parsed from unsigned bytes, so it
// carries no trust — it only routes the request to the right secret.
type eventHead struct {
	Type     string `json:"type"`
	APIAppID string `json:"api_app_id"`
	TeamID   string `json:"team_id"`
}

// parseEventHead extracts the type / api_app_id / team_id head. Returns a zero
// value on any decode failure; every downstream branch fails closed on the
// zero value (no api_app_id match, empty candidate list). Body is already
// capped at 1 MiB upstream.
func parseEventHead(body []byte) eventHead {
	var h eventHead
	_ = json.Unmarshal(body, &h)
	return h
}

// verifyInboundEvent implements the Phase 4 verification order (event POSTs).
// Fail-closed at every step; the switchboard/legacy path stays byte-for-byte
// the existing rooms behavior (lookupSigningSecrets). agentApps is the caller's
// once-per-request registration snapshot (m7): the SAME value must be handed to
// tryHandleEvent so verification and admission never disagree across a SIGHUP.
//
//  1. url_verification: no api_app_id in the handshake — trial-HMAC across the
//     env secret (+ any apps.json secret) and ALL registered agent secrets;
//     echo on any match. Side-effect-free, so a trial is acceptable here and
//     ONLY here.
//  2. event_callback, api_app_id == SLACK_APP_ID (the switchboard's own app):
//     verify against the env signing secret ONLY (the rooms path, unchanged).
//     This branch is authoritative and takes precedence over any registered
//     agent record that happens to carry the same api_app_id — the switchboard
//     identity is pinned to the env secret, never a file-registered one. Empty
//     SLACK_APP_ID disables the pin and lets the switchboard fall to rule 4.
//  3. event_callback, api_app_id == a registered agent app: verify against
//     exactly that record's secret. A mismatch — including a signature valid
//     under a DIFFERENT registered app's secret — is a strict-bind reject
//     (401, counter company_dm_sig_reject). The bind check is authoritative;
//     no fallback (rule 12 spoof defense).
//  4. otherwise (an unknown api_app_id): the legacy lookupSigningSecrets path,
//     UNCHANGED — except a legacy trial that matches a secret which is ALSO a
//     registered agent secret is rejected, because registration opts an app
//     into strict binding permanently.
func verifyInboundEvent(cfg config, agentApps *AgentApps, head eventHead, body []byte, ts, sig string) bool {
	if head.Type == "url_verification" {
		candidates := lookupSigningSecrets(cfg.appsRegistry, cfg.slackSigningKey, head.TeamID)
		candidates = append(candidates, agentApps.SigningSecrets()...)
		return verifySlackSignatureMulti(candidates, ts, body, sig)
	}
	// Rule 2: the switchboard's own api_app_id pins to the env secret only. It
	// is checked BEFORE the registered-agent lookup so a registered record
	// sharing SLACK_APP_ID can never shadow the env-secret path (spec §Verify
	// order rule 2/3 precedence). A mismatch here is a plain 401 (the rooms
	// path), not a company_dm_sig_reject.
	if head.Type == "event_callback" && cfg.slackAppID != "" && head.APIAppID == cfg.slackAppID {
		return verifySlackSignature(cfg.slackSigningKey, ts, body, sig)
	}
	if rec, ok := agentApps.Get(head.APIAppID); ok {
		if verifySlackSignature(rec.SigningSecret, ts, body, sig) {
			return true
		}
		// Strict binding: an event claiming a registered app must verify under
		// that app's own secret or be rejected, even if it verifies under some
		// other registered app's secret (the cross-app spoof).
		cfg.companyGateway.recordDMSigReject()
		return false
	}
	// The legacy trial set is the env/apps.json candidates PLUS every registered
	// agent secret, so a match on a registered secret is DETECTED (not silently
	// unmatched) and explicitly rejected: that app opted into strict binding via
	// registration, so it may never be admitted through the unknown-api_app_id
	// carve-out. A match on a non-registered legacy candidate is accepted.
	candidates := lookupSigningSecrets(cfg.appsRegistry, cfg.slackSigningKey, head.TeamID)
	candidates = append(candidates, agentApps.SigningSecrets()...)
	matched, ok := firstMatchingSecret(candidates, ts, body, sig)
	if !ok {
		return false
	}
	if agentApps.isRegisteredSecret(matched) {
		cfg.companyGateway.recordDMSigReject()
		return false
	}
	return true
}

// firstMatchingSecret trials each candidate secret against the HMAC and
// returns the first that verifies (and true). Fail-closed semantics per
// verifySlackSignature. Used by the legacy verification path so the caller can
// inspect WHICH secret matched (the strict-bind rejection above).
func firstMatchingSecret(secrets []string, ts string, body []byte, sig string) (string, bool) {
	for _, s := range secrets {
		if verifySlackSignature(s, ts, body, sig) {
			return s, true
		}
	}
	return "", false
}

// verifySlackSignatureMulti trials each candidate secret against the
// HMAC and returns true on the first match. Each per-secret call
// inherits fail-closed semantics from verifySlackSignature (malformed
// timestamp, stale window, missing headers); sec-S-01 still pins.
//
// Empty candidate list returns false — the natural fail-closed path
// when neither the apps registry nor the env supplies a secret. The
// extra HMAC ops are cheap and bounded by the small number of gc-
// imported apps per workspace; the trial is mechanical (no judgment
// in Go).
func verifySlackSignatureMulti(secrets []string, ts string, body []byte, sig string) bool {
	for _, s := range secrets {
		if verifySlackSignature(s, ts, body, sig) {
			return true
		}
	}
	return false
}

// clipTeamIDForLog bounds attacker-controlled team_id values before they
// hit log lines. Real Slack team IDs are "T" + 8-11 alphanumerics; 32
// is generous. Pre-HMAC body is unsigned, so an unbounded value would
// allow log amplification (~1 MiB per request).
func clipTeamIDForLog(s string) string {
	const max = 32
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func verifySlackSignature(secret, ts string, body []byte, sig string) bool {
	if secret == "" || ts == "" || sig == "" {
		return false
	}
	// Reject stale requests (>5 min) to mitigate replay. Fail closed on
	// any timestamp parse error: an attacker who controls the
	// timestamp header must not be able to bypass the replay window
	// just by sending an unparseable value (e.g. "abc", "1.5").
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(tsInt, 0)) > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + ts + ":"))
	_, _ = mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// slackKindFromChannelType maps a Slack message event's channel_type
// onto a gc ConversationKind. Slack channel_type values are:
//
//	"im"       -> direct message between two users  -> dm
//	"channel"  -> public channel                    -> room
//	"group"    -> private channel                   -> room
//	"mpim"     -> multi-party DM (group DM)         -> room
//
// When channel_type is missing, fall back to the channel-id prefix
// (D=im, C=channel, G=group). Defaults to "dm" for safety.
func slackKindFromChannelType(channelType, channelID string) string {
	switch channelType {
	case "channel", "group", "mpim":
		return "room"
	case "im":
		return "dm"
	}
	if len(channelID) > 0 {
		switch channelID[0] {
		case 'C', 'G':
			return "room"
		case 'D':
			return "dm"
		}
	}
	return "dm"
}

// processSlackEvent runs the per-inbound-event work (signature parse,
// postInbound to gc, optional alias dispatch). It owns the dispatch
// slot supplied by handleSlackEvents: the slot is released either on
// the function's own return path, or — when an alias dispatch fans
// out to its own goroutine — transferred to that goroutine's defer.
// The slot is released exactly once.
//
// threadReg is the launcher-mode binding store (cby.5). It may be nil
// in tests or in deployments that disable launcher mode entirely; the
// `@@<handle>` branch falls through to the regular `@<handle>` path
// when nil.
func processSlackEvent(cfg config, aliasReg *handleAliasRegistry, threadReg *threadSessionRegistry, roomLaunchReg *roomLaunchMappingRegistry, subteamMap *subteamAliasMap, threadHandleSticky *threadHandleStickiness, env slackEventEnvelope, release func()) {
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	// Conclude the dedup claim begun in handleSlackEvents: every path
	// out of this function is a final handling of the event — noise
	// drops included, a redelivery would take the identical path — so
	// the default verdict is commit. The one exception is a failed
	// forward to gc, which flips this off and forgets the id so a
	// redelivery can take over (codex r2 P1). No-op when the envelope
	// carries no event_id or the cache is nil.
	commitDedup := true
	defer func() {
		if commitDedup {
			cfg.eventDedup.commit(env.EventID)
		}
	}()
	if env.Type != "event_callback" || len(env.Event) == 0 {
		return
	}
	var msg slackMessageEvent
	if err := json.Unmarshal(env.Event, &msg); err != nil {
		log.Printf("decode slack event: %v", err)
		return
	}
	// Human emoji reactions forward to the bound session as tagged
	// notifications (gp-by3); the branch terminates here — a reaction
	// is never an ask. Requires the reaction_added/reaction_removed
	// event subscriptions (manifest + live app config).
	if msg.Type == "reaction_added" || msg.Type == "reaction_removed" {
		maybeDeliverReactionEvent(cfg, env)
		return
	}
	if msg.Type != "message" && msg.Type != "app_mention" {
		return
	}
	// Bot-authored messages historically dropped here unconditionally,
	// which made fleet mayors blind to each other's posts (gp-kop). Every
	// bot post that survives the fail-closed author resolution is now
	// delivered as tagged read-only context (gp-9e7 item 3): buffered by
	// default — riding the channel's next real delivery, never a wake —
	// and immediate only for ALLOWLISTED peers granted one (per-entry
	// "wake" or immediate_channels). The peer branch terminates here
	// either way: no target parsing, no busy affordance, no alias
	// dispatch — a bot post is never an ask.
	if msg.BotID != "" || msg.Subtype == "bot_message" {
		maybeDeliverPeerBotMessage(cfg, env, msg)
		return
	}
	// A deletion of a message still sitting in the coalesce buffer must
	// deliver as an explicit "deleted by sender" notice, never as a
	// dangling ts whose text then resolves to nothing (gp-0qw item 3,
	// pc_b334cff7f9c6). Handled before the generic subtype drop below;
	// the branch terminates either way — a deletion event is never
	// routable traffic. Deletions of already-delivered (or never-seen)
	// timestamps are a no-op: the buffered window is the only span this
	// adapter holds a message where the notice can still replace it.
	if msg.Subtype == "message_deleted" {
		if msg.DeletedTS != "" {
			if n := cfg.coalescer.markDeleted(msg.Channel, msg.DeletedTS); n > 0 {
				log.Printf("inbound: chan=%s ts=%s deleted by sender before delivery — %d buffered cop(y/ies) replaced with a deletion notice",
					msg.Channel, msg.DeletedTS, n)
			}
		}
		return
	}
	// Skip system messages. Subtyped messages are system noise
	// (message_changed, message_deleted, channel_join, …) with one
	// exception: "file_share" is a human posting a file and
	// must flow through so downloadSlackFiles can fetch the
	// attachment. Slack delivers file posts both ways — the modern
	// composer emits a plain message event with files[] and no
	// subtype, older/API surfaces emit subtype "file_share" — and
	// dropping the latter silently loses the upload (hw-94w5k #1).
	if (msg.Subtype != "" && msg.Subtype != "file_share") || msg.User == "" {
		return
	}
	// A message with neither text nor files carries nothing to route.
	// Files without a caption are common (a teammate drops a
	// screenshot and says nothing) and must not be discarded.
	if strings.TrimSpace(msg.Text) == "" && len(msg.Files) == 0 {
		return
	}

	// Launcher-mode address parser runs FIRST (cby.5.b). A `@@<handle>`
	// head means "spawn a new thread-bound session" (5.3 wires the
	// spawn) or "the handle is already a long-lived alias — instruct
	// the user to drop one `@`." Either branch terminates here without
	// falling through to postInbound or the single-`@` alias dispatch:
	// the launcher flow is operator-driven control plane, not message
	// transport. The single-`@` parser below only runs on miss, so the
	// existing alias dispatch behavior is unchanged.
	if cfg.handlePrefix != "" && threadReg != nil {
		if h, remainder, ok := parseDoubleHandlePrefix(msg.Text, cfg.handlePrefix); ok {
			// A transient launcher failure (spawn / first-message
			// forward) forgets the dedup claim so a Slack redelivery
			// retries it — same contract as the postInbound and alias
			// failure paths (codex r6). Terminal outcomes (delivered,
			// user-error ephemerals) commit via the defer.
			if !handleDoubleHandleDispatch(cfg, aliasReg, threadReg, roomLaunchReg, msg, env.TeamID, h, remainder) {
				commitDedup = false
				cfg.eventDedup.forget(env.EventID)
			}
			return
		}
	}

	text := msg.Text
	target := ""
	// Slack User Group mentions (beads gpk-2zi + gpk-hmr.2). Slack
	// delivers two shapes for a User Group mention and both must
	// normalize to the same address-by-handle dispatch path:
	//
	//   Labeled:    <!subteam^TEAMID|@handle>   (autocomplete in text)
	//   Unlabeled:  <!subteam^TEAMID>           (event-payload form)
	//
	// Different gating policy per shape, intentional asymmetry:
	//
	//   - LABELED: gated by aliasReg.Get(@handle) — same gate as the
	//     `@handle:` text-prefix path. The label is in the message
	//     itself, so the gate prevents arbitrary in-workspace User
	//     Groups (whose labels happen to look like gc handle names but
	//     have no registered session) from auto-routing.
	//
	//   - UNLABELED: gated by subteamAliasMap.Get(TEAMID) — Slack does
	//     NOT emit a handle label in this shape, so the operator-edited
	//     subteam-aliases.json IS the allowlist. A subteam ID with no
	//     entry in the map falls through to channel fanout. Locked-down
	//     workspaces without the `usergroups:read` scope still work:
	//     the map is populated off-band, no Slack API call is made.
	//
	// Downstream dispatch (the `if target != "" && aliasReg != nil`
	// block below) is unchanged — it still gates the cross-channel
	// session-message POST on aliasReg.Get, so a subteam-ID resolution
	// to a handle with no registered session yields the channel-bound
	// session seeing ExplicitTarget but no alias goroutine firing.
	// That matches the existing `@handle:` text-prefix semantics.
	if h, sid, rest, ok := parseSubteamMentionPrefix(msg.Text); ok {
		if h != "" {
			// Labeled form: preserve gpk-2zi behavior — aliasReg gate.
			if aliasReg != nil {
				if _, aliased := aliasReg.Get(h); aliased {
					target = h
					text = rest
				}
			}
		} else {
			// Unlabeled form: subteamAliasMap is the gate.
			if mappedHandle, mapped := subteamMap.Get(sid); mapped {
				target = mappedHandle
				text = rest
			}
		}
	}
	if target == "" && cfg.handlePrefix != "" {
		if h, rest := parseHandlePrefix(msg.Text, cfg.handlePrefix); h != "" {
			target = h
			text = rest
		}
	}

	// Thread-stickiness: if no explicit target was parsed AND this is a
	// thread reply (msg.ThreadTS is set and points at an earlier message,
	// not at the current message itself), inherit the target from the
	// thread root if a prior alias-dispatch registered one. Lets a human
	// say `@mayor: hi` once at the top of a thread and then keep replying
	// in the thread without re-tagging. An explicit re-tag in the thread
	// reply still wins because this lookup runs only on parser miss.
	if target == "" && threadHandleSticky != nil && msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
		if stickyHandle, ok := threadHandleSticky.Lookup(msg.Channel, msg.ThreadTS); ok {
			target = stickyHandle
			// text is unchanged: the human didn't write a prefix, so we
			// route the full message body — same shape as if they had
			// typed `@<handle>: <body>` from the start.
		}
	}

	// Alias-dispatch turn-dedup (gp-729 item 5): resolve the alias early
	// so the duplicate-suppression verdict can shape the channel copy's
	// text. When the aliased session already holds an active gc binding
	// for this conversation, the extmsg fan-out below IS its delivery —
	// the direct dispatch would land the same message id in that session
	// twice in one turn (once as its own prompt, once as a mid-turn
	// injection). Suppression restores the human's address marker into
	// the forwarded text so addressed-ness survives without the second
	// copy; a binding-lookup error fails open to dispatching both copies
	// (duplicate is overhead, a missed delivery is loss — binding_check.go).
	aliasedSessionID := ""
	aliasSuppressed := false
	if target != "" && aliasReg != nil {
		if sid, ok := aliasReg.Get(target); ok {
			aliasedSessionID = sid
			if sessionBoundToConversation(cfg, sid, msg.Channel) {
				aliasSuppressed = true
				text = "@" + target + ": " + text
				log.Printf("alias dispatch suppressed (turn-dedup gp-729): handle=%s session=%s already bound to chan=%s ts=%s",
					target, sid, msg.Channel, msg.TS)
			}
		}
	}

	// gp-4vq: humans address agents in bound rooms by @-mentioning the
	// adapter's bot user — the `@handle:` prefix syntax the parsers
	// above recognize almost never occurs in real traffic, so gating
	// the busy affordance on a parsed target alone left it effectively
	// invisible (live repro: zero busy lines across a full evening of
	// real mentions). A mention of the adapter's OWN bot user anywhere
	// in the raw text makes the inbound busy-eligible WITHOUT
	// fabricating a target: ExplicitTarget stays empty — a synthetic
	// value would read as "addressed to someone else" to the
	// channel-bound session and mute it — and no alias dispatch fires,
	// so routing is untouched. The bot's user id comes from the
	// envelope's authorizations block; when a delivery omits it, the
	// app_mention event type is itself proof the bot was tagged.
	// Detection runs on msg.Text, not the rewritten text — the
	// hq-uxln9 mention rewrite below replaces `<@U…>` tokens with
	// display names. Computed here, ahead of the thread-context
	// preamble, because the channel-claim gate below needs it.
	botMentioned := msg.Type == "app_mention" ||
		slackTextMentionsUser(msg.Text, env.botUserID())
	convKind := slackKindFromChannelType(msg.ChannelType, msg.Channel)
	// willBuffer mirrors the coalescer-branch condition below: buffered
	// chatter takes no per-ts claim here — its claim happens at batch-
	// delivery time (deliverCoalescedBatch).
	willBuffer := cfg.coalescer.enabled() && target == "" && !botMentioned && convKind == "room"

	// Channel-audience claim (gp-ios, pc_c920ff5fe90c live shape): a
	// bot-mention twin pair races BOTH copies down this urgent path
	// concurrently — distinct event_ids defeat the event dedup, and the
	// deliveredIDs check can't see a twin whose POST hasn't concluded.
	// Exactly one twin owns the (channel, ts) channel delivery; the
	// other parks (bounded by the owner's single gc forward — it holds
	// its dispatch slot while parked, an accepted cost for a rare
	// same-ts race) and then either continues with the channel copy
	// SKIPPED (owner committed — see skipChannelPost consumers below;
	// the alias-dispatch leg still runs under its own claim so a
	// targeted twin can recover a failed injection, codex r1 P1) or
	// takes over as the owner (owner forgot: its POST failed and this
	// copy is the retry). Claimed BEFORE the thread-context preamble so
	// a losing twin never advances threadContextCache — pre-claim
	// marking let a bare twin win the race while the decorated copy was
	// skipped, silently dropping the thread context (codex r1 P2).
	skipChannelPost := false
	var claimKey string
	if !willBuffer {
		claimKey = channelDeliveryClaimKey(msg.Channel, msg.TS)
		claimProceed, claimWait := cfg.channelClaims.begin(claimKey)
		for !claimProceed {
			if claimWait == nil {
				// Concluded by the twin that owned it. During a drain
				// that conclusion can mean "spooled for startup replay"
				// rather than "delivered", and saying so is the whole
				// point of this bead — the 8/27-28 incidents were read
				// off exactly this line.
				if cfg.draining != nil && cfg.draining.Load() {
					log.Printf("inbound: chan=%s ts=%s channel copy concluded by same-ts twin during the shutdown drain (delivered, or spooled for startup replay) — skipping channel post", msg.Channel, msg.TS)
				} else {
					log.Printf("inbound: chan=%s ts=%s channel copy already delivered by same-ts twin — skipping channel post", msg.Channel, msg.TS)
				}
				skipChannelPost = true
				break
			}
			log.Printf("inbound: chan=%s ts=%s parked behind in-flight same-ts twin delivery", msg.Channel, msg.TS)
			<-claimWait
			// Takeover is not a shutdown strategy (gp-32q). A twin never
			// has to decide that for itself, though: an owner that fails
			// during the drain CONCLUDES this claim when its copy
			// reached the spool and RELEASES it when nothing durable
			// exists, so waking to a released claim always means the
			// takeover below is the last recovery path — during a drain
			// as much as outside one.
			claimProceed, claimWait = cfg.channelClaims.begin(claimKey)
		}
	}

	// threadCtxPrevTS/threadCtxAdvanced remember the watermark this
	// delivery moved, so a delivery that then fails can put it back
	// (codex r4 P2 #5). Left advanced, the successor would read this ts
	// as context already conveyed and send its recovery copy without
	// the preamble that never verifiably arrived.
	threadCtxPrevTS := ""
	threadCtxAdvanced := false

	// gc-px8.5 + gc-px8.6: prepend thread-context preamble for inbounds
	// that are replies in a thread. The cache stores per-(target,
	// channel, thread) the ts of the most recent preamble already
	// delivered to that target. Each inbound fetches the thread's
	// reply chain (option B) and the formatter applies the cached ts
	// as a lower bound, so:
	//   - First mention of agent X: full priors window (matches gc-px8.5).
	//   - Subsequent mention of X with peer activity since the last
	//     visit: only the delta — what other bound agents (or human
	//     posts) added between visits — gets prepended (gc-px8.6).
	//   - Subsequent mention of X with no new activity: empty preamble.
	// Errors leave the cached ts unchanged so a transient failure
	// retries on the next inbound rather than silently losing context.
	// Skipped when the channel copy is (skipChannelPost): a skipping
	// twin delivers nothing that could carry the preamble, and marking
	// the cache without delivering would poison the delta for the next
	// real visit (codex r1 P2).
	//
	// KNOWN GAP, pre-existing and NOT closed here (codex r5 P2 #4): a
	// twin that skips the channel copy can still own the alias
	// injection, and that injection then carries the body with no
	// preamble in front of it. gp-32q adds one more route to it — the
	// successor of a drain spill that committed the channel claim but
	// left an injection owed. Closing it means resolving the alias claim
	// BEFORE this block so context is built whenever the event owns
	// either audience, which reorders two claim lifecycles in this
	// function; that is its own change, not a rider on a delivery-claim
	// fix. The failure mode is a thinner injection, never a missing or
	// truncated one.
	isThreadReply := msg.ThreadTS != "" && msg.ThreadTS != msg.TS
	preamble := ""
	parentAuthor, parentFirstLine := "", ""
	if !skipChannelPost && isThreadReply && cfg.threadContextCache != nil {
		sinceTS := cfg.threadContextCache.lastDeliveredFor(target, msg.Channel, msg.ThreadTS)
		fetchCtx, cancel := context.WithTimeout(context.Background(), threadContextFetchTimeout)
		replies, err := fetchThreadReplies(fetchCtx, cfg.slackBotToken, msg.Channel, msg.ThreadTS, cfg.slackThreadContextLimit)
		cancel()
		if err != nil {
			log.Printf("thread context fetch failed chan=%s thread=%s target=%q: %v", msg.Channel, msg.ThreadTS, target, err)
		} else {
			resolveName := func(id string) string { return resolveUserDisplayName(cfg, id) }
			// Parent context for the thread-reply anchor (gp-0qw item 2):
			// the thread root is the reply whose ts equals the thread ts.
			// Best-effort — a root outside the fetched window (or a failed
			// fetch) leaves the anchor with the thread ts alone.
			for _, m := range replies {
				if m.TS == msg.ThreadTS {
					if m.User != "" {
						parentAuthor = resolveName(m.User)
					}
					parentFirstLine = rewriteSlackUserMentions(cfg, m.Text)
					break
				}
			}
			// Priors this audience already received as their own
			// inbounds collapse to a one-line note instead of a
			// re-quote (gp-729 item 2). Exact-key lookup only: a
			// handle's aliased session may not be channel-bound, so
			// channel-audience history must never suppress its context.
			// Buffered-but-undelivered ids count as delivered for the
			// channel audience — they are guaranteed to land in the
			// same or an earlier delivery than this message (a parent
			// still in the coalesce buffer must not be re-quoted in
			// its own reply's preamble).
			alreadyDelivered := func(ts string) bool {
				if cfg.deliveredIDs.seen(target, msg.Channel, ts) {
					return true
				}
				return target == "" && cfg.coalescer.pendingContains(msg.Channel, ts)
			}
			preamble = formatThreadContextPreamble(replies, msg.TS, sinceTS, resolveName, alreadyDelivered)
			threadCtxPrevTS, threadCtxAdvanced = sinceTS, true
			cfg.threadContextCache.markDelivered(target, msg.Channel, msg.ThreadTS, msg.TS)
		}
	}

	// Inline `<@U…>` mentions (including any carried in by the
	// thread-context preamble above) render as raw ids in the session
	// reminder — rewrite them to display names via the users.info
	// cache (hq-uxln9). Runs AFTER target parsing so address tokens
	// are untouched, and before the inbound struct is built so the
	// alias-dispatch copy inherits the rewrite. The preamble is kept
	// separate from the body from here on (gp-0qw): the channel-copy
	// composer needs the bare body to enforce its head-protection
	// contract, while the alias copy and the gc envelope keep the
	// legacy preamble+body concatenation byte-for-byte.
	text = rewriteSlackUserMentions(cfg, text)
	if preamble != "" {
		preamble = rewriteSlackUserMentions(cfg, preamble)
	}
	textWithPreamble := preamble + text
	// Thread replies lead with a protected anchor naming the thread ts
	// and the parent's first line (gp-0qw item 2): the 8/26 incident
	// delivered a thread reply the reader could not place — the approval
	// it carried went unread for ~50 minutes. Computed once here so the
	// buffered copy carries the same anchor the immediate path renders.
	anchor := ""
	if isThreadReply {
		anchor = formatThreadReplyAnchor(msg.ThreadTS, parentAuthor, parentFirstLine)
	}

	var attachments []externalAttachment
	if len(msg.Files) > 0 {
		attachments = downloadSlackFiles(cfg, msg.Channel, msg.TS, msg.Files)
	}
	// Channel-binding delivery renders through gc's extmsg pipeline,
	// which surfaces only the message text to the bound session: the
	// Attachments field never reaches the session's reminder, and an
	// image-only message (empty text) produced no reminder at all
	// (ci-f6x0 / gp-gdo). Fold a file-description block into the text
	// forwarded via postInbound so attachment-bearing messages always
	// generate a reminder that names every file and carries the spooled
	// local path when the download succeeded. Built from msg.Files, not
	// the downloaded subset — a failed download (or unset
	// INBOUND_FILE_STORE) must still surface the file id/name rather
	// than vanish. The alias dispatch below keeps the un-augmented
	// text: dispatchToAliasedSession renders its own attachments block
	// and augmenting both would double-list the files.
	filesBlock := formatInboundFilesBlock(msg.Files, attachments)
	textForChannel := textWithPreamble
	if filesBlock != "" {
		if strings.TrimSpace(textForChannel) == "" {
			textForChannel = filesBlock
		} else {
			textForChannel += "\n\n" + filesBlock
		}
	}

	inbound := externalInboundMessage{
		ProviderMessageID: msg.TS,
		Conversation: conversationRef{
			ScopeID:        cfg.cityName,
			Provider:       cfg.provider,
			AccountID:      cfg.accountID,
			ConversationID: msg.Channel,
			Kind:           convKind,
		},
		Actor: externalActor{
			ID:          msg.User,
			DisplayName: resolveUserDisplayName(cfg, msg.User), // raw id on lookup failure (hq-uxln9)
			IsBot:       false,
		},
		Text:             textWithPreamble,
		ExplicitTarget:   target,
		ReplyToMessageID: msg.ThreadTS,
		Attachments:      attachments,
		DedupKey:         "slack-" + msg.TS,
		ReceivedAt:       time.Now().UTC(),
	}
	// botMentioned/convKind/willBuffer were computed ahead of the
	// thread-context preamble (the channel-claim gate needs them).
	//
	// Slack delivers a bot mention TWICE (message + app_mention,
	// distinct event_ids, same ts — hw-vzd5y edge case 2, live repro
	// in gp-4vq), so event_id dedup does not collapse the pair. Both
	// deliveries compute the same busy verdict and mark the same ts:
	// markBoth's same-message branch MERGES (one registry entry, both
	// addDone channels chained) rather than displacing, and the
	// second reactions.add is Slack's benign already_reacted — no
	// double-mark, no double-remove, no stranded emoji. Each delivery
	// still fires its own add after its own successful forward (a
	// skipChannelPost twin counts its twin's committed POST as that
	// success), so one delivery failing cannot suppress the survivor's
	// affordance. The CHANNEL copy itself is single-delivery via the
	// per-(channel, ts) claim above (gp-ios).

	// Busy-reaction lifecycle, mark registration (hq-xizo; replaces the
	// earlier unconditional "eyes" reaction). The reaction signals to
	// the human that an agent was explicitly addressed (via `@handle:`
	// prefix, a Slack User Group mention resolved via subteamAliasMap,
	// or an @-mention of the adapter's own bot user — gp-4vq)
	// and is working on the message; handlePublish removes it when the
	// agent's reply lands in the same conversation/thread — the
	// channel-native replacement for Slack Assistant-mode
	// assistant.threads.setStatus, which this adapter deliberately does
	// not use. Only fires when a target was parsed or the bot itself
	// was tagged — generic channel chatter that merely lands on the
	// bound session via postInbound does NOT trigger it, because most
	// channel messages aren't intentionally directed at an agent.
	//
	// The MARK is recorded BEFORE the forward (codex r4): gc can hand
	// the inbound to the agent — and the agent can /publish a reply —
	// before postInbound's response even returns here, and a reply
	// that finds no mark would leave the subsequently-added emoji
	// stuck forever. The reactions.add itself still fires only after
	// the forward succeeds (an emoji on a message no agent received
	// would be a lie); a failed forward cancels the mark. A mark whose
	// reply never arrives expires after busyReactionTTL. If alias
	// dispatch later fails, reactAliasDispatchFailure posts ⚠️ on the
	// same TS — semantically distinct (busy affordance vs. delivery
	// failure). Best-effort: errors are logged and don't block the
	// dispatch path. BUSY_REACTION= (set-but-empty) disables all of
	// this — no reaction, no mark.
	// Burst coalescing (gp-729 item 1): untargeted, non-bot-mentioned
	// channel chatter buffers for the debounce window (or the channel's
	// digest interval, item 6) and delivers as ONE inbound. Targeted or
	// bot-mentioned messages keep the exact busy/alias/dedup flow below,
	// with any pending buffer flushed AHEAD of them so ordering holds.
	// Admission to the buffer is final handling of the event — the ack
	// already went to Slack, the dedup claim commits via the defer, and
	// a failed flush retries from the coalescer's own timer rather than
	// via Slack redelivery.
	// Rooms only: DMs/MPIMs are direct conversations where latency
	// matters more than wrapper overhead — they keep the immediate path.
	if willBuffer {
		// A ts the channel audience already received is the trailing
		// half of a bot-mention pair (message + app_mention, same ts)
		// whose urgent twin delivered first. Buffering it would hand
		// the session the same message id again inside a batch whose
		// dedup key gc cannot correlate with the urgent copy's
		// "slack-<ts>" (pc_c920ff5fe90c). Skipping is final handling
		// exactly like buffer admission: the ack went to Slack and the
		// dedup claim commits via the defer.
		if cfg.deliveredIDs.seen("", msg.Channel, msg.TS) {
			log.Printf("inbound: chan=%s ts=%s already delivered to channel audience — twin skipped", msg.Channel, msg.TS)
			return
		}
		inboundForChannel := inbound
		inboundForChannel.Text = textForChannel
		// The parts ride alongside the folded Text so a single-entry
		// delivery re-composes under the head-protection contract
		// exactly like the immediate path (gp-0qw).
		cfg.coalescer.enqueue(msg.Channel, pendingChannelInbound{
			inbound:      inboundForChannel,
			threadAnchor: anchor,
			preamble:     preamble,
			body:         text,
			files:        filesBlock,
		})
		return
	}
	// withheldTwins: buffered copies of THIS message's ts, held back from
	// the flushed batch (pc_c920ff5fe90c). Restored below if the urgent
	// channel post fails — they were already acked to Slack when
	// buffered, so dropping them on failure would lose the message. A
	// skipChannelPost twin still flushes pending chatter ahead (ordering
	// holds for its alias leg); its same-ts withheld copies are already
	// delivered by the committed twin, so they simply drop.
	//
	// Buffered no-wake reactions do NOT deliver here (gp-9e7 fix round
	// 1b): a take with no real message in it stays in the side-buffer —
	// this message's own POST can be skipped (skipChannelPost) or fail,
	// and a reaction batch posted ahead of it would then be the only
	// inbound: a solo reaction wake. They drain via
	// deliverBufferedReactions after the POST below commits.
	withheldTwins := cfg.coalescer.flushAheadOf(msg.Channel, msg.TS)

	// A twin whose channel copy was skipped while the drain is running
	// takes no busy mark (gp-32q, codex r3 P2 #3). The mark's whole
	// lifecycle is in-memory: it is cleared by the reply to a delivery
	// this goroutine is no longer making, and the registry that could
	// remove the reaction does not survive the restart — so marking here
	// leaves a permanent hourglass on a message the next startup will
	// replay and answer normally.
	busyEligible := (target != "" || botMentioned) && cfg.slackBotToken != "" && cfg.busyReaction != "" &&
		!(skipChannelPost && cfg.draining != nil && cfg.draining.Load())
	var busyAddDone chan struct{}
	var busyDisplacedMarks []busyDisplaced
	if busyEligible {
		busyAddDone, busyDisplacedMarks = cfg.busyMarks.markBoth(msg.Channel, msg.ThreadTS, msg.TS)
	}

	if !skipChannelPost {
		// The undecorated channel text, kept for the drain-time spool
		// fallback below (gp-9e7 round 3, 2d): peer context and the
		// reply how-to are restored on failure and must decorate the
		// replayed delivery instead of being baked into the spool twice.
		baseTextForChannel := textForChannel
		// Buffered peer-bot context (gp-kop): pending allowlisted peer
		// posts for this channel ride ahead of the message that
		// naturally woke the bound session. Channel copy only — the
		// alias-dispatch copy below targets a session that may not be
		// bound to this channel at all. Drained here, restored on
		// forward failure so a Slack redelivery re-flushes the same
		// context.
		peerItems, peerDropped := cfg.peerContext.flush(msg.Channel)
		peerBlock := formatPeerContextBlock(peerItems, peerDropped)
		// Once-per-channel full reply how-to (gp-729 item 3): the
		// per-message reminder carries only the registered one-line
		// template, so the first delivery for a channel this adapter
		// lifetime appends the long form (and the channel's name+id
		// pairing, item 4).
		firstHelp := cfg.replyHelp.first(msg.Channel)
		helpBlock := ""
		if firstHelp {
			helpBlock = replyHelpBlock(cfg, msg.Channel)
		}
		// Head-protected composition (gp-0qw + gp-9gc): the message unit
		// leads and boilerplate attaches only inside the reminder budget
		// — see reminder_budget.go for the contract. Optional blocks the
		// composer withheld unwind their side effects here so they ride
		// a later delivery instead of being lost.
		composed, usedPeer, usedHelp, usedPreamble, trimmed := composeChannelReminderText(channelReminderParts{
			anchor:    anchor,
			preamble:  preamble,
			body:      text,
			files:     filesBlock,
			ts:        msg.TS,
			channelID: msg.Channel,
		}, peerBlock, helpBlock, cfg.reminderTextBudget)
		textForChannel = composed
		if firstHelp && !usedHelp {
			cfg.replyHelp.unmark(msg.Channel)
			firstHelp = false
			log.Printf("inbound: chan=%s ts=%s reply how-to withheld — delivery over reminder budget %d; re-arms for a smaller delivery (gp-9gc)",
				msg.Channel, msg.TS, cfg.reminderTextBudget)
		}
		if peerBlock != "" && !usedPeer {
			cfg.peerContext.restore(msg.Channel, peerItems, peerDropped)
			peerItems, peerDropped = nil, 0
			log.Printf("inbound: chan=%s ts=%s peer context withheld — delivery over reminder budget %d; restored to ride the next delivery",
				msg.Channel, msg.TS, cfg.reminderTextBudget)
		}
		if preamble != "" && !usedPreamble {
			log.Printf("inbound: chan=%s ts=%s thread-context preamble omitted — delivery over reminder budget %d; thread reachable via the anchor's --thread-ts",
				msg.Channel, msg.TS, cfg.reminderTextBudget)
		}
		if trimmed {
			log.Printf("inbound: chan=%s ts=%s body tail trimmed to reminder budget %d — head (anchor + first %d runes) preserved, marker names the full text",
				msg.Channel, msg.TS, cfg.reminderTextBudget, reminderBodyHeadRunes)
		}

		inboundForChannel := inbound
		inboundForChannel.Text = textForChannel
		receipt, postErr := postInboundWithReceipt(cfg, inboundForChannel)
		verdict := receipt.verdict(cfg.deliveryReceiptGate)
		// Delivery-receipt gate (gp-32q): gc accepting the payload is
		// not the same as the session receiving it — gc's inbound
		// handler notifies members in the background and answers 200
		// first, so a mangled last hop looks identical to a clean one
		// from here. When gc vouches for nothing, re-post ONCE in place
		// (the claim is still held, so a parked twin cannot duplicate
		// the attempt) and re-read the verdict. Only if that also fails
		// to vouch does this fall through to the failure path below,
		// which releases the claim so the parked twin re-posts.
		for attempt := 0; postErr == nil && verdict == receiptUnconfirmed && attempt < deliveryReceiptRepostAttempts && receiptRepostAllowed(cfg); attempt++ {
			log.Printf("inbound: chan=%s ts=%s gc did not vouch for delivery (%s) — re-posting in place (attempt %d/%d)",
				msg.Channel, msg.TS, receipt.logField(verdict), attempt+1, deliveryReceiptRepostAttempts)
			receipt, postErr = postInboundWithReceipt(cfg, inboundForChannel)
			verdict = receipt.verdict(cfg.deliveryReceiptGate)
		}
		if postErr == nil && verdict == receiptHeld {
			// gc took the payload and is still waiting for the session
			// to reach an idle boundary (mayor ruling, 2026-08-28). The
			// claim concludes and NOTHING is re-posted; this line exists
			// so a message that never arrives after a hold is still
			// greppable against gc's own log for the same receipt id.
			log.Printf("inbound: HELD chan=%s ts=%s gc accepted the payload and has not finished delivering it — not re-posting (a busy session's normal path) — %s",
				msg.Channel, msg.TS, receipt.logField(verdict))
		}
		if postErr == nil && verdict == receiptUnconfirmed {
			// The payload reached gc's transcript but nothing vouches
			// that it reached the SESSION. Treated exactly like a failed
			// POST: that path already releases the claim (so a parked
			// twin re-posts — the recovery this bead is about), puts the
			// withheld buffered copies back, cancels the busy mark no
			// reply will ever clear, and spools during a drain. gc
			// dedups a re-posted transcript record by
			// provider_message_id, so the twin's copy costs a duplicate
			// notification at worst — the trade this codebase always
			// takes over loss.
			log.Printf("inbound: UNDELIVERED chan=%s ts=%s gc accepted the payload but did not vouch that it reached the session after %d re-post(s) — %s",
				msg.Channel, msg.TS, deliveryReceiptRepostAttempts, receipt.logField(verdict))
			postErr = errDeliveryUnvouched
		}
		if err := postErr; err != nil {
			log.Printf("inbound POST failed: %v", err)
			// Failed urgent/DM delivery DURING THE SHUTDOWN DRAIN (gp-9e7
			// round 3, 2d): this event is already past the watermark and
			// Slack got its 200 long ago, so the spool is its durability,
			// exactly like coalesced residue. It replays through the
			// coalescer's normal buffers at the next startup.
			//
			// The spool runs BEFORE anything is released (gp-32q, codex
			// r3 P1). Whether this copy became durable is what decides
			// how the claim and the event id conclude below, and
			// releasing first would wake a parked twin into a decision
			// this goroutine has not made yet.
			drainSpooled := false
			draining := cfg.draining != nil && cfg.draining.Load()
			if draining {
				spoolCopy := inbound
				spoolCopy.Text = baseTextForChannel
				// Parts ride the spool line so the replayed delivery
				// keeps the head-protection contract and the parent
				// anchor (codex round-2 finding 4); a deletion already
				// processed must be spooled as its notice — tombstones
				// do not survive the restart (round-3 finding 3).
				spoolEntry := []pendingChannelInbound{{
					inbound:      spoolCopy,
					threadAnchor: anchor,
					preamble:     preamble,
					body:         text,
					files:        filesBlock,
				}}
				cfg.coalescer.applyDeletionTombstones(msg.Channel, spoolEntry)
				drainSpooled = cfg.inboundSpool.spillBatch(msg.Channel, spoolEntry)
				if drainSpooled {
					log.Printf("inbound: chan=%s ts=%s failed during shutdown drain — spooled for startup replay", msg.Channel, msg.TS)
				} else {
					log.Printf("inbound: LOSS chan=%s ts=%s failed during shutdown drain and could not be spooled — LOST (already acked to Slack; the watermark backfill cannot recover admitted events)", msg.Channel, msg.TS)
				}
			}
			// EVERY handover to a successor now happens below the
			// cleanup, not above it (gp-32q, codex r4 P2 #2). Releasing
			// the claim first — which is what this branch used to do —
			// wakes a parked twin that can call markBoth before this
			// goroutine's cancelBoth runs, and the late cancel then
			// deletes the RETRY's fresh mark and strands its hourglass.
			// The same ordering makes the restored riders visible before
			// any successor composes.
			cfg.peerContext.restore(msg.Channel, peerItems, peerDropped)
			if drainSpooled {
				// The withheld copies are same-ts twins of the message
				// just written to the spool (flushAheadOf withholds by
				// ts). Putting them back would spill a SECOND durable
				// copy of it through the coalescer's closed gate, and
				// the restart would notify the session twice (codex r4
				// P2 #3).
				if len(withheldTwins) > 0 {
					log.Printf("inbound: chan=%s ts=%s %d withheld buffered twin(s) dropped — the spooled copy is their replay", msg.Channel, msg.TS, len(withheldTwins))
				}
			} else {
				// The urgent copy never reached gc, so the withheld
				// buffered twins are the only remaining path for this ts
				// — put them back for the coalescer's timer retry
				// (pc_c920ff5fe90c).
				cfg.coalescer.restore(msg.Channel, withheldTwins)
			}
			if firstHelp {
				cfg.replyHelp.unmark(msg.Channel)
			}
			// The thread-context watermark was advanced for a preamble
			// that has not verifiably arrived (codex r4 P2 #5). Left
			// advanced, the successor reads this ts as already conveyed
			// and sends the recovery copy WITHOUT the context — a
			// recovery that is complete in body and short of the
			// decision-making the reader needs. Rolled back only while
			// the entry still names this ts, so a newer delivery keeps
			// its own advance; the worst case is one duplicated
			// preamble, which is the direction this cache already errs
			// on eviction.
			if threadCtxAdvanced {
				cfg.threadContextCache.rollbackDelivered(target, msg.Channel, msg.ThreadTS, msg.TS, threadCtxPrevTS)
			}
			// Nothing reached gc. Cancel the busy mark — no agent
			// received the message, so no reply will ever come to clear
			// it — restoring any marks it displaced (their agents may
			// still be working, codex r5).
			if busyEligible {
				// Marks whose thread a reply already consumed cannot be
				// restored (tombstoned) — remove their reactions instead
				// (codex r8).
				for _, tk := range cfg.busyMarks.cancelBoth(msg.Channel, msg.ThreadTS, msg.TS, busyAddDone, busyDisplacedMarks) {
					go removeBusyReaction(cfg, msg.Channel, tk)
				}
			}
			// Cleanup is settled: NOW hand the (channel, ts) claim to a
			// parked same-ts twin (or the restored buffered copy) as the
			// retry path (gp-ios) — UNLESS the drain spool made this copy
			// durable. Then the claim is CONCLUDED instead: a takeover
			// would post a second copy of a message the next startup
			// already replays, and its POST can burn the full forward
			// timeout, outliving both the 20s event drain and the spool
			// seal, at which point it can neither spool its own failure
			// nor conclude its claim (codex r3 P1). A spill the spool
			// REFUSED is the opposite case: nothing durable exists, so
			// the twin is the last recovery path and must be woken.
			if drainSpooled {
				cfg.channelClaims.commit(claimKey)
			} else {
				cfg.channelClaims.forget(claimKey)
			}
			// The event id concludes on the same evidence as the claim
			// (codex r3 P2 #4) — with one audience the spool does not
			// cover (codex r4 P1 #1). A spooled line replays as a
			// CHANNEL copy; nothing reconstructs an addressed session
			// injection, and this event's channel copy carries an
			// ExplicitTarget telling the channel-bound session to stay
			// silent. So when an injection is still owed, the EVENT is
			// not fully handled however durable the channel copy is:
			// the id is forgotten so a redelivery can still run that
			// leg, while the committed claim above keeps any successor
			// from re-posting the channel copy the spool already holds.
			if drainSpooled && !(aliasedSessionID != "" && !aliasSuppressed) {
				return
			}
			commitDedup = false
			cfg.eventDedup.forget(env.EventID)
			return
		}
		log.Printf("inbound: chan=%s user=%s ts=%s thread=%s target=%q bot_mention=%t files=%d spooled=%d text=%dch posted=%dch %s",
			msg.Channel, msg.User, msg.TS, msg.ThreadTS, target, botMentioned, len(msg.Files), len(attachments), len(textWithPreamble), len(textForChannel), receipt.logField(verdict))
		// Conclude the (channel, ts) claim: a parked same-ts twin now
		// continues with its channel copy skipped instead of re-posting
		// it (gp-ios).
		//
		// Delivery-honesty contract (gp-0qw, tightened by gp-32q): this
		// commit — and the deliveredIDs record below — fires ONLY after
		// postInbound accepted a payload carrying the complete message
		// unit (or an explicitly-marked trim; see reminder_budget.go)
		// AND the receipt gate above concluded. A twin that later skips
		// on this claim is therefore skipping a delivery gc vouched
		// for, not merely one gc acknowledged.
		//
		// The last hop is what gp-0qw could not vouch for: gc's inbound
		// handler runs the member notification in the background and
		// swallows per-member nudge errors (gascity internal/api/
		// huma_handlers_extmsg.go runBackground → extmsgNotifyMembers),
		// so before receipts "gc accepted" was the strongest available
		// signal — and the 8/27 22:57Z and 8/28 04:08Z fragments were
		// both mangled on exactly that hop while this claim reported
		// success (pc_2e2378b9918e). A gc that emits no receipt still
		// lands here on the unsupported verdict, which is that same old
		// behavior, named in the log line above rather than assumed.
		cfg.channelClaims.commit(claimKey)
		cfg.deliveredIDs.record("", msg.Channel, msg.TS)
		if aliasSuppressed {
			// The channel copy IS the handle audience's delivery when the
			// direct dispatch is suppressed; record it under the handle key
			// so sticky thread replies dedup their preambles too.
			cfg.deliveredIDs.record(target, msg.Channel, msg.TS)
		}
		// Buffered no-wake reactions piggyback on this committed
		// delivery's wake (gp-9e7 fix round 1b): drained only AFTER the
		// real POST succeeded, so a failed or skipped delivery leaves
		// them in the side-buffer for the channel's next real moment.
		cfg.coalescer.deliverBufferedReactions(msg.Channel)
	}

	// Busy-reaction lifecycle, add side: fires once per inbound (the
	// alias-dispatch fanout below targets the same Slack TS, so a
	// duplicate react would be a Slack no-op). Marks displaced by this
	// re-target are cleaned up only after the FINAL delivery for this
	// event succeeded — postInbound for plain channel routing, the
	// alias POST when an alias dispatch fires (codex r3 + r5 + r7):
	// their messages still wear the busy emoji and nothing else would
	// ever remove it, but removing before the event's outcome is
	// known could strip an affordance whose agent is still working.
	// The affordance moves to the newest targeted message. See the
	// displaced-cleanup dispatch after the alias block.
	if busyEligible {
		go func(channel, ts, emoji string, addDone chan struct{}) {
			// Closing addDone releases any remove waiting to run
			// after this add (clearBusyReaction orders remove-after-
			// add so a fast reply cannot leave a stale busy emoji).
			defer close(addDone)
			_, err := postReactionToSlack(cfg.slackBotToken, slackReactionsAddReq{
				Channel:   channel,
				Name:      emoji,
				Timestamp: ts,
			})
			if err != nil {
				log.Printf("react busy %s failed: chan=%s ts=%s: %v", emoji, channel, ts, err)
			}
		}(msg.Channel, msg.TS, cfg.busyReaction, busyAddDone)
	}

	// Cross-channel address-by-handle: if the parsed target matches a
	// registered alias, dispatch the inbound directly to the aliased
	// session via gc's session-message API, regardless of channel
	// binding. The originating channel's bound session still sees the
	// inbound (above) and is expected to stay silent (per its prompt)
	// because target != its handle.
	displacedOwned := false
	if aliasedSessionID != "" {
		// Thread-stickiness bind: record (channel, msg.TS) -> target
		// so subsequent thread replies (whose msg.ThreadTS will
		// equal this msg.TS) inherit the same handle without the
		// human re-tagging. Only binds when the handle resolved to a
		// registered alias — a target with no alias entry shouldn't
		// poison the sticky map. Binds on the suppressed path too:
		// thread replies must keep inheriting the handle (and keep
		// being suppressed) or the affordance flip-flops mid-thread.
		if threadHandleSticky != nil {
			threadHandleSticky.Bind(msg.Channel, msg.TS, target)
		}
		if !aliasSuppressed {
			// Transfer the slot we already hold to the alias goroutine.
			// No new acquireDispatchSlot — that would double-count
			// against dispatchSem (gc-cby.26 Phase 4 review fix).
			//
			// The dedup verdict transfers with it (codex r3 P1): for a
			// targeted inbound the alias dispatch IS the delivery to
			// the addressed session, so committing at processSlackEvent
			// return would drop a waiting Slack redelivery even when
			// this dispatch then fails. Success commits; failure
			// forgets so the redelivery can take over. The retaken
			// delivery re-runs postInbound too — a duplicate for the
			// channel-bound session (which self-dedupes by message ts)
			// is the acceptable cost of not losing the addressed
			// session's copy.
			commitDedup = false
			displacedOwned = true
			released = true
			// The alias leg delivers the addressed-session copy of an
			// ADMITTED event — watermark advanced, Slack acked — so it is
			// event-path work and must be inside eventWG (round 5, codex
			// r4 P0-part): without its own count, main's bounded shutdown
			// wait could conclude while this leg is still dispatching,
			// running the coalescer drain and the spool seal underneath a
			// delivery that can still fail and need the spool. The Add
			// happens before processSlackEvent returns, while the
			// handler's own entry-time count is still held (round 3, 2b),
			// so the group never touches zero across the handoff.
			if cfg.eventWG != nil {
				cfg.eventWG.Add(1)
			}
			dispatchInflightWG.Add(1)
			go func(displaced []busyDisplaced) {
				if cfg.eventWG != nil {
					defer cfg.eventWG.Done()
				}
				defer dispatchInflightWG.Done()
				defer release()
				// Alias-injection claim (gp-ios, codex review P1): the
				// session-message injection has no gc-side dedup at all,
				// and a bot-mention twin that skipped its channel copy
				// still reaches here — without a claim the addressed
				// session would read the same "address-by-handle"
				// reminder twice, and skipping the twin outright would
				// lose the injection when the owning twin's dispatch
				// then failed (both events already got their 200s). Same
				// two-state lifecycle as the channel claim: own it, or
				// park (bounded by the owner's single POST) and then
				// conclude like the success branch (owner delivered) or
				// take over (owner failed).
				aliasKey := aliasDeliveryClaimKey(target, inbound.Conversation.ConversationID, inbound.ProviderMessageID)
				aliasProceed, aliasWait := cfg.channelClaims.begin(aliasKey)
				for !aliasProceed {
					if aliasWait == nil {
						log.Printf("alias dispatch: handle=%s ts=%s already injected by same-ts twin — skipped",
							target, inbound.ProviderMessageID)
						// The twin's injection IS this event's addressed
						// delivery: conclude exactly like the success
						// branch below.
						for _, d := range displaced {
							go removeBusyReaction(cfg, inbound.Conversation.ConversationID, d.mark)
						}
						cfg.eventDedup.commit(env.EventID)
						return
					}
					log.Printf("alias dispatch: handle=%s ts=%s parked behind in-flight same-ts twin injection",
						target, inbound.ProviderMessageID)
					<-aliasWait
					// No drain deferral on THIS leg, deliberately (gp-32q,
					// codex r3 P1 #2). The channel leg can defer to the
					// shutdown spool because the spool replays a channel
					// copy — but a spooled line replays through the
					// coalescer as channel audience and does NOT
					// reconstruct an addressed session injection (see the
					// drain arm below). So during a drain there is nothing
					// durable behind this claim, and a twin that deferred
					// would suppress an addressed delivery that no replay
					// re-creates. The takeover POST is the only recovery
					// path there is; it keeps the pre-gp-32q behavior.
					aliasProceed, aliasWait = cfg.channelClaims.begin(aliasKey)
				}
				// Delivery-receipt gate for the injection leg (gp-32q).
				// Identical contract to the channel copy: a receipt
				// that vouches for nothing gets ONE in-place re-dispatch
				// while the injection claim is still held, and only then
				// falls through to the failure branch, which releases
				// the claim so a parked twin re-injects.
				aliasReceipt, aliasOK := dispatchToAliasedSession(cfg, aliasedSessionID, inbound, target)
				aliasVerdict := aliasReceipt.verdict(cfg.deliveryReceiptGate)
				for attempt := 0; aliasOK && aliasVerdict == receiptUnconfirmed && attempt < deliveryReceiptRepostAttempts && receiptRepostAllowed(cfg); attempt++ {
					log.Printf("alias dispatch: handle=%s ts=%s gc did not vouch for the injection (%s) — re-dispatching in place (attempt %d/%d)",
						target, inbound.ProviderMessageID, aliasReceipt.logField(aliasVerdict), attempt+1, deliveryReceiptRepostAttempts)
					aliasReceipt, aliasOK = dispatchToAliasedSession(cfg, aliasedSessionID, inbound, target)
					aliasVerdict = aliasReceipt.verdict(cfg.deliveryReceiptGate)
				}
				if aliasOK && aliasVerdict == receiptHeld {
					log.Printf("alias dispatch: HELD handle=%s ts=%s gc accepted the injection and has not finished delivering it — not re-dispatching — %s",
						target, inbound.ProviderMessageID, aliasReceipt.logField(aliasVerdict))
				}
				if aliasOK && aliasVerdict == receiptUnconfirmed {
					log.Printf("alias dispatch: UNDELIVERED handle=%s ts=%s gc accepted the injection but did not vouch that it reached the session after %d re-dispatch(es) — %s",
						target, inbound.ProviderMessageID, deliveryReceiptRepostAttempts, aliasReceipt.logField(aliasVerdict))
					aliasOK = false
				}
				if aliasOK {
					cfg.channelClaims.commit(aliasKey)
					cfg.deliveredIDs.record(target, inbound.Conversation.ConversationID, inbound.ProviderMessageID)
					// Final delivery landed: NOW retire the marks
					// this re-target displaced (codex r7).
					for _, d := range displaced {
						go removeBusyReaction(cfg, inbound.Conversation.ConversationID, d.mark)
					}
					cfg.eventDedup.commit(env.EventID)
					return
				}
				// The busy reaction was already launched for this
				// message, but no reply is coming — the addressed
				// session never got it and the channel-bound session
				// stays silent — so without cleanup the hourglass
				// sits forever next to the ⚠️ (codex r6). The removal
				// runs SYNCHRONOUSLY and completes before forget wakes
				// any parked retry: an async removal could otherwise
				// land after the retry's re-add (already_reacted) and
				// strip the recovered attempt's emoji (codex r7). The
				// marks this attempt displaced are restored — their
				// agents may still be working (codex r7).
				if cfg.busyReaction != "" {
					// ALL registry mutations happen before any Slack
					// network I/O (codex r10): take this message's
					// marks and restore the displaced ones first —
					// both are instant in-memory ops — so a displaced
					// agent's reply arriving during the (bounded, up
					// to add-wait + Slack timeout) removal calls below
					// finds its restored mark and clears normally
					// instead of missing the registry entirely.
					taken := cfg.busyMarks.takeMessage(
						inbound.Conversation.ConversationID, inbound.ReplyToMessageID, inbound.ProviderMessageID)
					// Displaced marks whose thread a reply consumed
					// while this dispatch was in flight cannot be
					// restored (tombstoned) — remove their reactions
					// instead (codex r8).
					blocked := cfg.busyMarks.restoreDisplaced(inbound.Conversation.ConversationID, displaced)
					for _, tk := range taken {
						removeBusyReaction(cfg, inbound.Conversation.ConversationID, tk)
					}
					for _, tk := range blocked {
						go removeBusyReaction(cfg, inbound.Conversation.ConversationID, tk)
					}
				}
				// Failed addressed-session leg DURING THE SHUTDOWN DRAIN
				// (round 5, mirroring the urgent/DM path — round 3, 2d):
				// the event is past the watermark, Slack got its 200 long
				// ago, and with the admission barrier down no redelivery
				// or parked twin can retry the injection — the spool is
				// the message's only durability. It replays through the
				// coalescer's normal CHANNEL buffers at the next startup
				// (the targeted session injection is not reconstructed;
				// the ⚠️ reaction below still tells the human this leg
				// failed, and the per-ts gc dedup key bounds a duplicate
				// if a parked twin does somehow land). Outside the drain,
				// the forget below keeps today's redelivery/twin retry
				// paths and the spool stays untouched.
				if cfg.draining != nil && cfg.draining.Load() {
					// Parts ride the spool line (codex round-2 finding
					// 4). files stays empty: the alias copy's Text was
					// never file-augmented, matching its legacy shape.
					// A processed deletion spools as its notice
					// (round-3 finding 3).
					spoolEntry := []pendingChannelInbound{{
						inbound:      inbound,
						threadAnchor: anchor,
						preamble:     preamble,
						body:         text,
					}}
					cfg.coalescer.applyDeletionTombstones(inbound.Conversation.ConversationID, spoolEntry)
					if cfg.inboundSpool.spillBatch(inbound.Conversation.ConversationID, spoolEntry) {
						log.Printf("alias dispatch: handle=%s ts=%s failed during shutdown drain — spooled for startup replay", target, inbound.ProviderMessageID)
					} else {
						log.Printf("alias dispatch: LOSS handle=%s ts=%s failed during shutdown drain and could not be spooled — LOST (already acked to Slack; the watermark backfill cannot recover admitted events)", target, inbound.ProviderMessageID)
					}
				}
				// Put the thread-context watermark back for the same
				// reason the channel leg does (codex r4 P2 #5): this
				// injection carried the preamble, and a takeover that
				// read the ts as already conveyed would re-dispatch the
				// body without it. Conditional on the entry still naming
				// this ts, so a newer delivery keeps its own advance.
				if threadCtxAdvanced {
					cfg.threadContextCache.rollbackDelivered(target, inbound.Conversation.ConversationID, msg.ThreadTS, inbound.ProviderMessageID, threadCtxPrevTS)
				}
				// Release the injection claim after the busy cleanup so
				// a parked same-ts twin taking over re-dispatches into a
				// settled registry; the affordance emoji this failed
				// attempt removed is a cosmetic miss for the takeover,
				// not a delivery one (gp-ios).
				cfg.channelClaims.forget(aliasKey)
				cfg.eventDedup.forget(env.EventID)
				reactAliasDispatchFailure(cfg.slackBotToken,
					inbound.Conversation.ConversationID, inbound.ProviderMessageID)
			}(busyDisplacedMarks)
		}
	}

	// No alias dispatch owns this event: postInbound was the final
	// delivery and it succeeded, so retire the displaced marks now
	// (codex r7 — when an alias dispatch fires, ITS success branch
	// does this instead, because the alias POST is the delivery that
	// decides the event).
	if busyEligible && !displacedOwned {
		for _, d := range busyDisplacedMarks {
			go removeBusyReaction(cfg, msg.Channel, d.mark)
		}
	}
}

// downloadSlackFiles fetches each file's bytes from Slack (Bearer-auth
// against url_private), writes them to
// $INBOUND_FILE_STORE/<channel>/<ts>-<safe-filename>, and returns
// externalAttachment records pointing at the local file:// path. Any file
// that fails to download is dropped from the returned slice and a
// warning is logged — the inbound is still posted with whatever files
// succeeded so the message itself isn't lost.
func downloadSlackFiles(cfg config, channel, ts string, files []slackFile) []externalAttachment {
	if cfg.inboundFileStore == "" {
		log.Printf("inbound file download skipped: INBOUND_FILE_STORE empty (%d files dropped)", len(files))
		return nil
	}
	// Sanitize channel + ts as path components before joining: filepath.Join
	// cleans `..` but does not confine to the base, so a hostile channel id
	// like "../etc" would still escape inboundFileStore. safePathComponent is
	// stricter than safeFilename — Slack channel IDs and ts strings are
	// ID-like, so a strict allowlist is appropriate. gc-ywe.7.
	channelDir := filepath.Join(cfg.inboundFileStore, safePathComponent(channel))
	// 0o700: store may contain DM file content; not world-readable. gc-ywe.6.
	if err := os.MkdirAll(channelDir, 0o700); err != nil {
		log.Printf("inbound file download: mkdir %s: %v", channelDir, err)
		return nil
	}
	tsPrefix := safePathComponent(ts)
	out := make([]externalAttachment, 0, len(files))
	for _, f := range files {
		if f.URLPrivate == "" {
			log.Printf("inbound file %s: url_private empty, dropped", f.ID)
			continue
		}
		name := f.Name
		if name == "" {
			name = f.Title
		}
		if name == "" {
			name = f.ID
		}
		dest := filepath.Join(channelDir, tsPrefix+"-"+safeFilename(name))
		if err := slackDownloadToFile(cfg.slackBotToken, f.URLPrivate, dest); err != nil {
			log.Printf("inbound file %s download failed: %v", f.ID, err)
			continue
		}
		out = append(out, externalAttachment{
			ProviderID: f.ID,
			URL:        "file://" + dest,
			MIMEType:   attachmentMIMEType(f),
		})
	}
	return out
}

// formatInboundFilesBlock renders a text block describing every file
// attached to an inbound Slack message, for the channel-binding
// delivery path whose downstream renderer (gc extmsg → bound-session
// reminder) surfaces only the message text (ci-f6x0). Iterates the
// event's files[] — not just the downloaded subset — so every
// attachment is named even when INBOUND_FILE_STORE is unset or a
// download failed; downloaded is matched by Slack file id to attach
// the spooled local path. Every Slack-controlled field is neutralized
// (cby.33) so a forged </system-reminder> in a filename cannot fake a
// reminder boundary once gc wraps the text in its envelope. Returns
// "" when there are no files.
func formatInboundFilesBlock(files []slackFile, downloaded []externalAttachment) string {
	if len(files) == 0 {
		return ""
	}
	localPath := make(map[string]string, len(downloaded))
	for _, att := range downloaded {
		localPath[att.ProviderID] = strings.TrimPrefix(att.URL, "file://")
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%d Slack %s attached]", len(files), noun)
	for i, f := range files {
		name := f.Name
		if name == "" {
			name = f.Title
		}
		if name == "" {
			name = f.ID
		}
		mime := f.MIMEType
		if mime == "" {
			mime = "unknown type"
		}
		fmt.Fprintf(&b, "\n  %d. %s (%s, id %s)", i+1,
			neutralizeMarkupBoundaries(name),
			neutralizeMarkupBoundaries(mime),
			neutralizeMarkupBoundaries(f.ID))
		if p, ok := localPath[f.ID]; ok {
			fmt.Fprintf(&b, " — saved to %s; Read that path to view it", neutralizeMarkupBoundaries(p))
		} else {
			b.WriteString(" — bytes not spooled locally; fetch via Slack API files.info + url_private with the adapter bot token")
		}
	}
	return b.String()
}

// safePathComponent sanitizes a Slack-supplied identifier (channel id, ts)
// for use as a filesystem path component. Stricter than safeFilename: only
// [A-Za-z0-9_.-] survive; everything else (path separators, NUL, control
// chars, whitespace, unicode, punctuation) is replaced with '_'. The first
// leading dot is replaced with '_' so the result can never be `.`, `..`,
// or be treated as a hidden dotfile; any further internal `..` segments
// are harmless because filepath.Join normalizes them within the joined
// path (they cannot escape the parent once the leading byte is `_`).
// Empty input returns "_" so the caller always has a usable non-empty
// component. Length capped at 64 chars — Slack channel IDs are ~10 chars
// and ts strings are ~17 chars, so 64 is generous. gc-ywe.7.
func safePathComponent(s string) string {
	const maxLen = 64
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		default:
			// Non-allowlist runes (including all multi-byte runes) are
			// replaced with a single ASCII underscore. This keeps the
			// invariant: cleaned is pure ASCII below.
			b.WriteRune('_')
		}
	}
	cleaned := b.String()
	if strings.HasPrefix(cleaned, ".") {
		cleaned = "_" + cleaned[1:]
	}
	// cleaned is guaranteed ASCII here (loop above maps every non-allowlist
	// rune to '_'), so the byte-indexed truncation cannot split a multi-byte
	// rune. Do not introduce a non-ASCII character into the allowlist
	// without revisiting this assumption.
	if len(cleaned) > maxLen {
		cleaned = cleaned[:maxLen]
	}
	if cleaned == "" {
		return "_"
	}
	return cleaned
}

// safeFilename strips path separators and other dangerous characters from
// a Slack-supplied filename so it can't escape the inbound file store
// directory. More permissive than safePathComponent: keeps spaces and
// non-ASCII characters that humans expect in filenames. Length is capped
// at 200 chars (well under the typical 255 filename limit) to leave room
// for the leading ts prefix.
func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0:
			b.WriteRune('_')
		case r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	cleaned := b.String()
	for strings.HasPrefix(cleaned, ".") {
		cleaned = "_" + cleaned[1:]
	}
	if len(cleaned) > 200 {
		cleaned = cleaned[:200]
	}
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// isSlackFileURL reports whether rawURL is safe to fetch with the Slack bot
// token: scheme must be https, host (lowercased, port stripped) must be one
// of slack.com, *.slack.com, slack-files.com, or *.slack-files.com, and the
// port (if present) must be 443. Trailing-dot FQDNs are rejected by the
// suffix check (see comment in the body). Returns a non-nil error only when
// the input fails URL parsing; policy rejections return (false, nil) so
// callers can distinguish.
//
// Defense against forged inbound url_private values post signing-secret
// compromise (gc-0fn). Without this gate, a forged event can point
// url_private at any URL and slackDownloadToFile sends the bot token in
// the Authorization header to that URL — credential exfiltration plus
// internal-service probing (cloud metadata, gc API on loopback, etc.).
//
// Companion defenses live in buildSlackHTTPClient (gc-vrw):
//   - DNS rebinding: a constrained Dialer rejects connections to private,
//     loopback, link-local, or unspecified addresses regardless of the
//     hostname that resolved to them.
//   - HTTP redirects: CheckRedirect re-applies validateSlackFileURL to
//     each 3xx target so a compromised Slack CDN host cannot 302 the
//     bot token to an attacker-controlled host.
func isSlackFileURL(rawURL string) (bool, error) {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		// This error propagates verbatim into slackDownloadToFile's
		// returned error and adapter logs — see unwrapURLError for why
		// it cannot be wrapped raw. gpk-la1y.
		return false, fmt.Errorf("parse url_private %q: %w", redactSlackURL(rawURL), unwrapURLError(err))
	}
	if !u.IsAbs() {
		// ParseRequestURI accepts absolute paths (e.g. "/files-pri/...") and
		// protocol-relative URLs ("//attacker.com/...") without an error;
		// IsAbs() returns false for both, so they are caught here. A forged
		// url_private must be a full absolute URL, never a path.
		return false, fmt.Errorf("url_private not absolute: %q", redactSlackURL(rawURL))
	}
	if u.Scheme != "https" {
		return false, nil
	}
	if p := u.Port(); p != "" && p != "443" {
		return false, nil
	}
	// Note: we do NOT trim a trailing dot from the host. A trailing-dot FQDN
	// (e.g. "files.slack.com.") is rejected by the suffix check below
	// because the literal string ends in ".com." rather than ".com" or
	// ".slack.com". This is the intended strict policy — Slack never
	// returns trailing-dot hosts in url_private.
	host := strings.ToLower(u.Hostname())
	if host == "slack.com" || host == "slack-files.com" ||
		strings.HasSuffix(host, ".slack.com") ||
		strings.HasSuffix(host, ".slack-files.com") {
		return true, nil
	}
	return false, nil
}

// redactSlackURL returns a form of raw safe to embed in error messages
// and logs: the query string AND fragment are dropped and userinfo is
// removed entirely, while host/path are preserved so log scanners can
// still correlate on the CDN link. Slack CDN url_private links and
// pre-signed upload URLs carry auth tokens in the query (t=xoxe-...,
// token=...) and sometimes the fragment, and a forged url_private may
// carry attacker-chosen credentials in user[:password]@host form.
// url.URL.Redacted() alone is NOT sufficient: it masks the userinfo
// password only (leaving a bare username or the query/fragment intact),
// so we clear those fields explicitly.
//
// Two input shapes bypass the structural field clears: input url.Parse
// rejects outright (handled by the '?'/'#' cut + stripURLUserinfo
// fallback), and an "opaque" URI (scheme:opaque with no "//"), where the
// credential-bearing text lives in u.Opaque untouched by the clears.
// url.Parse accepts the opaque form without error, so we detect it and
// route it through the same textual fallback. Finally, a slackTokenRe
// backstop guarantees no Slack token shape survives in ANY form
// (hierarchical, opaque, or backslash-delimited). gpk-la1y.
func redactSlackURL(raw string) string {
	var safe string
	if u, err := url.Parse(raw); err == nil && u.Opaque == "" {
		u.RawQuery = ""
		u.Fragment = ""
		u.User = nil
		safe = u.String()
	} else {
		safe, _, _ = strings.Cut(raw, "?")
		safe, _, _ = strings.Cut(safe, "#")
		safe = stripURLUserinfo(safe)
	}
	return slackTokenRe.ReplaceAllString(safe, "[redacted-token]")
}

// stripURLUserinfo removes a "user[:pass]@" prefix from the authority of
// a URL-ish string that redactSlackURL could not clear structurally, so
// forged credentials (or a token stuffed into userinfo) do not survive
// its fallback. Only the authority between "://" and the next '/' is
// considered, so an '@' in a later path segment is left alone. Inputs
// lacking a "://" (e.g. an opaque scheme:host@... or backslash-delimited
// form) have no authority we can locate textually; the slackTokenRe
// backstop in redactSlackURL covers token-shaped credentials there. gpk-la1y.
func stripURLUserinfo(s string) string {
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	authStart := i + len("://")
	rest := s[authStart:]
	authority := rest
	if end := strings.IndexByte(rest, '/'); end >= 0 {
		authority = rest[:end]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return s
	}
	return s[:authStart] + authority[at+1:] + s[authStart+len(authority):]
}

// urlInTextRe matches an absolute URL embedded in free-form text,
// bounded by whitespace, quotes, or angle brackets. It scrubs raw URLs
// that appear inside error text we do not author — see scrubSlackSecrets.
var urlInTextRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// slackTokenRe matches Slack auth token shapes (xoxb-/xoxe-/xoxp-/xoxa-/
// xoxr-/xoxs-/xoxd- and xapp- app tokens). It is the backstop for a token
// that reaches error text WITHOUT travelling inside an absolute URL — a
// token in a relative redirect Location (no scheme://, so urlInTextRe
// misses it), or one reflected bare by a compromised-but-allowlisted
// origin in a 4xx body. gpk-la1y.
var slackTokenRe = regexp.MustCompile(`(xox[a-zA-Z]|xapp)-[A-Za-z0-9-]+`)

// scrubSlackSecrets removes secrets that must never reach adapter logs or
// HTTP receipts from free-form text: every embedded absolute URL is passed
// through redactSlackURL (dropping token-bearing query, fragment, and
// userinfo), and any bare Slack token shape that survives is replaced.
// Both passes are required: net/http parses a redirect Location BEFORE
// CheckRedirect runs and, on failure, interpolates the raw header into a
// *url.Error's text — and that header may be relative, so the URL pass
// alone would miss its token. gpk-la1y.
func scrubSlackSecrets(s string) string {
	s = urlInTextRe.ReplaceAllStringFunc(s, redactSlackURL)
	s = slackTokenRe.ReplaceAllString(s, "[redacted-token]")
	return s
}

// sanitizeSlackErrorBody makes an untrusted HTTP error-response body safe
// to embed in an error that reaches log.Printf and an HTTP receipt: it
// strips control characters (defeating log-line injection via embedded
// CR/LF) and scrubs any URL or bare Slack token the origin reflected.
// gpk-la1y.
func sanitizeSlackErrorBody(b []byte) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, string(b))
	return scrubSlackSecrets(cleaned)
}

// unwrapURLError returns the underlying cause of a *url.Error, or err
// unchanged. A *url.Error's Error() text embeds the full request URL —
// token-bearing query included — so wrapping one with %w leaks the URL
// even when the surrounding message is redacted. Callers unwrap here and
// re-wrap the bare cause against a redactSlackURL form. gpk-la1y.
func unwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// redactTransportError re-wraps a net/http transport failure so no
// token-bearing URL reaches adapter logs or HTTP receipts, reporting safeURL
// (a redactSlackURL form) in place of the raw target. unwrapURLError strips
// the *url.Error wrapper — whose Error() embeds the full request URL —
// then scrubSlackSecrets removes any token the bare cause still carries.
// The critical case: net/http parses a redirect Location BEFORE
// CheckRedirect runs and, on a malformed header, returns a *url.Error whose
// .Err text interpolates the raw, token-bearing Location verbatim (absolute
// or relative). Safety rests on unwrapURLError reading only .Err (never
// .URL, which net/http also sets to the raw redirect target) and on
// scrubSlackSecrets as the redaction of last resort. The %w wrap is kept
// when scrubbing is a no-op so errors.Is still matches plain transport
// errors (e.g. connection-refused); it degrades to %s only on the
// URL-bearing path. Shared by slackDownloadToFile (GET) and
// slackPutFileBytes (upload POST). gpk-la1y.
func redactTransportError(action, safeURL string, e error) error {
	cause := unwrapURLError(e)
	causeText := cause.Error()
	if scrubbed := scrubSlackSecrets(causeText); scrubbed != causeText {
		return fmt.Errorf("%s %s: %s", action, safeURL, scrubbed)
	}
	return fmt.Errorf("%s %s: %w", action, safeURL, cause)
}

// validateSlackFileURL is the SSRF gate applied to inbound url_private
// values before slackDownloadToFile sends the bot token. Indirected through
// a package var so tests of unrelated download mechanics (atomic write,
// 4xx handling, permissions) can swap it for a permit-all stub via
// testAllowAnyURL — production callers always see isSlackFileURL.
//
// WARNING: not safe for concurrent test access. Tests that swap this var
// must NOT call t.Parallel(), and must not run alongside any test that
// depends on the production validator. testAllowAnyURL uses t.Cleanup to
// restore the previous value after the test exits.
var validateSlackFileURL = isSlackFileURL

// slackDialIPGuard is the per-IP guard invoked from net.Dialer.Control
// inside buildSlackHTTPClient. Indirected through a package var so the
// existing test helper testAllowAnyURL can also relax the dial-time
// check for tests that point url_private at httptest stubs on
// 127.0.0.1. Production callers always see isPrivateOrLoopbackIP.
//
// WARNING: same concurrency contract as validateSlackFileURL. Tests
// swapping this var must NOT call t.Parallel().
var slackDialIPGuard = isPrivateOrLoopbackIP

// slackDownloadToFile GETs urlPrivate with a Bearer token and streams the
// body to dest via an atomic temp+rename. Non-2xx responses produce an
// error with the truncated body for diagnosis. The url_private host is
// validated against the Slack allowlist before any network I/O — see
// isSlackFileURL for the threat model (gc-0fn).
func slackDownloadToFile(token, urlPrivate, dest string) error {
	// Every error below reports safeURL — never the raw urlPrivate — to
	// keep its token-bearing query and any attacker-chosen userinfo out
	// of adapter logs (see redactSlackURL for the threat model). The
	// validateSlackFileURL branch is covered too: isSlackFileURL redacts
	// its own error messages. gpk-la1y.
	safeURL := redactSlackURL(urlPrivate)
	// Transport failures below route through redactTransportError, which
	// unwraps the *url.Error (whose Error() embeds the full token-bearing
	// request URL, including a raw redirect Location) and reports safeURL.
	// gpk-la1y.
	ok, err := validateSlackFileURL(urlPrivate)
	if err != nil {
		return fmt.Errorf("validating url_private: %w", err)
	}
	if !ok {
		// safeURL preserves the host/path for log-scanner matching while
		// masking attacker-chosen credentials and dropping the query.
		return fmt.Errorf("url_private host not in slack allowlist: %q", safeURL)
	}
	req, err := http.NewRequest(http.MethodGet, urlPrivate, nil)
	if err != nil {
		return redactTransportError("build download request to", safeURL, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := slackHTTPClientSingleton().Do(req)
	if err != nil {
		return redactTransportError("GET", safeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// respBody is untrusted origin content: sanitize control chars
		// (log-line injection) and scrub any reflected URL/token. gpk-la1y.
		return fmt.Errorf("GET %s: %s — %s", safeURL, resp.Status, sanitizeSlackErrorBody(respBody))
	}
	tmp := dest + ".tmp"
	// 0o600: file content may be DM-private; rename below preserves this mode. gc-ywe.6.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy body: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, dest, err)
	}
	return nil
}

// cgnat100_64 is RFC 6598 (100.64.0.0/10), the IPv4 carrier-grade NAT
// space. net.IP.IsPrivate does not cover this range, but Tailscale
// assigns 100.64.x.x addresses to peers and this adapter is documented
// as deployable behind Tailscale Funnel. A url_private host that briefly
// resolves to a Tailscale peer is exactly the DNS-rebinding case the
// dial guard exists to defeat. gc-vrw review.
var cgnat100_64 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isPrivateOrLoopbackIP reports whether ip falls into a range that the
// adapter must never dial when fetching url_private. The set covers
// IPv4 RFC1918 (10/8, 172.16/12, 192.168/16) and IPv6 unique-local
// (fc00::/7) via net.IP.IsPrivate; RFC 6598 carrier-grade NAT
// (100.64.0.0/10) including Tailnet peer ranges; loopback (127/8, ::1)
// via IsLoopback; link-local unicast (169.254/16, fe80::/10) and
// link-local/interface-local multicast; and the unspecified address
// (0.0.0.0, ::). Public unicast addresses, including legitimate Slack
// CDN ranges, return false.
//
// The check is by address only — there is no DNS or hostname lookup
// here. It is invoked from net.Dialer.ControlContext after Go's
// resolver has produced a candidate address but before the connect
// syscall, so a hostname that briefly resolves to a private IP (DNS
// rebinding) is caught at the dial step regardless of how the URL was
// validated. gc-vrw.
func isPrivateOrLoopbackIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnat100_64.Contains(v4) {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsInterfaceLocalMulticast() {
		return true
	}
	return false
}

// buildSlackHTTPClient returns an *http.Client wired with two
// defense-in-depth controls beyond the URL allowlist enforced by
// validateSlackFileURL:
//
//  1. A constrained net.Dialer whose Control hook inspects the
//     resolved address (post-DNS, pre-connect) and refuses any IP that
//     isPrivateOrLoopbackIP flags. This blocks DNS-rebinding attacks
//     where an allowlisted hostname briefly resolves to an internal IP.
//
//  2. A CheckRedirect policy that re-applies validateSlackFileURL to
//     each 3xx target. The default http.Client.CheckRedirect would
//     follow a 302 from a legitimate (or compromised) Slack CDN host
//     to attacker.com, sending the bot token in the Authorization
//     header. This policy aborts the redirect with a typed error
//     before the second hop is dialed.
//
// The function returns a fresh *http.Client (and underlying
// *http.Transport) per call. Production code reaches the client via
// slackHTTPClientSingleton, which wraps a single buildSlackHTTPClient
// invocation in sync.Once so idle-connection pooling is shared across
// batched slackDownloadToFile calls (gc-px8.3). Tests retain direct
// access to construct fresh clients for property assertions.
//
// HTTP proxy environment variables (HTTP_PROXY / HTTPS_PROXY) are
// intentionally NOT honored: a private-IP proxy would bypass the
// dial-time IP guard, since net.Dialer.ControlContext sees the proxy
// address rather than the final Slack target. The slack-pack adapter
// reaches Slack CDN hosts directly in every supported deployment, so
// proxy support is unnecessary and removing it eliminates a real
// SSRF bypass. gc-vrw review.
func buildSlackHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("dial %s %s: split host/port: %w", network, address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				// net.Dialer resolves to literal IPs before invoking
				// ControlContext, so a non-IP host here is a
				// programming error or an unexpected resolver result —
				// fail closed.
				return fmt.Errorf("dial %s %s: refusing to dial non-literal address %q", network, address, host)
			}
			if slackDialIPGuard(ip) {
				return fmt.Errorf("dial %s %s: refusing to dial private, loopback, or link-local address %s", network, address, ip)
			}
			return nil
		},
	}
	transport := &http.Transport{
		// Proxy intentionally nil — see function doc.
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		// Bound the whole round-trip including response-body read.
		// 5 minutes accommodates a slow ~1 GB Slack file at modest
		// throughput; legitimate downloads finish well inside this.
		Timeout:   5 * time.Minute,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Redirect targets carry the same token-bearing query as the
			// original url_private, and this error's text survives inside
			// the *url.Error returned by Client.Do — redactTransport only
			// sanitizes the outer URL, not this message. Redacted() alone
			// keeps the query, so redact fully here. gpk-la1y.
			if len(via) >= 10 {
				return fmt.Errorf("redirect chain exceeded 10 hops at %s", redactSlackURL(req.URL.String()))
			}
			ok, err := validateSlackFileURL(req.URL.String())
			if err != nil {
				return fmt.Errorf("validating redirect target: %w", err)
			}
			if !ok {
				return fmt.Errorf("refusing redirect to non-slack host %q", redactSlackURL(req.URL.String()))
			}
			return nil
		},
	}
}

// slackHTTPClientSingleton returns the process-wide *http.Client used
// by slackDownloadToFile. The first call constructs the client via
// buildSlackHTTPClient inside a sync.Once; subsequent calls return
// the cached instance, so the underlying *http.Transport's idle
// connection pool is reused across batched downloads (gc-px8.3).
//
// Reuse is safe with the existing test seams: slackDialIPGuard is
// read on every dial (not captured at construction time), so tests
// can swap it via the package var even after the singleton has been
// initialized. validateSlackFileURL inside CheckRedirect is similarly
// resolved per redirect.
//
// Tests that need a fresh client for structural property assertions
// (Transport identity, CheckRedirect non-nil, etc.) should call
// buildSlackHTTPClient directly rather than this accessor.
func slackHTTPClientSingleton() *http.Client {
	slackHTTPClientOnce.Do(func() {
		slackHTTPClient = buildSlackHTTPClient()
	})
	return slackHTTPClient
}

var (
	slackHTTPClient     *http.Client
	slackHTTPClientOnce sync.Once
)

// sweepResult summarizes one pass of the inbound file janitor. All counts
// are over a single sweep; aggregate behavior over time is not tracked
// (the bd issue gc-g52 was scoped to retention, not metrics).
type sweepResult struct {
	FilesRemoved int
	DirsRemoved  int
	BytesRemoved int64
	Errors       []error
}

// sweepInboundStore deletes regular files under root whose mtime is
// older than now-ttl, then removes any channel sub-directories that are
// empty after the file pass. Returns counts and any errors encountered;
// a missing root is not an error (the store is created lazily on first
// inbound). A non-positive ttl is a no-op so callers can guard at the
// config layer without re-checking here.
//
// The function is pure (no goroutines, no logging) so callers can test
// it deterministically with table-driven inputs and a fixed `now`.
func sweepInboundStore(root string, ttl time.Duration, now time.Time) sweepResult {
	var res sweepResult
	if root == "" || ttl <= 0 {
		return res
	}
	cutoff := now.Add(-ttl)

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res
		}
		res.Errors = append(res.Errors, fmt.Errorf("read root %s: %w", root, err))
		return res
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			// Files at the root are unexpected (the store layout puts
			// everything under <channel>/) — skip them so we don't delete
			// configuration the operator may have left there.
			continue
		}
		channelDir := filepath.Join(root, entry.Name())
		sweepChannelDir(channelDir, cutoff, &res)
	}
	return res
}

// sweepChannelDir applies the file-age filter to a single channel
// directory and removes the directory itself if it ends up empty.
// Errors are appended to res.Errors but never abort the sweep — one
// unreadable file shouldn't block the rest of the housekeeping pass.
func sweepChannelDir(channelDir string, cutoff time.Time, res *sweepResult) {
	files, err := os.ReadDir(channelDir)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("read %s: %w", channelDir, err))
		return
	}
	for _, f := range files {
		if !f.Type().IsRegular() {
			continue
		}
		path := filepath.Join(channelDir, f.Name())
		info, err := f.Info()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("stat %s: %w", path, err))
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		size := info.Size()
		if err := os.Remove(path); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("remove %s: %w", path, err))
			continue
		}
		res.FilesRemoved++
		res.BytesRemoved += size
	}
	// Re-read to see if the directory is now empty; only remove if so.
	remaining, err := os.ReadDir(channelDir)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("re-read %s: %w", channelDir, err))
		return
	}
	if len(remaining) == 0 {
		if err := os.Remove(channelDir); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("rmdir %s: %w", channelDir, err))
			return
		}
		res.DirsRemoved++
	}
}

// runInboundFileJanitor wakes every cfg.inboundFileSweepInterval and
// runs sweepInboundStore against cfg.inboundFileStore using cfg.inboundFileTTL.
// Returns immediately if either duration is non-positive or the store
// path is empty (janitor disabled). Cancellation via ctx is honored
// between ticks; an in-flight sweep runs to completion since each pass
// is bounded by the directory size.
func runInboundFileJanitor(ctx context.Context, cfg config) {
	if cfg.inboundFileStore == "" || cfg.inboundFileTTL <= 0 || cfg.inboundFileSweepInterval <= 0 {
		log.Printf("inbound file janitor disabled (store=%q ttl=%s interval=%s)",
			cfg.inboundFileStore, cfg.inboundFileTTL, cfg.inboundFileSweepInterval)
		return
	}
	log.Printf("inbound file janitor started: store=%s ttl=%s interval=%s",
		cfg.inboundFileStore, cfg.inboundFileTTL, cfg.inboundFileSweepInterval)
	ticker := time.NewTicker(cfg.inboundFileSweepInterval)
	defer ticker.Stop()
	// Run one sweep promptly on startup so a long-uptime adapter doesn't
	// wait a full interval before the first pass.
	logSweepResult(sweepInboundStore(cfg.inboundFileStore, cfg.inboundFileTTL, time.Now()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logSweepResult(sweepInboundStore(cfg.inboundFileStore, cfg.inboundFileTTL, time.Now()))
		}
	}
}

// logSweepResult emits one log line per sweep pass at most. Silent
// no-op passes (nothing removed, no errors) don't log to keep noise
// down on idle deployments.
func logSweepResult(res sweepResult) {
	if res.FilesRemoved == 0 && res.DirsRemoved == 0 && len(res.Errors) == 0 {
		return
	}
	log.Printf("inbound file janitor: files_removed=%d dirs_removed=%d bytes_removed=%d errors=%d",
		res.FilesRemoved, res.DirsRemoved, res.BytesRemoved, len(res.Errors))
	for _, err := range res.Errors {
		log.Printf("inbound file janitor error: %v", err)
	}
}

// tightenStorePermissions is a one-shot startup migration helper for
// pre-fix installs. The create-time mode constants in saveLocked +
// downloadSlackFiles + slackDownloadToFile produce 0o700/0o600 for
// every new write, but legacy state from prior versions sits at
// 0o755/0o644. This walks the three configured stores and tightens
// only-if-strictly-looser. Setuid/setgid/sticky bits are preserved
// (operators may deliberately set setgid on a shared-group inbound
// dir). Operator-tighter perms (e.g. 0o400 read-only) are left alone.
// Errors are logged and never fatal — the helper is best-effort.
//
// gc-ywe.6.
func tightenStorePermissions(cfg config) {
	for _, p := range []string{cfg.identityStorePath, cfg.handleAliasStorePath, cfg.channelMappingPath, cfg.rigMappingPath, cfg.threadSessionsStorePath, cfg.roomLaunchPath} {
		if p == "" {
			continue
		}
		tightenPerm(filepath.Dir(p), 0o700)
		tightenPerm(p, 0o600)
	}

	if cfg.inboundFileStore == "" {
		return
	}
	tightenPerm(cfg.inboundFileStore, 0o700)
	// One level deep: each <channel>/ subdir + its immediate
	// children. The adapter owns this layout (downloadSlackFiles
	// at L1316). Don't recurse further — anything deeper is
	// operator-customized territory.
	entries, err := os.ReadDir(cfg.inboundFileStore)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("tighten store: readdir %s: %v", cfg.inboundFileStore, err)
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		channelDir := filepath.Join(cfg.inboundFileStore, e.Name())
		tightenPerm(channelDir, 0o700)
		children, err := os.ReadDir(channelDir)
		if err != nil {
			log.Printf("tighten store: readdir %s: %v", channelDir, err)
			continue
		}
		for _, c := range children {
			if c.IsDir() {
				continue
			}
			tightenPerm(filepath.Join(channelDir, c.Name()), 0o600)
		}
	}
}

// tightenPerm chmods path to target if its perm bits are strictly
// looser. Setuid/setgid/sticky bits are preserved. Missing paths and
// symlinks are no-ops (Go's stdlib has no Lchmod, so following the
// link would chmod the target — refuse to do that). Errors are
// logged and never fatal.
func tightenPerm(path string, target os.FileMode) {
	if path == "" {
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("tighten store: stat %s: %v", path, err)
		}
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		log.Printf("tighten store: skipping symlink %s", path)
		return
	}
	mode := info.Mode()
	if mode.Perm()&^target == 0 {
		return
	}
	preserved := mode & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	final := preserved | target
	if err := os.Chmod(path, final); err != nil {
		log.Printf("tighten store: chmod %s: %v", path, err)
		return
	}
	// Use %v to render special bits symbolically (e.g. "g+s" for setgid)
	// so an operator reading the log can verify preservation.
	log.Printf("tighten store: %s %v -> %v (legacy state)", path, mode, final)
}

// parseHandlePrefix recognizes a leading address token of the form
// "<prefix><handle>" at the start of text, where the handle is followed
// by a colon, whitespace, or end-of-string. Leading whitespace before
// the prefix is tolerated. The handle character class is [A-Za-z0-9_-];
// the consumer chooses what handles map to which sessions via the
// /handle-alias registry. When matched, the handle is returned along
// with the remainder of the text (with any leading separator + single
// leading space trimmed); on no match, the original text is returned
// with an empty handle.
//
// Both `@name: foo` and `@name foo` are accepted because human users
// don't reliably type the colon — the colon is optional, but if it
// appears it must be the first character after the handle.
//
// Examples (with prefix "@"):
//
//	"@gascity: status?"       -> ("gascity", "status?")
//	"@ops foo"                -> ("ops",     "foo")
//	"@ops:hello"              -> ("ops",     "hello")
//	"  @lead hi"              -> ("lead",    "hi")
//	"@gascity"                -> ("gascity", "")
//	"@: foo"                  -> ("",        "@: foo")           (empty handle)
//	"hello @gascity: x"       -> ("",        "hello @gascity: x") (not at start)
//	"@bad/handle x"           -> ("",        "@bad/handle x")    (invalid separator after handle chars)
func parseHandlePrefix(text, prefix string) (handle, remainder string) {
	if prefix == "" {
		return "", text
	}
	trimmed := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(trimmed, prefix) {
		return "", text
	}
	rest := trimmed[len(prefix):]

	// Scan the longest run of valid handle characters at the start.
	handleEnd := 0
	for i := 0; i < len(rest); i++ {
		r := rest[i]
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			handleEnd = i + 1
		} else {
			break
		}
	}
	if handleEnd == 0 {
		return "", text
	}
	candidate := rest[:handleEnd]
	body := rest[handleEnd:]

	// Handle must end at: end-of-string, colon, or whitespace.
	// Anything else (e.g. `@name.foo`) means this isn't an address token.
	if body == "" {
		return candidate, ""
	}
	sep := body[0]
	switch sep {
	case ':':
		body = body[1:]
	case ' ', '\t', '\n':
		// whitespace separator — leave it; the next trim handles it
	default:
		return "", text
	}
	if len(body) > 0 && (body[0] == ' ' || body[0] == '\t' || body[0] == '\n') {
		body = body[1:]
	}
	return candidate, body
}

// identityRegistry maps gc session ids to per-message Slack identity
// overrides (chat:write.customize username/avatar). When a publish arrives
// for a known session id, the adapter injects username/icon into
// chat.postMessage so each role posts under its own visible name + avatar
// without spinning up a separate bot user.
//
// The registry persists to disk (atomic temp + rename) so adapter restarts
// don't strip identity from running sessions. Reads are RLock-only so
// concurrent /publish calls don't serialize.
type identityRegistry struct {
	mu       sync.RWMutex
	byID     map[string]identityRecord
	diskPath string
}

func newIdentityRegistry(diskPath string) (*identityRegistry, error) {
	r := &identityRegistry{
		byID:     make(map[string]identityRecord),
		diskPath: diskPath,
	}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("load identity registry from %s: %w", diskPath, err)
	}
	return r, nil
}

// Get returns the identity record for sessionID, plus a boolean indicating
// whether one is registered. Callers should treat the no-record case as
// "use default bot identity" rather than an error.
func (r *identityRegistry) Get(sessionID string) (identityRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.byID[sessionID]
	return rec, ok
}

// Set stores rec for sessionID and persists the registry to disk. An empty
// record (zero username + icon fields) is allowed — it effectively unsets
// the override. To fully delete the entry use Delete instead.
func (r *identityRegistry) Set(sessionID string, rec identityRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[sessionID] = rec
	return r.saveLocked()
}

// Delete removes the identity record for sessionID and persists the
// registry. Returns whether an entry actually existed; missing entries
// are not an error so callers can treat Delete as idempotent.
func (r *identityRegistry) Delete(sessionID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.byID[sessionID]
	delete(r.byID, sessionID)
	if err := r.saveLocked(); err != nil {
		return existed, err
	}
	return existed, nil
}

func (r *identityRegistry) load() error {
	if r.diskPath == "" {
		return nil
	}
	f, err := openRegistryFile(r.diskPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open identity store %s: %w", r.diskPath, err)
	}
	defer func() { _ = f.Close() }()
	// LimitReader caps the read at maxRegistryBytes+1 so a hostile or
	// corrupt file can't force a multi-gigabyte allocation before the
	// size check fires (gc-cby.32). The +1 lets us detect overflow
	// precisely: reading exactly maxRegistryBytes+1 means the
	// underlying file is at least maxRegistryBytes+1 bytes.
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return fmt.Errorf("read identity store %s: %w", r.diskPath, err)
	}
	if int64(len(data)) > maxRegistryBytes {
		return fmt.Errorf("identity store %s exceeds %d bytes", r.diskPath, maxRegistryBytes)
	}
	var stored map[string]identityRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode identity store: %w", err)
	}
	if stored != nil {
		r.byID = stored
	}
	return nil
}

func (r *identityRegistry) saveLocked() error {
	if r.diskPath == "" {
		return nil
	}
	// 0o700/0o600: store maps session-id ↔ persona display name; not world-readable. gc-ywe.6.
	// writeFile0600 (interactions.go) routes through os.CreateTemp so two
	// writers in the same directory don't collide on a fixed `<path>.tmp`
	// name (gc-px8.4 / gc-cby.14).
	data, err := json.MarshalIndent(r.byID, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity store: %w", err)
	}
	if err := writeFile0600(r.diskPath, data); err != nil {
		return fmt.Errorf("write identity store: %w", err)
	}
	return nil
}

// handleAliasRegistry maps a handle (consumer-defined string, e.g. a role
// or persona name) to a gc session id. Used by the cross-channel
// address-by-handle dispatcher: when a Slack inbound parses a handle
// that matches an alias, the adapter delivers the inbound directly to
// the aliased session via gc's session-message API, even if that session
// has no Slack binding for the originating channel.
//
// Persists to disk so restarts don't lose mappings; same atomic write
// pattern as the identity registry.
type handleAliasRegistry struct {
	mu       sync.RWMutex
	byHandle map[string]string
	diskPath string
}

func newHandleAliasRegistry(diskPath string) (*handleAliasRegistry, error) {
	r := &handleAliasRegistry{
		byHandle: make(map[string]string),
		diskPath: diskPath,
	}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("load handle alias registry from %s: %w", diskPath, err)
	}
	return r, nil
}

// Get returns the session id mapped to handle, plus a bool indicating
// whether one is registered. Callers should treat the no-record case as
// "not an alias — fall through to normal channel-binding routing".
func (r *handleAliasRegistry) Get(handle string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sid, ok := r.byHandle[handle]
	return sid, ok
}

// Set stores handle -> sessionID. Empty sessionID removes the entry.
func (r *handleAliasRegistry) Set(handle, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sessionID == "" {
		delete(r.byHandle, handle)
	} else {
		r.byHandle[handle] = sessionID
	}
	return r.saveLocked()
}

// findHandlesBySessionID returns every handle currently mapped to
// sessionID. Returns an empty slice (not nil) when sessionID is empty
// or no handles match. Used by the cby.5.4 thread-binding teardown
// subscriber to unwind the alias bootstrap installed in
// dispatchRoomLaunch when the underlying session ends. O(n) over the
// alias map; acceptable while the alias registry is bounded by active
// handles (typically tens to low hundreds). If the alias map ever
// grows large, store the handle alongside the thread binding instead.
func (r *handleAliasRegistry) findHandlesBySessionID(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var matches []string
	for handle, sid := range r.byHandle {
		if sid == sessionID {
			matches = append(matches, handle)
		}
	}
	return matches
}

// Delete removes the alias for handle and persists the registry. Returns
// whether an entry actually existed; missing entries are not an error so
// callers can treat Delete as idempotent. This is the explicit counterpart
// to Set with empty sessionID; both work, but Delete is the intent-clear
// verb.
func (r *handleAliasRegistry) Delete(handle string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.byHandle[handle]
	delete(r.byHandle, handle)
	if err := r.saveLocked(); err != nil {
		return existed, err
	}
	return existed, nil
}

func (r *handleAliasRegistry) load() error {
	if r.diskPath == "" {
		return nil
	}
	f, err := openRegistryFile(r.diskPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open handle alias store %s: %w", r.diskPath, err)
	}
	defer func() { _ = f.Close() }()
	// LimitReader caps the read at maxRegistryBytes+1 so a hostile or
	// corrupt file can't force a multi-gigabyte allocation before the
	// size check fires (gc-cby.32). The +1 lets us detect overflow
	// precisely: reading exactly maxRegistryBytes+1 means the
	// underlying file is at least maxRegistryBytes+1 bytes.
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return fmt.Errorf("read handle alias store %s: %w", r.diskPath, err)
	}
	if int64(len(data)) > maxRegistryBytes {
		return fmt.Errorf("handle alias store %s exceeds %d bytes", r.diskPath, maxRegistryBytes)
	}
	var stored map[string]string
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode handle alias store: %w", err)
	}
	if stored != nil {
		r.byHandle = stored
	}
	return nil
}

func (r *handleAliasRegistry) saveLocked() error {
	if r.diskPath == "" {
		return nil
	}
	// 0o700/0o600: store maps cross-channel @handle → session-id; not world-readable. gc-ywe.6.
	// writeFile0600 (interactions.go) routes through os.CreateTemp so two
	// writers in the same directory don't collide on a fixed `<path>.tmp`
	// name (gc-px8.4 / gc-cby.14).
	data, err := json.MarshalIndent(r.byHandle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode handle alias store: %w", err)
	}
	if err := writeFile0600(r.diskPath, data); err != nil {
		return fmt.Errorf("write handle alias store: %w", err)
	}
	return nil
}

// handleHandleAlias serves POST /handle-alias. Empty session_id removes
// the entry; non-empty stores or replaces it.
func handleHandleAlias(reg *handleAliasRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req handleAliasRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		handle := strings.TrimSpace(req.Handle)
		if handle == "" {
			http.Error(w, "handle is required", http.StatusBadRequest)
			return
		}
		removed := strings.TrimSpace(req.SessionID) == ""
		if err := reg.Set(handle, strings.TrimSpace(req.SessionID)); err != nil {
			log.Printf("handle alias store error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("handle alias: handle=%q session=%q removed=%v",
			handle, req.SessionID, removed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(handleAliasReceipt{
			Stored:    !removed,
			Removed:   removed,
			Handle:    handle,
			SessionID: req.SessionID,
		})
	}
}

// handleHandleAliasDelete serves DELETE /handle-alias. The handle is
// taken from either ?handle= query param (preferred for explicit verb)
// or from a JSON body { "handle": "..." } for symmetry with POST. Empty
// handle is rejected.
func handleHandleAliasDelete(reg *handleAliasRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handle := strings.TrimSpace(r.URL.Query().Get("handle"))
		if handle == "" {
			var req handleAliasRequest
			if r.ContentLength > 0 {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
					return
				}
				handle = strings.TrimSpace(req.Handle)
			}
		}
		if handle == "" {
			http.Error(w, "handle is required (?handle= or JSON body)", http.StatusBadRequest)
			return
		}
		existed, err := reg.Delete(handle)
		if err != nil {
			log.Printf("handle alias delete error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("handle alias delete: handle=%q existed=%v", handle, existed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(handleAliasDeleteReceipt{
			Removed: true,
			Existed: existed,
			Handle:  handle,
		})
	}
}

// dispatchToAliasedSession POSTs a system reminder to the gc session-message
// endpoint for the aliased session. The payload carries everything the
// receiving session needs to compose a reply: originating channel id (for
// routing the reply back), message ts (for threading), and the inbound text.
//
// Returns true on successful delivery, false on any error. The caller is
// responsible for surface-visible failure signaling (e.g. a ⚠️ reaction).
// dispatchToAliasedSession injects one address-by-handle reminder into
// the aliased session. Returns gc's delivery receipt (zero-valued when
// gc emits none) and whether gc ACCEPTED the injection — the receipt is
// what says it arrived (gp-32q).
func dispatchToAliasedSession(cfg config, sessionID string, msg externalInboundMessage, handle string) (deliveryReceipt, bool) {
	// Every interpolated string is run through neutralizeMarkupBoundaries
	// to prevent a Slack workspace member from forging </system-reminder>
	// boundaries inside the dispatched body and injecting arbitrary
	// system instructions into the receiving aliased session (cby.33,
	// extends cby.17 sanitization to the alias dispatch path).
	//
	// Surface any downloaded attachments (file:// local paths) so the aliased
	// session can Read them — vision works on local files. downloadSlackFiles
	// already wrote each file to local disk and set URL="file://"+localpath
	// and MIMEType; before this the dispatch interpolated only msg.Text and
	// dropped the images entirely. Each field is neutralized like the text
	// path (cby.33) so a forged </system-reminder> in a filename cannot break
	// out of the reminder envelope. Empty string when there are no
	// attachments, leaving the body byte-identical to the text-only form.
	attachmentsBlock := ""
	if len(msg.Attachments) > 0 {
		var ab strings.Builder
		fmt.Fprintf(&ab, "\nAttachments (%d) — saved to local disk; Read the file:// path to view:\n", len(msg.Attachments))
		for i, att := range msg.Attachments {
			name := filepath.Base(strings.TrimPrefix(att.URL, "file://"))
			fmt.Fprintf(&ab, "  %d. %s (%s): %s\n",
				i+1,
				neutralizeMarkupBoundaries(name),
				neutralizeMarkupBoundaries(att.MIMEType),
				neutralizeMarkupBoundaries(att.URL))
		}
		attachmentsBlock = ab.String()
	}
	// The sender renders as "Display Name (U0…)" when the envelope carries
	// a resolved name — the raw id stays alongside for audit and reply
	// plumbing — and as the bare id when resolution failed or is disabled
	// (Actor.DisplayName then equals the id or is empty). hq-uxln9.
	sender := msg.Actor.ID
	if msg.Actor.DisplayName != "" && msg.Actor.DisplayName != msg.Actor.ID {
		sender = msg.Actor.DisplayName + " (" + msg.Actor.ID + ")"
	}
	// A thread reply names its thread alongside its own ts (gp-0qw
	// item 2) so the addressed session can place the message without
	// re-deriving the thread from Slack.
	tsContext := neutralizeMarkupBoundaries(msg.ProviderMessageID)
	// The reply command threads under the ROOT: Slack's thread_ts must
	// name the parent, not a reply — a child ts here can misthread or
	// fail outright (codex round-1 finding 5). Top-level messages keep
	// threading under themselves.
	replyThreadTS := msg.ProviderMessageID
	if msg.ReplyToMessageID != "" && msg.ReplyToMessageID != msg.ProviderMessageID {
		tsContext += ", a reply in thread " + neutralizeMarkupBoundaries(msg.ReplyToMessageID)
		replyThreadTS = msg.ReplyToMessageID
	}
	body := fmt.Sprintf(
		"<system-reminder>\n"+
			"Slack address-by-handle: @%s addressed you from channel %s (Slack ts %s) by user %s.\n"+
			"\n"+
			"Message text:\n"+
			"%s\n"+
			"%s"+
			"\n"+
			"React to this message with writing_hand to signal you are actively working on it:\n"+
			"  gc slack react --emoji writing_hand\n"+
			"\n"+
			"To reply in that channel (threaded under their message), write your reply to a tmpfile and run:\n"+
			"  gc slack publish-to-channel \\\n"+
			"    --conversation-id %s \\\n"+
			"    --thread-ts %s \\\n"+
			"    --body-file <tmpfile>\n"+
			"\n"+
			"This bypasses your local channel binding (you have none for that channel) and posts directly through the slack adapter, with your registered identity applied.\n"+
			"</system-reminder>",
		neutralizeMarkupBoundaries(handle),
		// Prose position renders "#name (Cid)" when resolvable (gp-729
		// item 4); the --conversation-id flag below keeps the bare id
		// the CLI needs.
		neutralizeMarkupBoundaries(channelDisplay(cfg, msg.Conversation.ConversationID)),
		tsContext, // already per-field neutralized above
		neutralizeMarkupBoundaries(sender),
		neutralizeMarkupBoundaries(msg.Text),
		attachmentsBlock, // already per-field neutralized; pass raw
		neutralizeMarkupBoundaries(msg.Conversation.ConversationID),
		neutralizeMarkupBoundaries(replyThreadTS),
	)
	payload, _ := json.Marshal(gcSessionMessageRequest{Message: body})
	// PathEscape cityName and sessionID so URL-significant characters
	// (slash, percent, etc.) cannot alter routing on the gc API side
	// (sec-S-06). cityName comes from operator config and sessionID is
	// currently always gc-internal, but the registry is operator-editable
	// and future cby work may let external systems supply session ids.
	target := fmt.Sprintf("%s/v0/city/%s/session/%s/messages",
		cfg.gcAPIBase, url.PathEscape(cfg.cityName), url.PathEscape(sessionID))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		log.Printf("alias dispatch: build request: %v", err)
		return deliveryReceipt{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "gc-slack-adapter-alias")
	// Same ask as the channel forward (gp-32q). The session-message
	// endpoint is async today — it answers with a request id and emits
	// the real outcome as an event — so this leg is dishonest in exactly
	// the way the channel leg was, and it reads whatever receipt a
	// receipt-emitting gc returns through the shared parser.
	req.Header.Set("X-GC-Delivery-Receipt", "require")
	resp, err := gcForwardClient.Do(req)
	if err != nil {
		log.Printf("alias dispatch: POST %s: %v", target, err)
		return deliveryReceipt{}, false
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, deliveryReceiptBodyLimit))
	if resp.StatusCode >= 400 {
		log.Printf("alias dispatch: %s -> %s: %s", target, resp.Status, string(respBody))
		return deliveryReceipt{}, false
	}
	if readErr != nil {
		log.Printf("alias dispatch: handle=%s -> session=%s accepted; receipt unreadable: %v", handle, sessionID, readErr)
		return deliveryReceipt{}, true
	}
	receipt := parseDeliveryReceipt(respBody)
	log.Printf("alias dispatch: handle=%s -> session=%s OK %s",
		handle, sessionID, receipt.logField(receipt.verdict(cfg.deliveryReceiptGate)))
	return receipt, true
}

// reactAliasDispatchFailure fires a best-effort ⚠️ reaction on the original
// Slack message so a failed alias dispatch is visible in-channel rather than
// silently dropped. Runs inside the dispatch goroutine (already async).
func reactAliasDispatchFailure(token, channelID, ts string) {
	if token == "" {
		return
	}
	resp, err := postReactionToSlack(token, slackReactionsAddReq{
		Channel:   channelID,
		Name:      "warning",
		Timestamp: ts,
	})
	if err != nil {
		log.Printf("react warning (alias dispatch failure): chan=%s ts=%s: %v", channelID, ts, err)
		return
	}
	if !resp.OK {
		log.Printf("react warning (alias dispatch failure): chan=%s ts=%s: slack error=%s", channelID, ts, resp.Error)
	}
}

// handleIdentity serves POST /identity. The caller (gc slack identity)
// supplies a session_id and zero or more of {username, icon_url, icon_emoji}.
// The record is stored in the registry and persisted; subsequent publishes
// keyed by the same session_id pick up the override.
func handleIdentity(reg *identityRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req identityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.SessionID) == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		rec := identityRecord{
			Username:  req.Username,
			IconURL:   req.IconURL,
			IconEmoji: req.IconEmoji,
		}
		if err := reg.Set(req.SessionID, rec); err != nil {
			log.Printf("identity store error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("identity: session=%s username=%q icon_url=%q icon_emoji=%q",
			req.SessionID, rec.Username, rec.IconURL, rec.IconEmoji)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identityReceipt{Stored: true, SessionID: req.SessionID})
	}
}

// handleIdentityDelete serves DELETE /identity. The session id is taken
// from either ?session_id= query param (preferred) or JSON body
// { "session_id": "..." }. Idempotent: missing entries return Existed=false
// without error.
func handleIdentityDelete(reg *identityRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if sessionID == "" {
			var req identityRequest
			if r.ContentLength > 0 {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
					return
				}
				sessionID = strings.TrimSpace(req.SessionID)
			}
		}
		if sessionID == "" {
			http.Error(w, "session_id is required (?session_id= or JSON body)", http.StatusBadRequest)
			return
		}
		existed, err := reg.Delete(sessionID)
		if err != nil {
			log.Printf("identity delete error: %v", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		log.Printf("identity delete: session=%s existed=%v", sessionID, existed)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(identityDeleteReceipt{
			Removed:   true,
			Existed:   existed,
			SessionID: sessionID,
		})
	}
}

// gcForwardClient bounds every adapter→gc control-plane call
// (postInbound, adapter registration, alias dispatch). These ran on
// http.DefaultClient with no timeout, which let a hung gc keep a
// dedup claim undecided indefinitely — parking acked Slack
// redeliveries forever (codex r4). 20 seconds is generous for a
// same-host API; a timeout surfaces as a normal forward failure,
// which forgets the claim so a redelivery can take over.
var gcForwardClient = &http.Client{Timeout: 20 * time.Second}

// slackAPIClient bounds Slack Web API JSON calls (chat.postMessage,
// reactions, ephemerals, upload bookkeeping). Every synchronous call
// reachable while a dedup claim is open must conclude, or parked
// redeliveries wait forever (codex r5); the rest simply shouldn't
// hang goroutines on a stalled upstream either.
var slackAPIClient = &http.Client{Timeout: 30 * time.Second}

// slackUploadClient bounds the pre-signed file-bytes POST, which
// carries whole file payloads and deserves a longer leash than the
// JSON calls.
var slackUploadClient = &http.Client{Timeout: 120 * time.Second}

// postInbound forwards one inbound to gc, discarding the delivery
// receipt. For call sites that take no same-ts claim (peer-bot context,
// reaction notifications) there is nothing a receipt could gate.
func postInbound(cfg config, msg externalInboundMessage) error {
	_, err := postInboundWithReceipt(cfg, msg)
	return err
}

// postInboundWithReceipt forwards one inbound and returns gc's delivery
// receipt alongside the transport outcome (gp-32q). A nil error means
// gc ACCEPTED the payload; whether it REACHED the session is the
// receipt's question, and only a claim-holding caller needs to ask it —
// see delivery_receipt.go. A receipt-less gc yields the zero receipt
// (present=false), which every gate treats as the legacy 2xx verdict.
func postInboundWithReceipt(cfg config, msg externalInboundMessage) (deliveryReceipt, error) {
	body, _ := json.Marshal(map[string]any{
		"message": msg,
	})
	// PathEscape cityName for the same reason as registerAdapter (gc-cby.28).
	target := fmt.Sprintf("%s/v0/city/%s/extmsg/inbound", cfg.gcAPIBase, url.PathEscape(cfg.cityName))
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return deliveryReceipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "gc-slack-adapter")
	// Asks a receipt-emitting gc to vouch for the last hop before it
	// answers. Ignored by every gc that predates gp-2rq, which is
	// exactly the "unsupported" arm.
	req.Header.Set("X-GC-Delivery-Receipt", "require")
	resp, err := gcForwardClient.Do(req)
	if err != nil {
		return deliveryReceipt{}, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, deliveryReceiptBodyLimit))
	if resp.StatusCode >= 400 {
		return deliveryReceipt{}, &inboundPostError{Status: resp.StatusCode, StatusText: resp.Status, Body: string(respBody)}
	}
	if readErr != nil {
		// gc accepted (2xx headers) but the body did not finish. The
		// message is not lost — but nothing here vouches for the last
		// hop either, so report the zero receipt and let the caller's
		// gate treat it as an unsupported/legacy conclusion rather
		// than inventing a delivery claim from a truncated body.
		log.Printf("inbound receipt: response body read failed (delivery still accepted): %v", readErr)
		return deliveryReceipt{}, nil
	}
	return parseDeliveryReceipt(respBody), nil
}
