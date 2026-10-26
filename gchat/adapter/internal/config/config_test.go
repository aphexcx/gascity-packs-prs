package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gchat-adapter/internal/fakes/fakegc"

	"github.com/BurntSushi/toml"
)

func TestLoadWithoutSettingsFile(t *testing.T) {
	env, _, dir := configFixture(t)
	c, err := Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Identity().TokenFile != filepath.Join(dir, "token.json") || c.Wake("spaces/example") != WakeAll || c.CatchupBudget() != 50 || c.Endpoints.MediaMaxBytes != 209715200 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestMissingEnvFileNamesPathAndRequiredKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-env")
	_, err := Load(path, "", dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
	for _, want := range []string{path, "GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestAuthEndpointOverridesInSecretsFile(t *testing.T) {
	env, _, dir := configFixture(t)
	body, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, env, string(body)+"GCHAT_OAUTH_AUTH_URL=http://127.0.0.1:1/authorize\nGCHAT_OAUTH_TOKEN_URL=http://127.0.0.1:1/token\n")
	c, err := Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoints.OAuthAuthURL != "http://127.0.0.1:1/authorize" || c.Endpoints.OAuthTokenURL != "http://127.0.0.1:1/token" {
		t.Fatalf("file endpoint overrides ignored: %+v", c.Endpoints)
	}
	t.Setenv("GCHAT_OAUTH_AUTH_URL", "http://127.0.0.1:2/authorize")
	t.Setenv("GCHAT_OAUTH_TOKEN_URL", "http://127.0.0.1:2/token")
	c, err = Load(env, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoints.OAuthAuthURL != "http://127.0.0.1:2/authorize" || c.Endpoints.OAuthTokenURL != "http://127.0.0.1:2/token" {
		t.Fatalf("process overrides lost precedence: %+v", c.Endpoints)
	}
}

func TestLoadSecretsAndSettings(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	os.WriteFile(env, []byte("GCHAT_PROJECT_ID=proj\nGCHAT_PUBSUB_TOPIC=projects/proj/topics/t\nGCHAT_PUBSUB_SUBSCRIPTION=projects/proj/subscriptions/s\nGCHAT_OAUTH_CLIENT_FILE="+filepath.Join(dir, "client.json")+"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL="+fakegc.New(t).URL()+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[defaults]
wake = "mentions"
[[identity]]
name = "example"
agent = "mayor"
[[space]]
id = "spaces/AAA"
wake = "all"
[[space]]
id = "spaces/BBB"
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
	if c.Wake("spaces/AAA") != WakeAll || c.Wake("spaces/BBB") != WakeMentions || c.Wake("spaces/ZZZ") != WakeMentions {
		t.Fatal("wake policy")
	}
	if c.Settings.Media.MaxBytes != 1000 || c.Endpoints.MediaMaxBytes != 1000 || c.Endpoints.ChatAPIBase != "https://chat.googleapis.com" {
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

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configFixture(t *testing.T) (env, settings, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "examplecity", ".gc", "services", "gchat", "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GC_CITY_PATH", "GC_SERVICE_SOCKET", "GC_SERVICE_URL_PREFIX", "GCHAT_CHAT_API_BASE", "GCHAT_EVENTS_API_BASE", "GCHAT_PUBSUB_ENDPOINT", "GCHAT_OAUTH_AUTH_URL", "GCHAT_OAUTH_TOKEN_URL", "GCHAT_MEDIA_DIR", "GCHAT_UPLOAD_ROOT", "GCHAT_STATE_FILE"} {
		t.Setenv(key, "")
	}
	env = filepath.Join(dir, "env")
	settings = filepath.Join(dir, "..", "config.toml")
	writeFixture(t, settings, "")
	writeFixture(t, env, "\n # a comment\nGCHAT_PROJECT_ID = 'proj'\nGCHAT_PUBSUB_TOPIC=projects/proj/topics/t\nGCHAT_PUBSUB_SUBSCRIPTION=projects/proj/subscriptions/s\nGCHAT_OAUTH_CLIENT_FILE=\"client=one #literal.json\"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL="+fakegc.New(t).URL()+"\n")
	return
}

func TestEmptySettingsDefaults(t *testing.T) {
	env, settings, dir := configFixture(t)
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Secrets.ProjectID != "proj" || c.Secrets.OAuthClientFile != "client=one #literal.json" || c.SecretsDir != dir {
		t.Fatalf("secrets: %+v, dir: %s", c.Secrets, c.SecretsDir)
	}
	if !reflect.DeepEqual(c.Settings, Settings{}) {
		t.Fatalf("omitted settings: %+v, want zero values", c.Settings)
	}
	wantIdentity := Identity{Name: "default", Agent: "mayor", TokenFile: c.Secrets.TokenFile}
	if !reflect.DeepEqual(c.Identities(), []Identity{wantIdentity}) || c.Identity() != wantIdentity {
		t.Fatalf("default identity: %+v", c.Identities())
	}
	identities := c.Identities()
	identities[0].Name = "changed"
	if c.Identity() != wantIdentity || len(c.Settings.Identities) != 0 {
		t.Fatal("effective default identity aliases settings or another call")
	}
	if c.Wake("spaces/AAA") != WakeAll || c.CatchupBudget() != 50 {
		t.Fatalf("defaults: %+v", c.Settings)
	}
	city := filepath.Clean(filepath.Join(dir, "../../../.."))
	wantEndpoints := Endpoints{
		ChatAPIBase: "https://chat.googleapis.com", EventsAPIBase: "https://workspaceevents.googleapis.com",
		OAuthAuthURL: "https://accounts.google.com/o/oauth2/auth", OAuthTokenURL: "https://oauth2.googleapis.com/token",
		MediaDir: filepath.Join(city, ".gc/gchat-media/inbound"), UploadRoot: filepath.Join(city, ".gc/gchat-out"),
		MediaMaxBytes: 209715200,
		StateFile:     filepath.Join(dir, "..", "state.json"),
	}
	if c.Endpoints != wantEndpoints {
		t.Fatalf("endpoints: %+v, want %+v", c.Endpoints, wantEndpoints)
	}
	if c.Settings.Media != (Media{}) {
		t.Fatalf("omitted media settings: %+v, want zero values", c.Settings.Media)
	}
	writeFixture(t, settings, "[defaults]\nwake=''\n")
	c, err = Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Defaults.Wake != "" || c.Wake("spaces/AAA") != WakeAll {
		t.Fatalf("empty default wake: %+v", c.Settings.Defaults)
	}
}

func TestSettingsRoundTripOmitsComputedValues(t *testing.T) {
	env, settings, dir := configFixture(t)
	writeFixture(t, settings, "[[space]]\nid = \"spaces/AAA\"\n")
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	var encoded strings.Builder
	if err := toml.NewEncoder(&encoded).Encode(c.Settings); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"wake", "catchup_budget", "identity", "token_file", "inbound_dir", "upload_root", "max_bytes"} {
		if strings.Contains(encoded.String(), key) {
			t.Errorf("encoded settings contain omitted key %q:\n%s", key, encoded.String())
		}
	}
	var roundTrip Settings
	if _, err := toml.Decode(encoded.String(), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip, Settings{Spaces: []Space{{ID: "spaces/AAA"}}}) {
		t.Fatalf("round-trip settings: %+v", roundTrip)
	}
}

func TestIdentityTokenPathsAndFirstEntry(t *testing.T) {
	for _, tc := range []struct{ name, token, want string }{
		{"relative", "tokens/example.json", "tokens/example.json"},
		{"absolute", "/tmp/example-token.json", "/tmp/example-token.json"},
		{"omitted", "", "secret-token.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			body, err := os.ReadFile(env)
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, env, string(body)+"GCHAT_TOKEN_FILE="+filepath.Join(dir, "secret-token.json")+"\n")
			writeFixture(t, settings, "[[identity]]\nname='example'\nagent='mayor'\ntoken_file='"+tc.token+"'\n[[identity]]\nname='second'\nagent='worker'\n")
			c, err := Load(env, settings, dir)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if !filepath.IsAbs(want) {
				want = filepath.Join(dir, want)
			}
			if c.Identity() != (Identity{Name: "example", Agent: "mayor", TokenFile: want}) {
				t.Fatalf("first identity: %+v", c.Identity())
			}
			wantSettings := []Identity{
				{Name: "example", Agent: "mayor", TokenFile: tc.token},
				{Name: "second", Agent: "worker"},
			}
			if !reflect.DeepEqual(c.Settings.Identities, wantSettings) {
				t.Fatalf("decoded identities: %+v, want %+v", c.Settings.Identities, wantSettings)
			}
			wantIdentities := []Identity{
				{Name: "example", Agent: "mayor", TokenFile: want},
				{Name: "second", Agent: "worker", TokenFile: c.Secrets.TokenFile},
			}
			identities := c.Identities()
			if !reflect.DeepEqual(identities, wantIdentities) {
				t.Fatalf("effective identities: %+v, want %+v", identities, wantIdentities)
			}
			identities[0].Name = "changed"
			identities[1].TokenFile = "changed.json"
			if !reflect.DeepEqual(c.Settings.Identities, wantSettings) || !reflect.DeepEqual(c.Identities(), wantIdentities) {
				t.Fatal("effective identities alias settings or another call")
			}
		})
	}
}

func TestEndpointAndServiceEnvironmentOverrides(t *testing.T) {
	env, settings, dir := configFixture(t)
	writeFixture(t, settings, "catchup_budget=7\n[media]\ninbound_dir='/tmp/config-in'\nupload_root='/tmp/config-out'\nmax_bytes=1024\n")
	values := map[string]string{
		"GCHAT_CHAT_API_BASE": "http://127.0.0.1:1", "GCHAT_EVENTS_API_BASE": "http://127.0.0.1:2",
		"GCHAT_PUBSUB_ENDPOINT": "127.0.0.1:3", "GCHAT_OAUTH_AUTH_URL": "http://127.0.0.1:4/auth",
		"GCHAT_OAUTH_TOKEN_URL": "http://127.0.0.1:4/token", "GCHAT_MEDIA_DIR": "/tmp/env-in",
		"GCHAT_UPLOAD_ROOT": "/tmp/env-out", "GCHAT_STATE_FILE": "/tmp/env-state.json",
		"GC_SERVICE_SOCKET": "/tmp/service.sock", "GC_SERVICE_URL_PREFIX": "/v0/city/examplecity/svc/gchat",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Endpoints{ChatAPIBase: values["GCHAT_CHAT_API_BASE"], EventsAPIBase: values["GCHAT_EVENTS_API_BASE"],
		PubSubEndpoint: values["GCHAT_PUBSUB_ENDPOINT"], OAuthAuthURL: values["GCHAT_OAUTH_AUTH_URL"],
		OAuthTokenURL: values["GCHAT_OAUTH_TOKEN_URL"], MediaDir: values["GCHAT_MEDIA_DIR"],
		UploadRoot: values["GCHAT_UPLOAD_ROOT"], MediaMaxBytes: 1024, StateFile: values["GCHAT_STATE_FILE"]}
	if c.Endpoints != want || c.Secrets.ServiceSocket != values["GC_SERVICE_SOCKET"] || c.Secrets.ServiceURLPrefix != values["GC_SERVICE_URL_PREFIX"] {
		t.Fatalf("overrides: %+v %+v", c.Endpoints, c.Secrets)
	}
	if c.Settings.Media != (Media{InboundDir: "/tmp/config-in", UploadRoot: "/tmp/config-out", MaxBytes: 1024}) || c.Settings.CatchupBudget != 7 {
		t.Fatalf("settings: %+v", c.Settings)
	}
	if c.CatchupBudget() != 7 {
		t.Fatalf("catchup budget: %d, want 7", c.CatchupBudget())
	}
	for _, key := range []string{"GCHAT_MEDIA_DIR", "GCHAT_UPLOAD_ROOT"} {
		t.Setenv(key, "")
	}
	c, err = Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoints.MediaDir != "/tmp/config-in" || c.Endpoints.UploadRoot != "/tmp/config-out" {
		t.Fatalf("TOML paths: %+v", c.Endpoints)
	}
}

func TestMediaEnvironmentOverridePreservesSettings(t *testing.T) {
	env, settings, dir := configFixture(t)
	writeFixture(t, settings, "[media]\ninbound_dir='/tmp/config-in'\n")
	t.Setenv("GCHAT_MEDIA_DIR", "/tmp/env-in")
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Media != (Media{InboundDir: "/tmp/config-in"}) {
		t.Errorf("decoded media settings: %+v", c.Settings.Media)
	}
	if c.Endpoints.MediaDir != "/tmp/env-in" {
		t.Errorf("effective media dir: %s, want /tmp/env-in", c.Endpoints.MediaDir)
	}
}

func TestMediaDefaultsUseCityPath(t *testing.T) {
	env, settings, dir := configFixture(t)
	city := filepath.Join(t.TempDir(), "examplecity")
	t.Setenv("GC_CITY_PATH", city)
	c, err := Load(env, settings, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoints.MediaDir != filepath.Join(city, ".gc/gchat-media/inbound") || c.Endpoints.UploadRoot != filepath.Join(city, ".gc/gchat-out") {
		t.Fatalf("city paths: %+v", c.Endpoints)
	}
}

func TestWakeInheritsDefault(t *testing.T) {
	c := &Config{}
	c.Settings.Defaults.Wake = WakeMentions
	c.Settings.Spaces = []Space{{ID: "spaces/ALL", Wake: WakeAll}, {ID: "spaces/INHERIT"}}
	got := []WakePolicy{c.Wake("spaces/ALL"), c.Wake("spaces/INHERIT"), c.Wake("spaces/UNKNOWN")}
	want := []WakePolicy{WakeAll, WakeMentions, WakeMentions}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wake policies: %v, want %v", got, want)
	}
}

func TestAllMissingSecretsNamedWithoutEnvironmentFallback(t *testing.T) {
	env, settings, dir := configFixture(t)
	writeFixture(t, env, "# no secrets\n")
	keys := []string{"GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"}
	for _, key := range keys {
		t.Setenv(key, "must-not-mask-missing-file-key")
	}
	_, err := Load(env, settings, dir)
	if err == nil {
		t.Fatal("expected missing keys error")
	}
	for _, name := range append(keys, env) {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q omits %s", err, name)
		}
	}
}

func TestMalformedSecretsNameFileAndKey(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"missing equals", "GCHAT_PROJECT_ID"},
		{"unterminated quote", "GCHAT_PROJECT_ID='unterminated"},
		{"over-long line", "GCHAT_PROJECT_ID=" + strings.Repeat("x", 70*1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			writeFixture(t, env, tc.line+"\n")
			_, err := Load(env, settings, dir)
			if err == nil || !strings.Contains(err.Error(), env) {
				t.Fatalf("want file error, got %v", err)
			}
			if len(tc.line) < 70*1024 && !strings.Contains(err.Error(), "GCHAT_PROJECT_ID") {
				t.Fatalf("want key error, got %v", err)
			}
		})
	}
}

func TestInvalidSettingsNameFileAndKey(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"[defaults]\nwake='sometimes'", "defaults.wake"},
		{"[[space]]\nid='spaces/AAA'\nwake='sometimes'", "space"},
		{"[[space]]\nwake='all'", "space"},
		{"[[identity]]\nagent='mayor'", "identity"},
		{"[[identity]]\nname='example'", "identity"},
		{"[media]\nmax_bytes=-1", "media.max_bytes"},
		{"[media]\nmax_bytes=0", "media.max_bytes"},
		{"catchup_budget=0", "catchup_budget must be positive"},
		{"catchup_budget=-1", "catchup_budget must be positive"},
		{"[defaults]\nwake=123", "wake"},
		{"[defaults]\nwkae='mentions'", "defaults.wkae"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			env, settings, dir := configFixture(t)
			writeFixture(t, settings, tc.body+"\n")
			_, err := Load(env, settings, dir)
			if err == nil || !strings.Contains(err.Error(), settings) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("want file and %s, got %v", tc.key, err)
			}
		})
	}
}

func TestFileReadErrors(t *testing.T) {
	env, settings, dir := configFixture(t)
	_, err := Load(env+".missing", settings, dir)
	if err == nil || !strings.Contains(err.Error(), env+".missing") {
		t.Fatalf("missing env: %v", err)
	}
	settings = filepath.Join(dir, "settings-directory")
	if err := os.Mkdir(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = Load(env, settings, dir)
	if err == nil || !strings.Contains(err.Error(), settings) {
		t.Fatalf("unreadable settings: %v", err)
	}
}

func TestMissingSettingsNamesFile(t *testing.T) {
	env, settings, dir := configFixture(t)
	_, err := Load(env, settings+".missing", dir)
	if err == nil || !strings.Contains(err.Error(), settings+".missing") {
		t.Fatalf("want missing settings file error, got %v", err)
	}
}
