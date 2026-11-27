package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/wire"
)

// Each way this test drives the service to put a message into Chat (a gc
// reply, a reply in a thread, a direct message, a reply split in two, the
// publish and upload commands, and the adapter's own notice) states
// MARKUP_SYNTAX_CHAT, the syntax the converter writes and the one in which
// <users/ID> is a mention. The recovery sweep and the owner-notice client are
// not driven here; they reach the same two creates, which the chat package's
// test covers.
func TestEverySendPathStatesChatMarkupSyntax(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	seed := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "**hello** [plan](https://example.com)", IdempotencyKey: "publish"})
	f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "a reply", IdempotencyKey: "reply", ReplyToMessageID: seed.Metadata["thread"]})
	f.send(t, wire.PublishRequest{Conversation: conversation("spaces/dm"), Text: "a direct reply", IdempotencyKey: "dm"})
	if r := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: strings.Repeat("x", 33000), IdempotencyKey: "split"}); r.Metadata["parts"] != "2" {
		t.Fatalf("split: %+v", r)
	}
	if _, b, _ := f.call(t, "/commands/publish", command("command")); !decode[CommandResult](t, b).Delivered {
		t.Fatalf("command: %s", b)
	}
	path := filepath.Join(f.cfg.Endpoints.UploadRoot, "file.txt")
	if err := os.WriteFile(path, []byte("attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	upload := command("upload")
	upload["path"] = path
	if _, b, _ := f.call(t, "/commands/upload", upload); !decode[CommandResult](t, b).Delivered {
		t.Fatalf("upload: %s", b)
	}
	if err := f.s.PostNotice(context.Background(), neversilent.Message{Conversation: "spaces/room", Thread: seed.Metadata["thread"]}, neversilent.DownLine); err != nil {
		t.Fatal(err)
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		if len(g.Messages) != 8 {
			t.Errorf("messages in Chat = %d, want 8", len(g.Messages))
		}
		names := make([]string, 0, len(g.Messages))
		for name := range g.Messages {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if m := g.Messages[name]; m.MarkupSyntax != "MARKUP_SYNTAX_CHAT" {
				t.Errorf("%s (%.20q): markupSyntax = %q, want MARKUP_SYNTAX_CHAT", name, m.Text, m.MarkupSyntax)
			}
		}
	})
}
