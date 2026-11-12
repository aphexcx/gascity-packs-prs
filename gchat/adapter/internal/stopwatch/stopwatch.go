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
	log                   func(string)
	warning               sync.Once
}

func New(dir, channel, release string, log func(string)) *Writer {
	w := &Writer{dir: dir, channel: channel, release: release, seen: map[string]bool{}, log: log}
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
				if key := eventKey(e); key != "" {
					w.seen[key] = true
				}
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
	if key != "" {
		w.seen[key] = true
	}
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
