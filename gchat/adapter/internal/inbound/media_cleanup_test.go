package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/media"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// The safety boundary is exercised before the functional cleanup cases.
func TestRemovalStaysInsideTheMessageFolder(t *testing.T) {
	for _, hazard := range []string{"file_link", "message_link", "space_link", "root_link", "nested", "prefix", "dot"} {
		t.Run(hazard, func(t *testing.T) {
			f := setup(t)
			root := f.opts.Config.Endpoints.MediaDir
			outside := filepath.Join(filepath.Dir(root), "outside")
			keep := writeCleanupFile(t, outside, "keep.txt")
			name := "one"
			if hazard == "dot" {
				name = "one.part"
			}
			dir := filepath.Join(root, "room", name)
			sibling := writeCleanupFile(t, filepath.Join(root, "room", name+"-other"), "keep.txt")
			other := writeCleanupFile(t, filepath.Join(root, "other", name), "keep.txt")
			var saved string
			switch hazard {
			case "message_link":
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			case "space_link", "root_link":
				link := root
				if hazard == "space_link" {
					link = filepath.Join(root, "room")
				}
				if err := os.Rename(link, link+"-kept"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				if hazard == "root_link" {
					sibling = strings.Replace(sibling, root, root+"-kept", 1)
					other = strings.Replace(other, root, root+"-kept", 1)
				} else {
					sibling = strings.Replace(sibling, filepath.Join(root, "room"), filepath.Join(root, "room-kept"), 1)
				}
			default:
				saved = writeCleanupFile(t, dir, "saved.txt")
				if hazard == "file_link" {
					if err := os.Symlink(keep, filepath.Join(dir, "link")); err != nil {
						t.Fatal(err)
					}
				}
				if hazard == "nested" {
					keep = writeCleanupFile(t, filepath.Join(dir, "nested"), "keep.txt")
				}
			}
			m := &api.Message{Name: space + "/messages/" + name}
			handle(t, f, event("delete", "message.deleted", m))
			for _, path := range []string{keep, sibling, other} {
				assertCleanupFile(t, path)
			}
			if saved != "" {
				assertCleanupGone(t, saved)
			}
		})
	}
}

func TestRemovalRefusesUnsafeIds(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../one", "one/extra", "one\\extra", "one\x00"} {
		for _, part := range []string{"space", "message"} {
			t.Run(part+"/"+strings.ReplaceAll(id, "\x00", "NUL"), func(t *testing.T) {
				f := setup(t)
				keep := writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one"), "keep.txt")
				room, msg := "room", "one"
				if part == "space" {
					room = id
				} else {
					msg = id
				}
				out, err := f.p.HandleChange(context.Background(), puller.Change{Kind: "message.deleted", Space: "spaces/" + room, Message: &api.Message{Name: "spaces/" + room + "/messages/" + msg}})
				if out != puller.Quarantine || err == nil {
					t.Fatal("unsafe id accepted")
				}
				assertCleanupFile(t, keep)
			})
		}
	}
}

func writeCleanupFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("fixture bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCleanupFile(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "fixture bytes" {
		t.Fatal("unrelated or retained file changed")
	}
}

func assertCleanupGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("saved file or message directory remains")
	}
}

func cleanupAttachment(m *api.Message, id string) *api.Attachment {
	return &api.Attachment{Name: m.Name + "/attachments/" + id, ContentName: id + ".txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: space + "/media/" + id}}
}

func saveCleanupMessage(t *testing.T, f *fixture, m *api.Message, kind, transport string) []string {
	t.Helper()
	f.google.Configure(func(g *fakegoogle.Server) {
		for _, a := range m.Attachment {
			g.Media[a.AttachmentDataRef.ResourceName] = []byte("fixture bytes")
		}
	})
	handle(t, f, event(transport, kind, m))
	turns := f.gc.Inbound()
	var paths []string
	for _, a := range turns[len(turns)-1].Attachments {
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "file" {
			t.Fatal("attachment not saved")
		}
		paths = append(paths, u.Path)
	}
	return paths
}

func TestDeletedMessageRemovesItsSavedFiles(t *testing.T) {
	f := setup(t)
	m := message()
	m.Attachment = []*api.Attachment{cleanupAttachment(m, "file")}
	paths := saveCleanupMessage(t, f, m, "message.created", "create")
	root := f.opts.Config.Endpoints.MediaDir
	kept := []string{writeCleanupFile(t, filepath.Join(root, "room", "two"), "keep.txt"), writeCleanupFile(t, filepath.Join(root, "elsewhere", "one"), "keep.txt")}
	before, err := media.Usage(root)
	if err != nil {
		t.Fatal(err)
	}
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: m.Name}))
	assertCleanupGone(t, paths[0])
	assertCleanupGone(t, filepath.Dir(paths[0]))
	for _, path := range kept {
		assertCleanupFile(t, path)
	}
	for _, dir := range []string{root, filepath.Join(root, "room")} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatal("parent removed")
		}
	}
	after, err := media.Usage(root)
	if err != nil || after >= before {
		t.Fatal("cleanup did not free quota")
	}
	turns := f.gc.Inbound()
	if len(turns) != 2 || turns[1].Text != framed("deleted") {
		t.Fatal("deletion turn changed")
	}
}

func TestDeletionWithNoSavedFilesChangesNothing(t *testing.T) {
	for _, row := range []string{"plain", "drive", "unseen", "own"} {
		t.Run(row, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.p.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			m := message()
			if row == "own" {
				m.Sender.Name = identity
			}
			if row == "drive" {
				m.Attachment = []*api.Attachment{{Name: m.Name + "/attachments/drive", DriveDataRef: &api.DriveDataRef{DriveFileId: "drive-id"}}}
			}
			if row != "unseen" {
				handle(t, f, event("create", "message.created", m))
			}
			keep := writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "two"), "keep.txt")
			handle(t, f, event("delete", "message.deleted", &api.Message{Name: m.Name}))
			assertCleanupFile(t, keep)
			if logs.Len() != 0 {
				t.Fatal("empty removal logged")
			}
		})
	}
}

func TestDeletionRemovesEveryCopy(t *testing.T) {
	f := setup(t)
	m := message()
	m.Attachment = []*api.Attachment{cleanupAttachment(m, "file")}
	first := saveCleanupMessage(t, f, m, "message.created", "create")
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	second := saveCleanupMessage(t, f, m, "message.updated", "edit")
	if first[0] == second[0] {
		t.Fatal("copy policy changed")
	}
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: m.Name}))
	for _, path := range append(first, second...) {
		assertCleanupGone(t, path)
	}
}

func TestDeletionLearnedAfterAnOutageRemovesFiles(t *testing.T) {
	f := setup(t)
	path := writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one"), "saved.txt")
	out, err := f.p.HandleChange(context.Background(), puller.Change{Kind: "message.deleted", Space: space, Message: &api.Message{Name: messageName}})
	if err != nil || out != puller.Ack {
		t.Fatal("catch-up deletion failed")
	}
	assertCleanupGone(t, path)
}

func TestRemovalLogHoldsIdsOnly(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed", true: "failed"}[fail], func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.p.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			dir := filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one")
			writeCleanupFile(t, dir, "private-filename.txt")
			if fail {
				writeCleanupFile(t, filepath.Join(dir, "private-directory"), "private-filename.txt")
			}
			handle(t, f, event("delete", "message.deleted", &api.Message{Name: messageName}))
			if logs.Len() == 0 {
				t.Fatal("removal was silent")
			}
			for _, secret := range []string{"private-filename", "private-directory", "fixture bytes", "Reader", dir} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("cleanup log exposed private data")
				}
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			want := 1
			if fail {
				want = 2
			}
			if len(lines) != want {
				t.Fatal("unexpected cleanup log count")
			}
			for _, line := range lines {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) != nil || record["message"] != messageName {
					t.Fatal("missing resource id")
				}
			}
		})
	}
}

func TestEditKeepsFilesUnlessTheAttachmentWasRemoved(t *testing.T) {
	for _, row := range []string{"text", "one", "all", "stale", "local", "legacy", "restart"} {
		t.Run(row, func(t *testing.T) {
			f := setup(t)
			m := message()
			m.Attachment = []*api.Attachment{cleanupAttachment(m, "first"), cleanupAttachment(m, "second")}
			paths := saveCleanupMessage(t, f, m, "message.created", "create")
			legacy := writeCleanupFile(t, filepath.Dir(paths[0]), "legacy.txt")
			if row == "restart" {
				restartCleanupProcessor(t, f)
			}
			m.LastUpdateTime = "2026-10-22T10:01:00Z"
			m.Text = "updated"
			if row != "text" {
				m.Attachment = m.Attachment[1:]
			}
			if row == "all" {
				m.Attachment = nil
			}
			if row == "stale" {
				m.LastUpdateTime = m.CreateTime
			}
			if row == "local" {
				m.Sender.Name = identity
			}
			handle(t, f, event("edit", "message.updated", m))
			if row == "text" || row == "stale" {
				assertCleanupFile(t, paths[0])
			} else {
				assertCleanupGone(t, paths[0])
			}
			if row == "all" {
				assertCleanupGone(t, paths[1])
			} else {
				assertCleanupFile(t, paths[1])
			}
			assertCleanupFile(t, legacy)
		})
	}
}

func TestFailedRemovalIsLoggedAndRetried(t *testing.T) {
	f := setup(t)
	m := message()
	m.Attachment = []*api.Attachment{cleanupAttachment(m, "file")}
	paths := saveCleanupMessage(t, f, m, "message.created", "create")
	// A foreign directory injects a deterministic removal refusal. No permissions change.
	block := filepath.Join(filepath.Dir(paths[0]), "blocked")
	if err := os.Mkdir(block, 0700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	f.p.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: m.Name}))
	if !strings.Contains(logs.String(), "WARN") {
		t.Fatal("failed removal was silent")
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	// A redelivery with a fresh transport id reaches the existing tombstone.
	handle(t, f, event("delete-again", "message.deleted", &api.Message{Name: m.Name}))
	if err := f.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCleanupGone(t, filepath.Dir(paths[0]))
	if len(f.gc.Inbound()) != 2 || !f.l.Messages[m.Name].Deleted {
		t.Fatal("deletion lost or doubled")
	}
}

func TestFailedRemovalDoesNotStopOtherMessages(t *testing.T) {
	f := setup(t)
	writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one", "blocked"), "keep.txt")
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: messageName}))
	m := message()
	m.Name = space + "/messages/two"
	handle(t, f, event("next", "message.created", m))
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("cleanup blocked another message")
	}
}

func restartCleanupProcessor(t *testing.T, f *fixture) {
	t.Helper()
	l, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.l = l
	f.opts.Ledger = l
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteAfterRestartRemovesSavedFiles(t *testing.T) {
	f := setup(t)
	m := message()
	m.Attachment = []*api.Attachment{cleanupAttachment(m, "file")}
	paths := saveCleanupMessage(t, f, m, "message.created", "create")
	restartCleanupProcessor(t, f)
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: m.Name}))
	assertCleanupGone(t, paths[0])
}

func TestStopBetweenTombstoneAndRemovalIsFinished(t *testing.T) {
	f := setup(t)
	path := writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one"), "saved.txt")
	// A durable checkpoint from the new writer, before the cleanup attempt.
	f.l.Messages[messageName] = &ledger.MessageState{Deleted: true}
	b, err := json.Marshal(f.l)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw["media"] = json.RawMessage(`{"spaces/room/messages/one":{"delete_pending":true}}`)
	b, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.opts.LedgerPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	restartCleanupProcessor(t, f)
	if err = f.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCleanupGone(t, path)
}

func TestStartLeavesFilesOfMessagesDeletedBefore(t *testing.T) {
	f := setup(t)
	path := writeCleanupFile(t, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room", "one"), "legacy.txt")
	f.l.Messages[messageName] = &ledger.MessageState{Deleted: true}
	if err := f.l.Save(f.opts.LedgerPath); err != nil {
		t.Fatal(err)
	}
	restartCleanupProcessor(t, f)
	if err := f.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCleanupFile(t, path)
}
