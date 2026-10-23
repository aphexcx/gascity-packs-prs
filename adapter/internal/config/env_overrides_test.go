package config

import (
	"os"
	"reflect"
	"testing"
)

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
