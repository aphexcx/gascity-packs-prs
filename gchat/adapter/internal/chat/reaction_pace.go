package chat

import (
	"context"
	"sort"
	"sync"
	"time"
)

const reactionInterval = time.Second / 5

type reactionJob struct {
	ctx      context.Context
	deadline time.Time
	ready    <-chan struct{}
	result   chan bool
}

// All reactions for an identity share this queue, including explicit agent
// reactions. The queue grants permission to start HTTP; it never waits for a
// slow HTTP response. Deadlines govern choice abandonment, not request drops.
type reactionQueue struct {
	mu      sync.Mutex
	jobs    []*reactionJob
	next    time.Time
	timing  *timing
	changed chan struct{}
	running bool
}

func isReady(ready <-chan struct{}) bool {
	select {
	case <-ready:
		return true
	default:
		return false
	}
}

func latestSlot(jobs []*reactionJob) time.Time {
	var latest time.Time
	for i, job := range jobs {
		slot := job.deadline.Add(-time.Duration(i) * reactionInterval)
		if latest.IsZero() || slot.Before(latest) {
			latest = slot
		}
	}
	return latest
}

// plan is pure with respect to the injected clock. Earliest deadlines go first
// unless a ready choice can use a free slot without costing another message
// its latest safe slot. A full queue starts now and keeps every overflow job.
func (q *reactionQueue) plan(now time.Time) (int, time.Time) {
	free := now
	if q.next.After(free) {
		free = q.next
	}
	latest := latestSlot(q.jobs)
	if isReady(q.jobs[0].ready) || !latest.After(free) {
		return 0, free
	}
	for i := 1; i < len(q.jobs); i++ {
		if !isReady(q.jobs[i].ready) {
			continue
		}
		rest := append([]*reactionJob{}, q.jobs[:i]...)
		rest = append(rest, q.jobs[i+1:]...)
		if !latestSlot(rest).Before(free.Add(reactionInterval)) {
			return i, free
		}
	}
	return 0, latest
}

func (q *reactionQueue) notify() {
	select {
	case q.changed <- struct{}{}:
	default:
	}
}

func (q *reactionQueue) wait(ctx context.Context, deadline time.Time, ready <-chan struct{}) (bool, error) {
	job := &reactionJob{ctx: ctx, deadline: deadline, ready: ready, result: make(chan bool, 1)}
	q.mu.Lock()
	if q.changed == nil {
		q.changed = make(chan struct{}, 1)
	}
	if job.deadline.IsZero() {
		job.deadline = q.timing.Now()
	}
	q.jobs = append(q.jobs, job)
	sort.SliceStable(q.jobs, func(i, j int) bool { return q.jobs[i].deadline.Before(q.jobs[j].deadline) })
	if !q.running {
		q.running = true
		go q.run()
	}
	q.mu.Unlock()
	q.notify()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ready:
			q.notify()
		case <-ctx.Done():
			q.notify()
			return
		case <-done:
			return
		}
		select {
		case <-ctx.Done():
			q.notify()
		case <-done:
		}
	}()
	select {
	case chosen := <-job.result:
		return chosen, nil
	case <-ctx.Done():
		q.notify()
		return false, ctx.Err()
	}
}

func (q *reactionQueue) run() {
	for {
		q.mu.Lock()
		live := q.jobs[:0]
		for _, job := range q.jobs {
			if job.ctx.Err() == nil {
				live = append(live, job)
			}
		}
		q.jobs = live
		if len(q.jobs) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		now := q.timing.Now()
		index, at := q.plan(now)
		if !at.After(now) {
			job := q.jobs[index]
			q.jobs = append(q.jobs[:index], q.jobs[index+1:]...)
			q.next = now.Add(reactionInterval)
			q.mu.Unlock()
			job.result <- isReady(job.ready) && !now.After(job.deadline)
			continue
		}
		q.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		wake := make(chan struct{})
		go func() { _ = q.timing.Sleep(ctx, at.Sub(now)); close(wake) }()
		select {
		case <-wake:
		case <-q.changed:
		}
		cancel()
	}
}

func (l *Limiter) waitReactionChoice(ctx context.Context, deadline time.Time, ready <-chan struct{}) (bool, error) {
	l.mu.Lock()
	if l.reactionChoices == nil {
		l.reactionChoices = &reactionQueue{timing: &l.timing}
	}
	q := l.reactionChoices
	l.mu.Unlock()
	return q.wait(ctx, deadline, ready)
}
