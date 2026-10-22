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
	Identities    []Identity `toml:"identity"`
	Spaces        []Space    `toml:"space"`
	Media         Media      `toml:"media"`
	CatchupBudget int        `toml:"catchup_budget"` // per space per pass, default 50
}

type Config struct {
	Secrets    Secrets
	Settings   Settings
	Endpoints  Endpoints
	SecretsDir string
}

// Load reads KEY=VALUE secrets and TOML settings. A missing settings file uses
// defaults; missing secrets and malformed files are errors. Endpoint overrides
// and gc's service coordinates come from the process environment.
func Load(envFile, settingsFile, secretsDir string) (*Config, error) {
	values, err := readEnv(envFile)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, key := range []string{"GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"} {
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
	c.Settings.Defaults.Wake = WakeAll
	c.Settings.Media.MaxBytes = 209715200
	c.Settings.CatchupBudget = 50
	metadata, err := toml.DecodeFile(settingsFile, &c.Settings)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("settings %s: %w", settingsFile, err)
	}
	if unknown := metadata.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("settings %s: unknown key %s", settingsFile, unknown[0])
	}
	if len(c.Settings.Identities) == 0 {
		c.Settings.Identities = []Identity{{Name: "default", Agent: "mayor"}}
	}
	if err := c.validateSettings(); err != nil {
		return nil, fmt.Errorf("settings %s: %w", settingsFile, err)
	}
	for i := range c.Settings.Identities {
		identity := &c.Settings.Identities[i]
		if identity.TokenFile == "" {
			identity.TokenFile = c.Secrets.TokenFile
		} else if !filepath.IsAbs(identity.TokenFile) {
			identity.TokenFile = filepath.Join(secretsDir, identity.TokenFile)
		}
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
	c.Endpoints = Endpoints{
		ChatAPIBase:    envOr("GCHAT_CHAT_API_BASE", "https://chat.googleapis.com"),
		EventsAPIBase:  envOr("GCHAT_EVENTS_API_BASE", "https://workspaceevents.googleapis.com"),
		PubSubEndpoint: os.Getenv("GCHAT_PUBSUB_ENDPOINT"),
		OAuthAuthURL:   envOr("GCHAT_OAUTH_AUTH_URL", "https://accounts.google.com/o/oauth2/auth"),
		OAuthTokenURL:  envOr("GCHAT_OAUTH_TOKEN_URL", "https://oauth2.googleapis.com/token"),
		MediaDir:       envOr("GCHAT_MEDIA_DIR", mediaDir), UploadRoot: envOr("GCHAT_UPLOAD_ROOT", uploadRoot),
		StateFile: envOr("GCHAT_STATE_FILE", filepath.Join(secretsDir, "..", "state.json")),
	}
	// Both public views expose the same effective paths. Environment overrides
	// take precedence over TOML, which takes precedence over city defaults.
	c.Settings.Media.InboundDir = c.Endpoints.MediaDir
	c.Settings.Media.UploadRoot = c.Endpoints.UploadRoot
	return c, nil
}

// Wake returns a space override, or the default when no override is set.
func (c *Config) Wake(spaceID string) WakePolicy {
	for _, space := range c.Settings.Spaces {
		if space.ID == spaceID && space.Wake != "" {
			return space.Wake
		}
	}
	return c.Settings.Defaults.Wake
}

// Identity returns the first configured identity. Load always provides one.
func (c *Config) Identity() Identity {
	return c.Settings.Identities[0]
}

func (c *Config) validateSettings() error {
	validWake := func(wake WakePolicy) bool { return wake == WakeAll || wake == WakeMentions }
	if !validWake(c.Settings.Defaults.Wake) {
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
	if c.Settings.Media.MaxBytes <= 0 {
		return fmt.Errorf("media.max_bytes must be positive")
	}
	if c.Settings.CatchupBudget <= 0 {
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
