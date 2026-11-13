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

// QueueSize bounds the events waiting for the writer goroutine. Record never
// blocks: on a full queue the event is dropped with the one warning, a lost
// measurement and never a lost message. CloseTimeout bounds the drain at
// shutdown.
const (
	QueueSize    = 1024
	CloseTimeout = 2 * time.Second
)

// Writer decides under its lock (dedup, Slack thread normalization) and
// hands each new line to one writer goroutine, which does the file work and
// its flush. No caller ever waits on the disk.
type Writer struct {
	mu                    sync.Mutex
	dir, channel, release string
	seen                  map[string]bool
	threads               map[string]string
	log                   func(string)
	warning               sync.Once
	queue                 chan Event
	queued                int        // handed to the writer, not yet written
	idle                  *sync.Cond // broadcast as queued falls
	closed                bool
	done                  chan struct{}
	beforeWrite           func() // test seam: runs before each disk write
}

func New(dir, channel, release string, log func(string)) *Writer {
	w := &Writer{dir: dir, channel: channel, release: release, seen: map[string]bool{}, threads: map[string]string{}, log: log, queue: make(chan Event, QueueSize), done: make(chan struct{})}
	w.idle = sync.NewCond(&w.mu)
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
	go w.run()
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

// Record returns at once. The first line per key wins here, before the disk
// sees it, so "first only" holds whatever the writer's pace.
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
	if e.At.IsZero() || w.closed {
		return
	}
	w.remember(e)
	select {
	case w.queue <- e:
		w.queued++
	default:
		w.warn()
	}
}

func (w *Writer) run() {
	defer close(w.done)
	for e := range w.queue {
		w.write(e)
		w.mu.Lock()
		w.queued--
		w.idle.Broadcast()
		w.mu.Unlock()
	}
}

// Flush waits until every line recorded so far is on disk. For tests.
func (w *Writer) Flush() {
	if w == nil {
		return
	}
	w.mu.Lock()
	for w.queued > 0 {
		w.idle.Wait()
	}
	w.mu.Unlock()
}

// Close stops intake and drains the queue for at most CloseTimeout. It is
// safe to call more than once.
func (w *Writer) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(CloseTimeout):
	}
}

func (w *Writer) write(e Event) {
	if w.beforeWrite != nil {
		w.beforeWrite()
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

// ReleaseOf names the running release. When the executable cannot be found,
// opened or read, the release is "unknown", log hears one line, and the
// adapter starts: the stopwatch never breaks the adapter.
func ReleaseOf(executable func() (string, error), log func(string)) string {
	if path, err := executable(); err == nil {
		if release, err := Release(path); err == nil {
			return release
		}
	}
	if log != nil {
		log("stopwatch: release unavailable; recorded as unknown")
	}
	return "unknown"
}
