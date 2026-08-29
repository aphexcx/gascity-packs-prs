package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// gp-xnc: Slack voice clips (file subtype "slack_audio") arrive with an
// EMPTY mimetype AND filetype — files.info for the incident file
// (F0BSX4RQFLL, 2026-08-26) reported mimetype "" filetype "" subtype
// "slack_audio" name "audio_message.m4a". gc's
// extmsg.ExternalAttachment declares mime_type as a required property,
// so the adapter must always derive one rather than pass Slack's value
// through verbatim.
func TestAttachmentMIMETypeDerivation(t *testing.T) {
	cases := []struct {
		name string
		file slackFile
		want string
	}{
		{"slack mimetype wins over extension", slackFile{Name: "x.bin", MIMEType: "image/png"}, "image/png"},
		{"slack mimetype is trimmed", slackFile{Name: "x.m4a", MIMEType: " audio/mp4 "}, "audio/mp4"},
		{"voice clip by .m4a extension", slackFile{Name: "audio_message.m4a", Subtype: "slack_audio"}, "audio/mp4"},
		{"extension is case-insensitive", slackFile{Name: "CLIP.M4A"}, "audio/mp4"},
		{"title used when name empty", slackFile{Title: "memo.mp3"}, "audio/mpeg"},
		{"slack filetype code used when no extension", slackFile{Name: "noext", Filetype: "m4a"}, "audio/mp4"},
		{"slack_audio subtype when nothing else", slackFile{Name: "noext", Subtype: "slack_audio"}, "audio/mp4"},
		{"stdlib table, charset parameter stripped", slackFile{Name: "style.css"}, "text/css"},
		{"unknown extension falls back to octet-stream", slackFile{Name: "blob.zzzunknown"}, "application/octet-stream"},
		{"no hints at all falls back to octet-stream", slackFile{ID: "F1"}, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachmentMIMEType(tc.file); got != tc.want {
				t.Fatalf("attachmentMIMEType(%+v) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// The downloaded attachment record carries the derived type, not
// Slack's empty string — this is the exact shape that produced the
// 422 storm.
func TestDownloadSlackFilesDerivesMIMETypeForVoiceClip(t *testing.T) {
	testAllowAnyURL(t)
	slackStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("AAC-BYTES"))
	}))
	t.Cleanup(slackStub.Close)

	cfg := config{
		slackBotToken:    "xoxb-test",
		inboundFileStore: filepath.Join(t.TempDir(), "inbound"),
		dispatchSem:      defaultTestDispatchSem,
	}
	files := []slackFile{{
		ID:         "F0BSX4RQFLL",
		Name:       "audio_message.m4a",
		Title:      "audio_message.m4a",
		URLPrivate: slackStub.URL + "/files/F0BSX4RQFLL",
		MIMEType:   "",
		Filetype:   "",
		Subtype:    "slack_audio",
	}}
	got := downloadSlackFiles(cfg, "C0BKF28CYUE", "1787737683.095299", files)
	if len(got) != 1 {
		t.Fatalf("got %d attachments, want 1", len(got))
	}
	if got[0].MIMEType != "audio/mp4" {
		t.Fatalf("attachment mime_type = %q, want audio/mp4", got[0].MIMEType)
	}
}

// mime_type mirrors a REQUIRED property on the gc side: it must be
// serialized even when empty so a payload can never be rejected for a
// missing key (an empty string is at worst a degraded value, never a
// 422).
func TestExternalAttachmentAlwaysSerializesMIMEType(t *testing.T) {
	raw, err := json.Marshal(externalAttachment{ProviderID: "F1", URL: "file:///tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"mime_type":""`) {
		t.Fatalf("mime_type must always be present, got %s", raw)
	}
}
