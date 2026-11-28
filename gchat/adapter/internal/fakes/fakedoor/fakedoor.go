// Package fakedoor is a stand-in for the box's root-side door, on a socket
// in a test's own temporary directory. It decides nothing by itself: each
// test says what it answers. UNPROVEN: that the real door, built in the box
// image, answers in the contract's line form; this stand-in encodes it. Files it writes are the test user's, and a
// test's Book trusts them only through the owners package's test stand-in.
package fakedoor

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/owners"
)

// Wait is the test books' bound on one call to the door: short enough that a
// silent door costs a test one second, long enough for a loaded machine.
const Wait = time.Second

// Silent is an answer that never comes: the door reads the line and waits
// until the caller gives up.
const Silent = "\x00silent"

// Door listens on Dir/door.sock.
type Door struct {
	Dir string

	mu       sync.Mutex
	requests []map[string]string
	answer   func(map[string]string) string
	ln       net.Listener
}

// Dir makes an empty directory of the copy's shape, mode 0755, short enough
// for a socket path, without a door.
func Dir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gco")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// New starts a door that answers unavailable until a test says otherwise.
func New(t testing.TB) *Door {
	t.Helper()
	d := &Door{Dir: Dir(t), answer: func(map[string]string) string { return `{"result":"unavailable"}` }}
	ln, err := net.Listen("unix", filepath.Join(d.Dir, "door.sock"))
	if err != nil {
		t.Fatal(err)
	}
	d.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go d.serve()
	return d
}

func (d *Door) serve() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.one(conn)
	}
}

func (d *Door) one(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request map[string]string
	if json.Unmarshal(line, &request) != nil {
		request = map[string]string{"unparsed": string(line)}
	}
	d.mu.Lock()
	d.requests = append(d.requests, request)
	answer := d.answer
	d.mu.Unlock()
	reply := answer(request)
	if reply == Silent {
		_, _ = conn.Read(make([]byte, 1))
		return
	}
	_, _ = conn.Write([]byte(reply + "\n"))
}

// Answer sets what the door answers to each request, as one line without
// its newline, or Silent.
func (d *Door) Answer(f func(request map[string]string) string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = f
}

// Requests are the lines the door received, in order.
func (d *Door) Requests() []map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]map[string]string, len(d.requests))
	copy(out, d.requests)
	return out
}

// WriteCopy publishes a copy of the owners as the door does: a new file,
// mode 0644, renamed into place.
func WriteCopy(t testing.TB, dir string, records map[string]owners.Record) {
	t.Helper()
	if records == nil {
		records = map[string]owners.Record{}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "owners": records})
	if err != nil {
		t.Fatal(err)
	}
	WriteRaw(t, dir, data)
}

// WriteRaw publishes any bytes as the copy, mode 0644, renamed into place.
func WriteRaw(t testing.TB, dir string, data []byte) {
	t.Helper()
	tmp := filepath.Join(dir, ".owners.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "owners.json")); err != nil {
		t.Fatal(err)
	}
}

// Record is an owner record of the copy's shape, made up for a test.
func Record(claim string) owners.Record {
	return owners.Record{Since: "2026-01-02T03:04:05Z", By: "page-code", Claim: claim}
}

// Book is a Book on this door's directory with the test stand-ins and the
// test bound Wait.
func (d *Door) Book(settings []string, log *slog.Logger) *owners.Book {
	return owners.ForTest(owners.TestOptions{Settings: settings, Dir: d.Dir, IsRoot: Root, Log: log, Wait: Wait})
}
