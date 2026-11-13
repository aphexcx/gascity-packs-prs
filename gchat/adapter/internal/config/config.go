// Package config loads the per-city secrets (an env file) and settings (a TOML
// file). Nothing here knows any particular city.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/redact"
	"github.com/BurntSushi/toml"
)

// TokenSource names where the identity's access tokens come from.
type TokenSource string

const (
	// TokenSourceOAuth is the default: a Desktop OAuth client and a token file
	// written by gc gchat auth.
	TokenSourceOAuth TokenSource = "oauth"
	// TokenSourceKeyholder asks a key holder over a Unix socket for tokens
	// (POST /v1/token {scopes}); the adapter never holds a refresh credential,
	// and sign-in is managed outside it.
	TokenSourceKeyholder TokenSource = "keyholder"
)

type Secrets struct {
	ProjectID        string      // GCHAT_PROJECT_ID
	Topic            string      // GCHAT_PUBSUB_TOPIC  projects/<id>/topics/<name>
	Subscription     string      // GCHAT_PUBSUB_SUBSCRIPTION
	TokenSource      TokenSource // GCHAT_TOKEN_SOURCE, default oauth
	KeyholderSocket  string      // GCHAT_KEYHOLDER_SOCKET, required for the keyholder source
	OAuthClientFile  string      // GCHAT_OAUTH_CLIENT_FILE, required for the oauth source
	TokenFile        string      // GCHAT_TOKEN_FILE, default <secrets dir>/token.json
	CityName         string      // GC_CITY_NAME
	GCAPIBase        string      // GC_API_BASE_URL
	ServiceSocket    string      // GC_SERVICE_SOCKET (set by gc)
	ServiceURLPrefix string      // GC_SERVICE_URL_PREFIX (set by gc)
	// Provenance: who counts as an owner (addresses or users/<id>, comma
	// separated) and the organization's mail domain. Optional; see the
	// provenance package. Not secrets, but written by the same hand.
	Owners             []string // GCHAT_OWNERS
	OrganizationDomain string   // GCHAT_ORGANIZATION_DOMAIN
}

// Endpoints holds effective values: named process environment overrides, then
// env-file overrides, then settings, then defaults. Settings stays as decoded.
type Endpoints struct {
	ChatAPIBase    string // GCHAT_CHAT_API_BASE,    default https://chat.googleapis.com
	EventsAPIBase  string // GCHAT_EVENTS_API_BASE,  default https://workspaceevents.googleapis.com
	PubSubEndpoint string // GCHAT_PUBSUB_ENDPOINT,  default "" (library default); host:port for the fake, insecure
	OAuthAuthURL   string // GCHAT_OAUTH_AUTH_URL,   default https://accounts.google.com/o/oauth2/auth
	OAuthTokenURL  string // GCHAT_OAUTH_TOKEN_URL,  default https://oauth2.googleapis.com/token
	PeopleAPIBase  string // GCHAT_PEOPLE_API_BASE,  default https://people.googleapis.com; the keyholder source learns its Chat identity here
	MediaDir       string // GCHAT_MEDIA_DIR,        default <city>/.gc/gchat-media/inbound
	UploadRoot     string // GCHAT_UPLOAD_ROOT,      default <city>/.gc/gchat-out
	MediaMaxBytes  int64  // media.max_bytes,        default 209715200
	// Per message and for the whole store: a space member chooses how many
	// files a message carries and how large they are, so the count is bounded
	// separately from the per-file limit and the store has one quota.
	MediaMaxAttachments int    // media.max_attachments, default 20
	MediaMaxTotalBytes  int64  // media.max_total_bytes, default 2147483648
	StateFile           string // GCHAT_STATE_FILE,     default <secrets dir>/../state.json
}

type WakePolicy string

const (
	WakeAll      WakePolicy = "all"
	WakeMentions WakePolicy = "mentions"
)

type Identity struct {
	Name      string `toml:"name"`
	Agent     string `toml:"agent"`
	TokenFile string `toml:"token_file,omitempty"`
}

type Space struct {
	ID   string     `toml:"id"`
	Wake WakePolicy `toml:"wake,omitempty"`
}

type Media struct {
	InboundDir     string `toml:"inbound_dir,omitempty"`
	UploadRoot     string `toml:"upload_root,omitempty"`
	MaxBytes       int64  `toml:"max_bytes,omitzero"`
	MaxAttachments int    `toml:"max_attachments,omitzero"`
	MaxTotalBytes  int64  `toml:"max_total_bytes,omitzero"`
}

type Settings struct {
	Defaults struct {
		Wake WakePolicy `toml:"wake,omitempty"`
	} `toml:"defaults,omitempty"`
	Identities    []Identity `toml:"identity"`
	Spaces        []Space    `toml:"space"`
	Media         Media      `toml:"media,omitempty"`         // as decoded; effective values are in Endpoints
	CatchupBudget int        `toml:"catchup_budget,omitzero"` // per space per pass, default 50
}

type Config struct {
	Acknowledgement ack.Options `json:"-"`
	Secrets         Secrets
	Settings        Settings
	Endpoints       Endpoints
	SecretsDir      string
	NeverSilent     struct {
		Enabled                                    bool
		HealthFile, OperatorContext, OperatorAgent string
	}
}

// Load reads KEY=VALUE secrets and TOML settings, preserving Settings as decoded.
// Methods and Endpoints provide effective defaults. An empty settingsFile uses
// defaults; a specified file must exist and be well formed. Endpoint overrides
// are loaded from the env file for every subcommand, with nonempty process
// overrides taking precedence. gc's service coordinates remain process-only.
func Load(envFile, settingsFile, secretsDir string) (*Config, error) {
	values, err := readEnv(envFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w; required keys: %s", err, RequiredKeys)
		}
		return nil, err
	}
	source := TokenSource(strings.TrimSpace(values["GCHAT_TOKEN_SOURCE"]))
	required := []string{"GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION"}
	switch source {
	case "", TokenSourceOAuth:
		source = TokenSourceOAuth
		required = append(required, "GCHAT_OAUTH_CLIENT_FILE")
	case TokenSourceKeyholder:
		required = append(required, "GCHAT_KEYHOLDER_SOCKET")
	default:
		return nil, fmt.Errorf("secrets %s: GCHAT_TOKEN_SOURCE must be oauth or keyholder, not %q", envFile, source)
	}
	required = append(required, "GC_CITY_NAME", "GC_API_BASE_URL")
	var missing []string
	for _, key := range required {
		if strings.TrimSpace(values[key]) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secrets %s: missing required keys: %s", envFile, strings.Join(missing, ", "))
	}
	c := &Config{SecretsDir: secretsDir}
	arrivalValue := func(key string) string {
		if value, ok := values[key]; ok {
			return value
		}
		return os.Getenv(key)
	}
	c.Acknowledgement = ack.Options{Mode: arrivalValue("ACK_REACTION"), Scope: arrivalValue("ACK_SCOPE"), Base: arrivalValue("JEV_API_BASE_URL"), Model: arrivalValue("JEV_MODEL"), Key: arrivalValue("JEV_API_KEY"), Catalog: ack.Catalog,
		// The Jev state passes Slack's secret redaction first, as on Slack.
		Redact: redact.Text}
	c.Secrets = Secrets{
		ProjectID: values["GCHAT_PROJECT_ID"], Topic: values["GCHAT_PUBSUB_TOPIC"],
		Subscription: values["GCHAT_PUBSUB_SUBSCRIPTION"], TokenSource: source,
		KeyholderSocket: values["GCHAT_KEYHOLDER_SOCKET"], OAuthClientFile: values["GCHAT_OAUTH_CLIENT_FILE"],
		TokenFile: values["GCHAT_TOKEN_FILE"], CityName: values["GC_CITY_NAME"], GCAPIBase: values["GC_API_BASE_URL"],
		ServiceSocket: os.Getenv("GC_SERVICE_SOCKET"), ServiceURLPrefix: os.Getenv("GC_SERVICE_URL_PREFIX"),
		Owners: splitList(values["GCHAT_OWNERS"]), OrganizationDomain: strings.ToLower(strings.TrimSpace(values["GCHAT_ORGANIZATION_DOMAIN"])),
	}
	if c.Secrets.TokenFile == "" {
		c.Secrets.TokenFile = "token.json"
	}
	if !filepath.IsAbs(c.Secrets.TokenFile) {
		c.Secrets.TokenFile = filepath.Join(secretsDir, c.Secrets.TokenFile)
	}
	var metadata toml.MetaData
	if settingsFile != "" {
		metadata, err = toml.DecodeFile(settingsFile, &c.Settings)
		if err != nil {
			return nil, fmt.Errorf("settings %s: %w", settingsFile, err)
		}
	}
	if unknown := metadata.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("settings %s: unknown key %s", settingsFile, unknown[0])
	}
	mediaMaxBytes := int64(209715200)
	if metadata.IsDefined("media", "max_bytes") {
		mediaMaxBytes = c.Settings.Media.MaxBytes
	}
	mediaMaxAttachments := 20
	if metadata.IsDefined("media", "max_attachments") {
		mediaMaxAttachments = c.Settings.Media.MaxAttachments
	}
	mediaMaxTotalBytes := int64(2147483648)
	if metadata.IsDefined("media", "max_total_bytes") {
		mediaMaxTotalBytes = c.Settings.Media.MaxTotalBytes
	}
	catchupBudget := 50
	if metadata.IsDefined("catchup_budget") {
		catchupBudget = c.Settings.CatchupBudget
	}
	if err := c.validateSettings(mediaMaxBytes, mediaMaxAttachments, mediaMaxTotalBytes, catchupBudget); err != nil {
		return nil, fmt.Errorf("settings %s: %w", settingsFile, err)
	}
	city, err := cityPath(secretsDir)
	if err != nil {
		return nil, err
	}
	setting := func(key string) string {
		if v, present := os.LookupEnv(key); present {
			return v
		}
		return values[key]
	}
	c.NeverSilent.Enabled = setting("NEVER_SILENT") == "on"
	c.NeverSilent.HealthFile = setting("MAYOR_HEALTH_FILE")
	if c.NeverSilent.HealthFile == "" {
		c.NeverSilent.HealthFile = filepath.Join(city, ".gc", "mayor-health.json")
	}
	c.NeverSilent.OperatorContext = setting("NEVER_SILENT_OPERATOR_CONTEXT")
	c.NeverSilent.OperatorAgent = setting("NEVER_SILENT_OPERATOR_AGENT")
	mediaDir := c.Settings.Media.InboundDir
	if mediaDir == "" {
		mediaDir = filepath.Join(city, ".gc", "gchat-media", "inbound")
	}
	uploadRoot := c.Settings.Media.UploadRoot
	if uploadRoot == "" {
		uploadRoot = filepath.Join(city, ".gc", "gchat-out")
	}
	authURL := values["GCHAT_OAUTH_AUTH_URL"]
	if authURL == "" {
		authURL = "https://accounts.google.com/o/oauth2/auth"
	}
	tokenURL := values["GCHAT_OAUTH_TOKEN_URL"]
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	endpoint := func(key, fallback string) string {
		if value := values[key]; value != "" {
			fallback = value
		}
		return envOr(key, fallback)
	}
	c.Endpoints = Endpoints{
		ChatAPIBase:    endpoint("GCHAT_CHAT_API_BASE", "https://chat.googleapis.com"),
		EventsAPIBase:  endpoint("GCHAT_EVENTS_API_BASE", "https://workspaceevents.googleapis.com"),
		PubSubEndpoint: endpoint("GCHAT_PUBSUB_ENDPOINT", ""),
		OAuthAuthURL:   endpoint("GCHAT_OAUTH_AUTH_URL", authURL),
		OAuthTokenURL:  endpoint("GCHAT_OAUTH_TOKEN_URL", tokenURL),
		PeopleAPIBase:  endpoint("GCHAT_PEOPLE_API_BASE", "https://people.googleapis.com"),
		MediaDir:       endpoint("GCHAT_MEDIA_DIR", mediaDir), UploadRoot: endpoint("GCHAT_UPLOAD_ROOT", uploadRoot),
		MediaMaxBytes: mediaMaxBytes, MediaMaxAttachments: mediaMaxAttachments, MediaMaxTotalBytes: mediaMaxTotalBytes,
		StateFile: endpoint("GCHAT_STATE_FILE", filepath.Join(secretsDir, "..", "state.json")),
	}
	if err := c.Endpoints.validate(); err != nil {
		return nil, fmt.Errorf("secrets %s: %w", envFile, err)
	}
	if endpoint("GCHAT_STATE_FILE", "") != "" && !filepath.IsAbs(c.Endpoints.StateFile) {
		c.Endpoints.StateFile = filepath.Join(filepath.Dir(envFile), c.Endpoints.StateFile)
	}
	return c, nil
}

// validate refuses an endpoint override that would put a credential or an
// unauthenticated event stream where the operator did not mean it. The
// defaults are Google's own https endpoints; an override exists for tests and
// private deployments, so plaintext is allowed only on this machine, where the
// in-memory fake runs, and the unauthenticated Pub/Sub transport only there.
func (e Endpoints) validate() error {
	for _, endpoint := range []struct{ key, value string }{
		{"GCHAT_CHAT_API_BASE", e.ChatAPIBase},
		{"GCHAT_EVENTS_API_BASE", e.EventsAPIBase},
		{"GCHAT_OAUTH_AUTH_URL", e.OAuthAuthURL},
		{"GCHAT_OAUTH_TOKEN_URL", e.OAuthTokenURL},
		{"GCHAT_PEOPLE_API_BASE", e.PeopleAPIBase},
	} {
		if err := validateEndpointURL(endpoint.key, endpoint.value); err != nil {
			return err
		}
	}
	if e.PubSubEndpoint != "" {
		host, _, err := net.SplitHostPort(e.PubSubEndpoint)
		if err != nil {
			return fmt.Errorf("GCHAT_PUBSUB_ENDPOINT must be host:port, not %q", e.PubSubEndpoint)
		}
		if !loopback(host) {
			return fmt.Errorf("GCHAT_PUBSUB_ENDPOINT may only name this machine (127.0.0.1, ::1 or localhost); its transport is unauthenticated, so %q would take events from anyone", e.PubSubEndpoint)
		}
	}
	return nil
}

// validateEndpointURL requires https, or plaintext on loopback: the Chat base
// carries the identity's bearer token, the token URL the OAuth client secret,
// and the events base decides where subscribed Chat events are delivered.
func validateEndpointURL(key, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	switch {
	case parsed.Host == "":
		return fmt.Errorf("%s must be an absolute https:// URL, not %q", key, raw)
	case parsed.User != nil:
		return fmt.Errorf("%s must not carry credentials, not %q", key, raw)
	case parsed.RawQuery != "" || parsed.Fragment != "":
		return fmt.Errorf("%s must be a bare base URL, not %q", key, raw)
	case parsed.Scheme == "https", parsed.Scheme == "http" && loopback(parsed.Hostname()):
		return nil
	default:
		return fmt.Errorf("%s must use https (plain http only on this machine), not %q", key, raw)
	}
}

// loopback reports whether a host names this machine.
func loopback(host string) bool {
	name := strings.ToLower(strings.Trim(host, "[]"))
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// RequiredKeys is the secrets contract in one line, for messages printed
// before the file can be read.
const RequiredKeys = "GCHAT_PROJECT_ID, GCHAT_PUBSUB_TOPIC, GCHAT_PUBSUB_SUBSCRIPTION, GC_CITY_NAME, GC_API_BASE_URL, and GCHAT_OAUTH_CLIENT_FILE (or GCHAT_TOKEN_SOURCE=keyholder with GCHAT_KEYHOLDER_SOCKET)"

// Wake returns a space override, then Settings.Defaults.Wake, then WakeAll.
func (c *Config) Wake(spaceID string) WakePolicy {
	for _, space := range c.Settings.Spaces {
		if space.ID == spaceID && space.Wake != "" {
			return space.Wake
		}
	}
	if c.Settings.Defaults.Wake != "" {
		return c.Settings.Defaults.Wake
	}
	return WakeAll
}

// CatchupBudget returns the configured budget per space per pass, or 50 if omitted.
func (c *Config) CatchupBudget() int {
	if c.Settings.CatchupBudget != 0 {
		return c.Settings.CatchupBudget
	}
	return 50
}

// Identities returns a fresh slice with resolved token paths, or the default
// identity when none are configured. Settings is never modified.
func (c *Config) Identities() []Identity {
	if len(c.Settings.Identities) == 0 {
		return []Identity{{Name: "default", Agent: "mayor", TokenFile: c.Secrets.TokenFile}}
	}
	identities := make([]Identity, len(c.Settings.Identities))
	copy(identities, c.Settings.Identities)
	for i := range identities {
		identity := &identities[i]
		if identity.TokenFile == "" {
			identity.TokenFile = c.Secrets.TokenFile
		} else if !filepath.IsAbs(identity.TokenFile) {
			identity.TokenFile = filepath.Join(c.SecretsDir, identity.TokenFile)
		}
	}
	return identities
}

// Identity returns the first effective identity, including the default if omitted.
func (c *Config) Identity() Identity {
	return c.Identities()[0]
}

func (c *Config) validateSettings(mediaMaxBytes int64, mediaMaxAttachments int, mediaMaxTotalBytes int64, catchupBudget int) error {
	validWake := func(wake WakePolicy) bool { return wake == WakeAll || wake == WakeMentions }
	if c.Settings.Defaults.Wake != "" && !validWake(c.Settings.Defaults.Wake) {
		return fmt.Errorf("defaults.wake must be all or mentions")
	}
	for i, space := range c.Settings.Spaces {
		if strings.TrimSpace(space.ID) == "" {
			return fmt.Errorf("space[%d].id is required", i)
		}
		if space.Wake != "" && !validWake(space.Wake) {
			return fmt.Errorf("space[%d].wake must be all or mentions", i)
		}
	}
	for i, identity := range c.Settings.Identities {
		if strings.TrimSpace(identity.Name) == "" {
			return fmt.Errorf("identity[%d].name is required", i)
		}
		if strings.TrimSpace(identity.Agent) == "" {
			return fmt.Errorf("identity[%d].agent is required", i)
		}
	}
	if mediaMaxBytes <= 0 {
		return fmt.Errorf("media.max_bytes must be positive")
	}
	if mediaMaxAttachments <= 0 {
		return fmt.Errorf("media.max_attachments must be positive")
	}
	if mediaMaxTotalBytes <= 0 {
		return fmt.Errorf("media.max_total_bytes must be positive")
	}
	if catchupBudget <= 0 {
		return fmt.Errorf("catchup_budget must be positive")
	}
	return nil
}

func readEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets %s: %w", path, err)
	}
	defer f.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("secrets %s:%d: key %q requires KEY=VALUE", path, lineNumber, key)
		}
		value = strings.TrimSpace(value)
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return nil, fmt.Errorf("secrets %s:%d: %s has unmatched quotes", path, lineNumber, key)
			}
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("secrets %s:%d: %w", path, lineNumber+1, err)
	}
	return values, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// gc supplies GC_CITY_PATH. Without it, recognize the documented service
// secrets layout; a standalone invocation uses its working directory.
func cityPath(secretsDir string) (string, error) {
	if city := os.Getenv("GC_CITY_PATH"); city != "" {
		return city, nil
	}
	path := filepath.Clean(secretsDir)
	for _, part := range []string{"secrets", "gchat", "services", ".gc"} {
		if filepath.Base(path) != part {
			cwd, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("resolve city path: %w", err)
			}
			return cwd, nil
		}
		path = filepath.Dir(path)
	}
	return path, nil
}

// splitList is a comma-separated list, trimmed, without empties.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
