// Package config loads the per-city secrets (an env file) and settings (a TOML
// file). Nothing here knows any particular city.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Secrets struct {
	ProjectID        string // GCHAT_PROJECT_ID
	Topic            string // GCHAT_PUBSUB_TOPIC  projects/<id>/topics/<name>
	Subscription     string // GCHAT_PUBSUB_SUBSCRIPTION
	OAuthClientFile  string // GCHAT_OAUTH_CLIENT_FILE
	TokenFile        string // GCHAT_TOKEN_FILE, default <secrets dir>/token.json
	CityName         string // GC_CITY_NAME
	GCAPIBase        string // GC_API_BASE_URL
	ServiceSocket    string // GC_SERVICE_SOCKET (set by gc)
	ServiceURLPrefix string // GC_SERVICE_URL_PREFIX (set by gc)
}

// Endpoints holds effective values: named process environment overrides, then
// env-file overrides, then settings, then defaults. Settings stays as decoded.
type Endpoints struct {
	ChatAPIBase    string // GCHAT_CHAT_API_BASE,    default https://chat.googleapis.com
	EventsAPIBase  string // GCHAT_EVENTS_API_BASE,  default https://workspaceevents.googleapis.com
	PubSubEndpoint string // GCHAT_PUBSUB_ENDPOINT,  default "" (library default); host:port for the fake, insecure
	OAuthAuthURL   string // GCHAT_OAUTH_AUTH_URL,   default https://accounts.google.com/o/oauth2/auth
	OAuthTokenURL  string // GCHAT_OAUTH_TOKEN_URL,  default https://oauth2.googleapis.com/token
	MediaDir       string // GCHAT_MEDIA_DIR,        default <city>/.gc/gchat-media/inbound
	UploadRoot     string // GCHAT_UPLOAD_ROOT,      default <city>/.gc/gchat-out
	MediaMaxBytes  int64  // media.max_bytes,        default 209715200
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
	TokenFile string `toml:"token_file,omitempty"`
}

type Space struct {
	ID   string     `toml:"id"`
	Wake WakePolicy `toml:"wake,omitempty"`
}

type Media struct {
	InboundDir string `toml:"inbound_dir,omitempty"`
	UploadRoot string `toml:"upload_root,omitempty"`
	MaxBytes   int64  `toml:"max_bytes,omitzero"`
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
	Secrets    Secrets
	Settings   Settings
	Endpoints  Endpoints
	SecretsDir string
}

// Load reads KEY=VALUE secrets and TOML settings, preserving Settings as decoded.
// Methods and Endpoints provide effective defaults. An empty settingsFile uses
// defaults; a specified file must exist and be well formed. Endpoint overrides
// are loaded from the env file for every subcommand, with nonempty process
// overrides taking precedence. gc's service coordinates remain process-only.
func Load(envFile, settingsFile, secretsDir string) (*Config, error) {
	values, err := readEnv(envFile)
	required := []string{"GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w; required keys: %s", err, strings.Join(required, ", "))
		}
		return nil, err
	}
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
	c.Secrets = Secrets{
		ProjectID: values["GCHAT_PROJECT_ID"], Topic: values["GCHAT_PUBSUB_TOPIC"],
		Subscription: values["GCHAT_PUBSUB_SUBSCRIPTION"], OAuthClientFile: values["GCHAT_OAUTH_CLIENT_FILE"],
		TokenFile: values["GCHAT_TOKEN_FILE"], CityName: values["GC_CITY_NAME"], GCAPIBase: values["GC_API_BASE_URL"],
		ServiceSocket: os.Getenv("GC_SERVICE_SOCKET"), ServiceURLPrefix: os.Getenv("GC_SERVICE_URL_PREFIX"),
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
	catchupBudget := 50
	if metadata.IsDefined("catchup_budget") {
		catchupBudget = c.Settings.CatchupBudget
	}
	if err := c.validateSettings(mediaMaxBytes, catchupBudget); err != nil {
		return nil, fmt.Errorf("settings %s: %w", settingsFile, err)
	}
	city, err := cityPath(secretsDir)
	if err != nil {
		return nil, err
	}
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
		MediaDir:       endpoint("GCHAT_MEDIA_DIR", mediaDir), UploadRoot: endpoint("GCHAT_UPLOAD_ROOT", uploadRoot),
		MediaMaxBytes: mediaMaxBytes,
		StateFile:     endpoint("GCHAT_STATE_FILE", filepath.Join(secretsDir, "..", "state.json")),
	}
	if endpoint("GCHAT_STATE_FILE", "") != "" && !filepath.IsAbs(c.Endpoints.StateFile) {
		c.Endpoints.StateFile = filepath.Join(filepath.Dir(envFile), c.Endpoints.StateFile)
	}
	return c, nil
}

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

func (c *Config) validateSettings(mediaMaxBytes int64, catchupBudget int) error {
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
