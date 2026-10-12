package main

import (
	"net/url"
	"testing"
)

// TestReadBackPublishedMessageAcceptsSlackEmojiConversion pins the gf-4c9
// round-2 gate finding at publish_readback.go:298 (gf-jr0 finding 3c): an
// unkeyed publish is confirmed by a text compare folded through
// slackNormalizeText, and that fold knew Slack's entity escaping and URL
// auto-linking but not its documented Unicode-to-colon emoji conversion
// (https://docs.slack.dev/messaging/formatting-message-text/#emoji), so
// "Done 🎉" read back as "Done :tada:" and a delivered message was reported
// a permanent failure. publish-to-channel takes the unkeyed path by default,
// so this was the founder-facing publish path. The fold now treats emoji —
// Unicode or shortcode, with skin tones, keycaps, flags and ZWJ sequences —
// as the same lossy class as escaping: equal on both sides, never a reason
// to refuse; text that differs beyond the emoji is still refused.
func TestReadBackPublishedMessageAcceptsSlackEmojiConversion(t *testing.T) {
	tests := []struct {
		name    string
		posted  string
		stored  string
		wantErr bool
	}{
		{name: "party popper to :tada:", posted: "Done 🎉", stored: "Done :tada:"},
		{name: "thumbs up with a skin tone", posted: "Deploy 👍🏽 ok", stored: "Deploy :+1::skin-tone-4: ok"},
		{name: "keycap digit", posted: "Step 1️⃣ done", stored: "Step :one: done"},
		{name: "flag (regional indicators)", posted: "Region 🇺🇸 up", stored: "Region :us: up"},
		{name: "ZWJ family sequence", posted: "Team 👨‍👩‍👧 ship", stored: "Team :family_man_woman_girl: ship"},
		{name: "emoji beside an escaped entity", posted: "R&D ✅ merged <ok>", stored: "R&amp;D :white_check_mark: merged &lt;ok&gt;"},
		{name: "shortcode posted, shortcode stored", posted: "Done :tada:", stored: "Done :tada:"},
		{name: "verbatim Unicode echo", posted: "Done 🎉", stored: "Done 🎉"},
		{name: "colon-delimited time beside an emoji", posted: "Deploy at 12:30:45 ✅", stored: "Deploy at 12:30:45 :white_check_mark:"},
		{name: "adjacent shortcode runs", posted: "a 🎉 ✅ b", stored: "a :tada: :white_check_mark: b"},
		{name: "a changed timestamp is not an emoji", posted: "Deploy at 12:30:45", stored: "Deploy at 12:59:45", wantErr: true},
		{name: "a keyboard symbol is not an emoji", posted: "Press ⌘Q", stored: "Press Q", wantErr: true},
		{name: "box drawing is not an emoji", posted: "┌─┐ frame", stored: " frame", wantErr: true},
		{name: "Braille is not an emoji", posted: "Braille ⠁⠃", stored: "Braille ", wantErr: true},
		{name: "a keyboard symbol beside an emoji", posted: "Press ⌘Q ✅", stored: "Press ⌘Q :white_check_mark:"},
		{name: "a changed ratio is not an emoji", posted: "Ratio 1:2:3", stored: "Ratio 1:4:3", wantErr: true},
		{name: "different text beyond the emoji", posted: "Done 🎉", stored: "Done :tada: and more", wantErr: true},
		{name: "truncated body", posted: "Done 🎉 see the thread", stored: "Done :tada:", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newSlackReadbackServer(t, "/conversations.history", func(url.Values) {}, mustJSON(t, map[string]any{
				"ok":       true,
				"messages": []map[string]string{{"ts": "1700.001", "text": test.stored}},
			}))
			err := readBackPublishedMessage(nil, "xoxb-test", readbackTarget(test.posted))
			if (err != nil) != test.wantErr {
				t.Fatalf("readBackPublishedMessage = %v, wantErr %v (posted %q, stored %q)", err, test.wantErr, test.posted, test.stored)
			}
		})
	}
}

// TestSlackNormalizeTextFoldsEmojiSymmetrically pins the fold itself: the
// Unicode form and Slack's colon form of the same emoji normalize to the
// same text, and a shortcode-looking token inside ordinary text is folded
// the same on both sides rather than compared literally.
func TestSlackNormalizeTextFoldsEmojiSymmetrically(t *testing.T) {
	pairs := [][2]string{
		{"Done 🎉", "Done :tada:"},
		{"👍🏽", ":+1::skin-tone-4:"},
		{"1️⃣ first", ":one: first"},
		{"🇺🇸", ":us:"},
		{"👨‍👩‍👧", ":family_man_woman_girl:"},
		{"ok ©", "ok :copyright:"},
	}
	for _, p := range pairs {
		if a, b := slackNormalizeText(p[0]), slackNormalizeText(p[1]); a != b {
			t.Errorf("slackNormalizeText(%q) = %q, slackNormalizeText(%q) = %q; want equal", p[0], a, p[1], b)
		}
	}
	if a, b := slackNormalizeText("Done 🎉"), slackNormalizeText("Done :tada: and more"); a == b {
		t.Errorf("text beyond the emoji folded away: %q", a)
	}
	// Colon-delimited text that is not an emoji stays in the compare (the
	// lane's codex round 1): two timestamps must not fold to one string.
	if a, b := slackNormalizeText("Deploy at 12:30:45"), slackNormalizeText("Deploy at 12:59:45"); a == b {
		t.Errorf("two different timestamps folded to the same text %q", a)
	}
	if got := slackNormalizeText("Deploy at 12:30:45"); got != "Deploy at 12:30:45" {
		t.Errorf("a timestamp was folded: %q", got)
	}
	// Symbols Slack stores as typed are not emoji and stay (the lane's codex
	// round 2): ⌘, box drawing, Braille, arrows other than the emoji ones.
	for _, kept := range []string{"Press ⌘Q", "┌─┐", "⠁⠃", "a → b", "x ≠ y"} {
		if got := slackNormalizeText(kept); got != kept {
			t.Errorf("slackNormalizeText(%q) = %q, want it unchanged", kept, got)
		}
	}
}
