package principal

import (
	"context"
	"sync"
)

// WarningLog serializes lookup diagnostics outside the delivery gate. A single
// instance serves the application's generations. While the sink is stalled,
// repeated warnings from new generations need only a count per source and cause.
type WarningLog struct {
	logger  interface{ Warn(string, ...any) }
	mu      sync.Mutex
	pending map[warning]int
	done    chan struct{}
}

type warning struct{ source, cause string }

func NewWarningLog(logger interface{ Warn(string, ...any) }) *WarningLog {
	if logger == nil {
		return nil
	}
	return &WarningLog{logger: logger, pending: make(map[warning]int)}
}

func (w *WarningLog) Report(source, cause string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[warning{source, cause}]++
	if w.done == nil {
		w.done = make(chan struct{})
		go w.write()
	}
}

func (w *WarningLog) write() {
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			close(w.done)
			w.done = nil
			w.mu.Unlock()
			return
		}
		var next warning
		for next = range w.pending {
			break
		}
		w.pending[next]--
		if w.pending[next] == 0 {
			delete(w.pending, next)
		}
		w.mu.Unlock()
		if next.source == "" {
			w.logger.Warn("directory lookup incomplete", "cause", next.cause)
		} else {
			w.logger.Warn("sender lookup incomplete", "source", next.source, "cause", next.cause)
		}
	}
}

// Flush waits for queued warnings up to the caller's deadline. No handler or
// generation shutdown waits for a diagnostic sink.
func (w *WarningLog) Flush(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
