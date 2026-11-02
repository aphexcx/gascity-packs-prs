package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultStateFileRetainsSecretsDirectoryBase(t *testing.T) {
	env, settings, _ := configFixture(t)
	c, err := Load(env, settings, "relative-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("relative-secrets", "..", "state.json"); c.Endpoints.StateFile != want {
		t.Fatalf("default state path=%q want %q", c.Endpoints.StateFile, want)
	}
}

func TestRelativeStateFileUsesEnvFileDirectory(t *testing.T) {
	for _, process := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "process"}[process], func(t *testing.T) {
			env, settings, dir := configFixture(t)
			contents, err := os.ReadFile(env)
			if err != nil {
				t.Fatal(err)
			}
			writeFixture(t, env, string(contents)+"GCHAT_STATE_FILE=../state/ledger.json\n")
			relative := "../state/ledger.json"
			if process {
				relative = "other/ledger.json"
				t.Setenv("GCHAT_STATE_FILE", relative)
			}
			// A caller can locate credentials separately from the env file.
			c, err := Load(env, settings, filepath.Join(dir, "credentials"))
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(filepath.Dir(env), relative); c.Endpoints.StateFile != want {
				t.Fatalf("state=%q want %q", c.Endpoints.StateFile, want)
			}
		})
	}
}

func TestEnvFileEndpointOverrides(t *testing.T) {
	env, settings, dir := configFixture(t)
	contents, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"GCHAT_CHAT_API_BASE": "http://127.0.0.1:1", "GCHAT_EVENTS_API_BASE": "http://127.0.0.1:2", "GCHAT_PUBSUB_ENDPOINT": "127.0.0.1:3",
		"GCHAT_MEDIA_DIR": "/tmp/file-media", "GCHAT_UPLOAD_ROOT": "/tmp/file-upload", "GCHAT_STATE_FILE": "/tmp/file-state.json",
	}
	for key, value := range values {
		t.Setenv(key, "")
		contents = append(contents, []byte(key+"="+value+"\n")...)
	}
	writeFixture(t, env, string(contents))
	writeFixture(t, settings, "[media]\ninbound_dir='/tmp/settings-media'\nupload_root='/tmp/settings-upload'\n")
	check := func() {
		t.Helper()
		c, err := Load(env, settings, dir)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{"GCHAT_CHAT_API_BASE": c.Endpoints.ChatAPIBase, "GCHAT_EVENTS_API_BASE": c.Endpoints.EventsAPIBase, "GCHAT_PUBSUB_ENDPOINT": c.Endpoints.PubSubEndpoint, "GCHAT_MEDIA_DIR": c.Endpoints.MediaDir, "GCHAT_UPLOAD_ROOT": c.Endpoints.UploadRoot, "GCHAT_STATE_FILE": c.Endpoints.StateFile}
		if !reflect.DeepEqual(got, values) {
			t.Fatalf("effective overrides: %v want %v", got, values)
		}
	}
	check()
	for key, value := range values {
		values[key] = value + "-process"
		t.Setenv(key, values[key])
	}
	check()
}
