package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// healthDegradedReasons collects the advisory degraded conditions for
// the X-GC-Health header: a confirmed inbound-liveness stall (the
// watchdog found human messages in channel history the adapter never
// received) and a socket transport down past its grace window (provable
// transport death, independent of channel activity). Components not
// wired by production main() contribute nothing.
func healthDegradedReasons(now time.Time) []string {
	var reasons []string
	if r := livenessHealth.Load().degradedReason(); r != "" {
		reasons = append(reasons, r)
	}
	if r := socketModeHealth.Load().degradedReason(now); r != "" {
		reasons = append(reasons, r)
	}
	return reasons
}

// headerSafe makes s a valid, bounded HTTP header value: the reason can
// embed error text from remote peers (websocket close frames), and a
// control character there would corrupt the very health response the
// keep-routing contract depends on.
func headerSafe(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// dispatchDroppedTotal counts inbound Slack deliveries dropped because
// the dispatch semaphore was saturated (acquireDispatchSlot returned
// not-ok). Package-level like dispatchInflightWG: saturation is a
// process-wide signal no matter which cfg value observed it. Exposed
// on /healthz and rolled up by runDispatchDropSummary so operators
// can see loss without scraping per-event "dispatch queue full" lines.
var dispatchDroppedTotal atomic.Uint64

// handleHealthz reports liveness plus the cumulative dropped-dispatch
// count. The first line stays exactly "ok" so status-code and
// grep-based checks keep working; the counter rides on a second line.
// When a company gateway is wired (production main() only), its barrier
// state, receipt-store write-failure counter, and snapshot state ride
// on trailing lines — the sole status surface for the company path
// (there is no separate gateway status payload; see company_delivery.go).
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// Advisory degraded headers (gp-rol): still 200 — gc must keep
	// routing /publish (outbound survives a dead inbound; the 8/19
	// incident's one working path) — but gc's proxy_process supervisor
	// reads X-GC-Health off the 2xx response and flips `gc service list`
	// to state=degraded with the reason. Older gc binaries ignore it.
	if reasons := healthDegradedReasons(time.Now()); len(reasons) > 0 {
		w.Header().Set("X-GC-Health", "degraded")
		w.Header().Set("X-GC-Health-Reason", headerSafe(strings.Join(reasons, "; "), 512))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "ok\ndispatch_dropped_total=%d\n", dispatchDroppedTotal.Load())
	if gw := companyHealthStatus.Load(); gw != nil {
		_, _ = io.WriteString(w, gw.healthzDetail())
	}
	// Inbound transport + liveness (gp-3og). Like the company gateway,
	// each line appears only when production main() wired the component
	// — the two-line "ok + counter" contract holds for bare handlers.
	if r := socketModeHealth.Load(); r != nil {
		_, _ = io.WriteString(w, r.healthzDetail())
	}
	if l := livenessHealth.Load(); l != nil {
		_, _ = io.WriteString(w, l.healthzDetail())
	}
}

// dispatchDropSummaryInterval paces the saturation roll-up log. One
// minute keeps the signal near-real-time; ticks with no new drops log
// nothing, so a healthy adapter stays silent.
const dispatchDropSummaryInterval = time.Minute

// runDispatchDropSummary logs a periodic roll-up of dropped
// dispatches so sustained saturation shows up as a low-noise
// heartbeat even when per-event drop lines are flooding. Only ticks
// where new drops occurred produce a line.
func runDispatchDropSummary(ctx context.Context, interval time.Duration, capacity int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastReported uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			total := dispatchDroppedTotal.Load()
			if delta := total - lastReported; delta > 0 {
				log.Printf("slack adapter: dispatch saturation summary: dropped=%d in last %s (total=%d cap=%d)",
					delta, interval, total, capacity)
				lastReported = total
			}
		}
	}
}
