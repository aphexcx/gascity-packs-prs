package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// startupGate lets gc's synchronous service constructor finish its health
// probe and publish the city route that registration needs. Only health is
// admitted until registration and spool recovery have completed.
type startupGate struct {
	attempt atomic.Int64
	ready   atomic.Bool
}

func (s *startupGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ready.Load() {
			next.ServeHTTP(w, r)
			return
		}
		reason := "starting"
		if n := s.attempt.Load(); n > 0 {
			reason = fmt.Sprintf("registering: attempt %d", n)
		}
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-GC-Health", "degraded")
			w.Header().Set("X-GC-Health-Reason", reason)
			// Non-2xx would hold gc's service constructor until its ready
			// timeout, before gc can publish the city API route.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, reason)
			return
		}
		w.Header().Set("Retry-After", "1")
		http.Error(w, reason, http.StatusServiceUnavailable)
	})
}

type registrationHTTPError struct {
	status int
	body   string
}

func (e *registrationHTTPError) Error() string {
	return fmt.Sprintf("register failed: %d %s — %s", e.status, http.StatusText(e.status), headerSafe(e.body, 2048))
}

func registrationNeedsCompatibility(err error) bool {
	var e *registrationHTTPError
	if !errors.As(err, &e) {
		return false
	}
	if e.status == http.StatusBadRequest {
		return strings.Contains(e.body, "reply_instructions")
	}
	// Current Huma validation reports an unknown property as 422. Do not
	// mistake an invalid field value or another missing field for old gc.
	if e.status == http.StatusUnprocessableEntity {
		var problem struct {
			Errors []struct {
				Message  string `json:"message"`
				Location string `json:"location"`
			} `json:"errors"`
		}
		if json.Unmarshal([]byte(e.body), &problem) == nil {
			for _, field := range problem.Errors {
				if field.Location == "body.reply_instructions" && field.Message == "unexpected property" {
					return true
				}
			}
		}
	}
	return false
}

func registrationCanRetry(err error) bool {
	var e *registrationHTTPError
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if !errors.As(err, &e) || e.status != http.StatusNotFound {
		return false
	}
	var problem struct {
		Code string `json:"code"`
	}
	return strings.Contains(e.body, "city not found or not running") ||
		(json.Unmarshal([]byte(e.body), &problem) == nil && problem.Code == "city-not-found")
}

type registrationRetryPolicy struct {
	initialWait time.Duration
	maxWait     time.Duration
	timeout     time.Duration
}

var bootRegistrationPolicy = registrationRetryPolicy{time.Second, 30 * time.Second, 10 * time.Minute}

// registerAdapterOnStart keeps compatibility negotiation separate from boot
// readiness. Once an old gc rejects the template, all subsequent attempts omit
// it. The deadline bounds both HTTP requests and waits, retaining the last
// response when waiting exhausts the budget.
func registerAdapterOnStart(ctx context.Context, cfg config, state *startupGate, policy registrationRetryPolicy) error {
	ctx, cancel := context.WithTimeout(ctx, policy.timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	wait := policy.initialWait
	withoutTemplate := false
	var lastErr error
	for attempt := int64(1); ; attempt++ {
		if err := ctx.Err(); err != nil {
			return registrationStopped(err, lastErr)
		}
		state.attempt.Store(attempt)
		template := registrationReplyInstructions(cfg)
		if withoutTemplate {
			template = ""
		}
		err := registerAdapterRequestContext(ctx, cfg, template)
		if err == nil {
			log.Printf("register adapter: attempt %d succeeded; next wait=0s", attempt)
			return nil
		}
		if ctx.Err() != nil {
			log.Printf("register adapter: attempt %d failed (%v); next wait=0s (stopped)", attempt, err)
			if lastErr == nil {
				lastErr = err
			}
			return registrationStopped(ctx.Err(), lastErr)
		}
		lastErr = err
		if !withoutTemplate && registrationNeedsCompatibility(err) {
			withoutTemplate = true
			log.Printf("register adapter: attempt %d failed (%v); next wait=0s (retrying without reply_instructions)", attempt, err)
			continue
		}
		if !registrationCanRetry(err) {
			log.Printf("register adapter: attempt %d failed (%v); next wait=0s (not retryable)", attempt, err)
			return err
		}
		nextWait := min(wait, time.Until(deadline))
		if nextWait <= 0 {
			log.Printf("register adapter: attempt %d failed (%v); next wait=0s (deadline)", attempt, err)
			return registrationStopped(context.DeadlineExceeded, lastErr)
		}
		log.Printf("register adapter: attempt %d failed (%v); next wait=%s", attempt, err, nextWait)
		timer := time.NewTimer(nextWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return registrationStopped(ctx.Err(), lastErr)
		case <-timer.C:
		}
		wait = min(wait*2, policy.maxWait)
	}
}

func registrationStopped(cause, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("registration stopped: %w; last failure: %v", cause, lastErr)
	}
	return fmt.Errorf("registration stopped: %w", cause)
}
