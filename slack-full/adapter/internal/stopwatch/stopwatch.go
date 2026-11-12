// Package stopwatch records times and message coordinates, never content.
package stopwatch

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	Event        string    `json:"event"`
	Channel      string    `json:"channel"`
	Conversation string    `json:"conversation"`
	Message      string    `json:"message,omitempty"`
	Thread       string    `json:"thread,omitempty"`
	MessageAt    time.Time `json:"message_at,omitzero"`
	At           time.Time `json:"at"`
	Release      string    `json:"release,omitempty"`
	Actor        string    `json:"actor,omitempty"`
	Choice       string    `json:"choice,omitempty"`
}
type Writer struct {
	mu                    sync.Mutex
	dir, channel, release string
	seen                  map[string]bool
	threads               map[string]string
	log                   func(string)
	warning               sync.Once
}

func New(dir, channel, release string, log func(string)) *Writer {
	w := &Writer{dir: dir, channel: channel, release: release, seen: map[string]bool{}, threads: map[string]string{}, log: log}
	files, err := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
	if err != nil {
		w.warn()
	}
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			w.warn()
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) == nil {
				w.remember(e)
			}
		}
		if scanner.Err() != nil {
			w.warn()
		}
		f.Close()
	}
	return w
}
func eventKey(e Event) string {
	if (e.Event == "inbound" || e.Event == "reaction" || e.Event == "words") && e.Message != "" {
		return e.Event + "\x00" + e.Conversation + "\x00" + e.Message
	}
	return ""
}

func (w *Writer) remember(e Event) {
	if key := eventKey(e); key != "" {
		w.seen[key] = true
	}
	if w.channel == "slack" && e.Message != "" && (e.Event == "inbound" || e.Event == "words") {
		root := e.Thread
		if root == "" {
			root = e.Message
		}
		w.threads[e.Conversation+"\x00"+e.Message] = root
	}
}
func (w *Writer) warn() {
	w.warning.Do(func() {
		if w.log != nil {
			w.log("stopwatch: state file unavailable; adapter continues")
		}
	})
}
func (w *Writer) Record(e Event) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e.Channel = w.channel
	if e.Event == "inbound" {
		e.Release = w.release
	}
	if e.Event == "words" && w.channel == "slack" {
		// A publish may address a child post instead of its canonical root.
		// Reuse channel coordinates already recorded, including after restart.
		visited := map[string]bool{}
		for e.Thread != "" && !visited[e.Thread] {
			visited[e.Thread] = true
			root := w.threads[e.Conversation+"\x00"+e.Thread]
			if root == "" || root == e.Thread {
				break
			}
			e.Thread = root
		}
	}
	key := eventKey(e)
	if key != "" && w.seen[key] {
		return
	}
	if e.At.IsZero() {
		return
	}
	if err := os.MkdirAll(w.dir, 0700); err != nil {
		w.warn()
		return
	}
	f, err := os.OpenFile(filepath.Join(w.dir, "stopwatch-"+e.At.UTC().Format("2006-01")+".jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		w.warn()
		return
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		w.warn()
		return
	}
	// Isolate a torn last append; the report skips the fragment independently.
	stat, err := f.Stat()
	if err != nil {
		w.warn()
		return
	}
	if stat.Size() > 0 {
		last := []byte{0}
		if _, err := f.ReadAt(last, stat.Size()-1); err != nil {
			w.warn()
			return
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				w.warn()
				return
			}
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		w.warn()
		return
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		w.warn()
		return
	}
	if err = f.Sync(); err != nil {
		w.warn()
		return
	}
	w.remember(e)
}
func Release(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
