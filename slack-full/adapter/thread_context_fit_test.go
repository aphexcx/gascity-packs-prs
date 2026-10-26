package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestThreadContextFitGoldens(t *testing.T) {
	for _, tc := range []struct {
		name, preamble, body string
		budget               int
	}{
		{"small-context-long-post", "Thread context (1 earlier message):\n@human: synthetic prior\n\n---\n\n", strings.Repeat("new report ", 330) + "end", 3500},
		{"newest-with-counted-notices", "Thread context: 2 earlier messages already delivered.\nThread context (4 earlier messages):\n@human: oldest synthetic report\n@robot: second synthetic report\n@human: third synthetic report\n@robot: newest synthetic report\n\n---\n\n" + threadContextIncompleteNotice, "new post", 150},
		{"cjk-newest-clipped", "Thread context (1 earlier message):\n@robot: " + strings.Repeat("报告", 90) + "\n\n---\n\n", "new post", 100},
		{"human-equal-length", "Thread context (2 earlier messages):\n@robot: " + strings.Repeat("older ", 30) + "\n@human: " + strings.Repeat("newer ", 30) + "\n\n---\n\n", "new post", 140},
		{"bot-equal-length", "Thread context (2 earlier messages):\n@human: " + strings.Repeat("older ", 30) + "\n@robot: " + strings.Repeat("newer ", 30) + "\n\n---\n\n", "new post", 140},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := channelReminderParts{anchor: "[reply in thread 100.1]", preamble: tc.preamble, body: tc.body, files: "[file: example.txt]"}
			got, _, _, _, trimmed := composeChannelReminderText(parts, "", "", tc.budget)
			if trimmed || !utf8.ValidString(got) || !strings.Contains(got, tc.body) || !strings.HasSuffix(got, parts.files) {
				t.Fatal("protected unit lost or invalid UTF-8")
			}
			path := filepath.Join("testdata", "context-fit", tc.name+".golden")
			if os.Getenv("UPDATE_CONTEXT_GOLDENS") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got+"\n" != string(want) {
				t.Fatalf("golden mismatch:\n%s", got)
			}
		})
	}
}

func TestThreadContextFitBudgetsAndAuthorParity(t *testing.T) {
	for budget := 1; budget < 400; budget++ {
		preamble := "Thread context: 1 earlier message already delivered.\nThread context (2 earlier messages):\n@older: " + strings.Repeat("界", 80) + "\n@human: " + strings.Repeat("é", 100) + "\n\n---\n\n" + threadContextIncompleteNotice
		human := fitThreadContext(preamble, budget)
		bot := fitThreadContext(strings.Replace(preamble, "@human", "@robot", 1), budget)
		labelFits := budget >= len("… [truncated]\n")+len(contextOmissionNotice(3))+1+len("@human")
		if len(human) != len(bot) || (labelFits && strings.Replace(human, "@human", "@robot", 1) != bot) || !utf8.ValidString(human) {
			t.Fatalf("budget %d split a rune or preferred an author", budget)
		}
		if len(human) > max(budget, len("… [truncated]\n")+len(contextOmissionNotice(3))+1) {
			t.Fatalf("budget %d exceeded beyond mandatory diagnostics: %d", budget, len(human))
		}
		if strings.Count(human, "omitted — thread context over budget") != 1 {
			t.Fatalf("budget %d lost its single counted remainder", budget)
		}
	}
}

func TestReminderContextFloorConfigAndBoundaries(t *testing.T) {
	filesOnly := channelReminderParts{preamble: "@robot: " + strings.Repeat("x", 300) + "\n\n", files: "[file: example.txt]"}
	if got, _, _, _, _ := composeChannelReminderText(filesOnly, "", "", 100); len(got) > 100 || !strings.HasSuffix(got, filesOnly.files) {
		t.Fatal("files-only context did not reserve its attachment separator")
	}
	preamble := "Thread context (2 earlier messages):\n@a: x\n@b: y\n\n---\n\n"
	if got := fitThreadContext(preamble, 20); got != "@a: x\n@b: y\n\n" {
		t.Fatalf("unnecessary count displaced a window that fits: %q", got)
	}
	for _, value := range []string{"", "80", "0", "-1", "bad"} {
		env := baseSlackEnv()
		env["SLACK_REMINDER_CONTEXT_FLOOR"] = value
		cfg, err := loadConfigFromEnv(stubEnv(env))
		if value == "0" || value == "-1" || value == "bad" {
			if err == nil {
				t.Fatalf("accepted floor %q", value)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := 1200
		if value == "80" {
			want = 80
		}
		if cfg.reminderContextFloor != want {
			t.Fatalf("floor=%d, want %d", cfg.reminderContextFloor, want)
		}
	}
	parts := channelReminderParts{body: strings.Repeat("b", 100), preamble: "@human: " + strings.Repeat("界", 100) + "\n\n"}
	for _, budget := range []int{99, 100, 101, 0} {
		got, _, _, _, _ := composeChannelReminderText(parts, "", "", budget, 80)
		context := strings.TrimSuffix(got, parts.body)
		if budget == 0 && context != parts.preamble {
			t.Fatal("disabled budget changed context")
		}
		if budget <= 100 && budget > 0 && len(context) > 80 {
			t.Fatal("custom context floor not applied")
		}
		if budget == 101 && !strings.Contains(context, "[truncated]") {
			t.Fatal("positive remainder incorrectly used floor")
		}
	}
}
