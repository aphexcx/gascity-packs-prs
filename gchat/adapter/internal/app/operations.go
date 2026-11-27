package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
)

// Seed once per credential identity. Observed Chat identity wins subsequent
// client rebuilds, so an OAuth/Chat mismatch cannot create a rebuild loop.
func (a *application) seedIdentity(id string) error {
	if id == "" {
		a.legacyWarning.Do(func() { a.log.Warn("legacy token has no identity; run gc gchat auth", "operation", "identity") })
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == a.l.IdentitySeed && a.l.IdentityUserID != "" {
		return nil
	}
	previous := a.l.IdentityUserID
	previousSeed := a.l.IdentitySeed
	previousDomain := a.l.IdentityDomainID
	a.l.IdentityUserID, a.l.IdentitySeed = id, id
	if previous != id {
		a.l.IdentityDomainID = ""
	}
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.IdentityUserID, a.l.IdentitySeed = previous, previousSeed
		a.l.IdentityDomainID = previousDomain
		return err
	}
	// Shutdown may checkpoint before the generation installs its new client.
	// Once a new consent is durable, the previous client cannot supply identity.
	a.chatClient.Store(nil)
	if previous != "" && previous != id {
		if previousSeed == "" {
			a.log.Warn("legacy ledger identity re-seeded from credential; observed identity will be verified again", "operation", "identity")
		} else {
			a.log.Warn("credential identity changed; rebuilding client", "operation", "identity")
		}
	}
	return nil
}

func (a *application) persistIdentity(c *chat.Client) error {
	id, conflicted := c.ObservedIdentityUserID()
	var result error
	if conflicted {
		result = errIdentityChanged
	}
	if id == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.l.IdentityUserID == id {
		return result
	}
	previous := a.l.IdentityUserID
	previousDomain := a.l.IdentityDomainID
	a.l.IdentityUserID = id
	a.l.IdentityDomainID = ""
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.IdentityUserID = previous
		a.l.IdentityDomainID = previousDomain
		return err
	}
	return result
}

func (a *application) quarantine(ctx context.Context, ev *puller.Event, path string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range a.l.Quarantine {
		if q.PubSubID == ev.PubSubID {
			return nil
		}
	}
	previous := a.l.Quarantine
	a.l.Quarantine = append(append([]ledger.QuarantinedEvent(nil), previous...), ledger.QuarantinedEvent{PubSubID: ev.PubSubID, Reason: cause.Error(), Path: path, At: time.Now()})
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.Quarantine = previous
		return err
	}
	return nil
}

func (a *application) retryPending(ctx context.Context, registered *atomic.Bool) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !registered.Load() {
				continue
			}
			if a.gcDown.Load() {
				if err := a.ensureRegistered(ctx, registered); err != nil {
					return
				}
				a.gcDown.Store(false)
			}
			state := a.s.Health().State
			// An interrupted send can supply the identity that discovery is
			// waiting for. Other recovery still waits for ordinary readiness.
			if state != "ready" && !(state == "catching-up" && a.identityDiscoveryHeld.Load()) {
				continue
			}
			if err := a.s.RetryPending(ctx); err != nil && ctx.Err() == nil {
				count := 1
				if joined, ok := err.(interface{ Unwrap() []error }); ok {
					count = len(joined.Unwrap())
				}
				a.log.Warn("outbound recovery deferred", "operation", "retry_pending", "failed_keys", count)
			}
		}
	}
}

type notification struct{ kind, message, agent, dedup string }

func (a *application) noticeHistoryRetired(references []string) {
	if len(references) == 0 {
		return
	}
	slices.Sort(references)
	message := fmt.Sprintf("The first load ended %d waiting deliveries as history:\n%s", len(references), strings.Join(references, "\n"))
	// Separate starts may retire the same references after restoring old state.
	a.notice("history-delivery-retired", message, rand.Text())
}

func (a *application) notice(kind string, details ...string) {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	key := kind
	if len(details) > 0 {
		key += fmt.Sprintf(":%x", sha256.Sum256([]byte(details[0])))
	}
	if kind == "history-delivery-retired" && len(details) > 1 {
		key += ":" + details[1]
	}
	if a.notified[key] {
		return
	}
	message := "Subscription reconciliation failed; inspect gc gchat status and the README Google-side setup checklist (enabled APIs and topic Publisher IAM)."
	if kind == "credentials" {
		message = "Google Chat credentials are missing or revoked; run gc gchat auth. Events remain at Google."
	} else if kind == "chat-access" {
		message = "Google Chat refuses the Workspace user's token (HTTP 401/403) although the key holder mints it. Check that the Chat API is configured for the project and Chat is on for the user, or send the user a first direct message; the adapter keeps retrying. Events remain at Google."
	} else if kind == "history-delivery-retired" && len(details) > 0 {
		message = details[0]
	} else if len(details) > 0 {
		message = details[0] + ". " + message
	}
	select {
	case a.notices <- notification{kind: kind, message: message, agent: a.cfg.Identity().Agent, dedup: "gchat:" + a.cfg.Secrets.CityName + ":" + a.cfg.Identity().Name + ":" + key}:
		a.notified[key] = true
	default:
	}
}
func (a *application) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case notice := <-a.notices:
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var err error
			if a.opts.Notify != nil {
				err = a.opts.Notify(nctx, "gchat: "+notice.kind, notice.message)
			} else {
				bin := os.Getenv("GC_BIN")
				if bin == "" {
					bin = "gc"
				}
				err = exec.CommandContext(nctx, bin, "mail", "send", notice.agent, "--dedup", notice.dedup, "-s", "gchat: "+notice.kind, "-m", notice.message).Run()
			}
			cancel()
			if err != nil {
				a.log.Warn("operator notification failed", "operation", "notify")
			}
		}
	}
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// raiseDelivery mails the identity's agent about one stuck or unresolved
// inbound delivery, the way the notifier sends, but synchronously: the
// processor records the raise only when this returns nil. The dedup key is
// inbound.Raise.DedupKey under the city and identity: one per life of the
// change and kind of mail, so a send whose result was lost is one mail at gc,
// a mail that says the message ended has its own key, and a replay's mails
// are never dropped for a key of the life before it.
func (a *application) raiseDelivery(ctx context.Context, r inbound.Raise) error {
	const mailTimeout = 120 * time.Second
	const wakeTimeout = 60 * time.Second
	mailLimit, wakeLimit := a.opts.mailTimeout, a.opts.wakeTimeout
	if mailLimit == 0 {
		mailLimit = mailTimeout
	}
	if wakeLimit == 0 {
		wakeLimit = wakeTimeout
	}
	mailCtx, cancelMail := context.WithTimeout(ctx, mailLimit)
	defer cancelMail()
	const subject = "gchat: delivery"
	if a.opts.Notify != nil {
		return a.opts.Notify(mailCtx, subject, r.Body)
	}
	identity := a.cfg.Identity()
	key := fmt.Sprintf("gchat:%s:%s:%s", a.cfg.Secrets.CityName, identity.Name, r.DedupKey())
	bin := os.Getenv("GC_BIN")
	if bin == "" {
		bin = "gc"
	}
	cmd := exec.CommandContext(mailCtx, bin, "mail", "send", identity.Agent, "--dedup", key, "--notify", "--json", "-s", subject, "-m", r.Body)
	// A descendant can outlive gc with an inherited output descriptor.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	// ErrWaitDelay by itself means gc exited successfully. Its result line
	// still has to confirm the mail, exactly as on an ordinary successful exit.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if mailCtx.Err() == context.DeadlineExceeded {
			return inbound.RaiseFailure("mail_timeout")
		}
		return inbound.RaiseFailure("mail_exit")
	}
	cancelMail()
	for line := range bytes.Lines(out) {
		var result struct {
			OK       bool `json:"ok"`
			Notified bool `json:"notified"`
			Data     struct {
				Notified bool `json:"notified"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &result) != nil || !result.OK {
			continue
		}
		if result.Notified || result.Data.Notified {
			return nil
		}
		wakeCtx, cancelWake := context.WithTimeout(ctx, wakeLimit)
		defer cancelWake()
		if err := exec.CommandContext(wakeCtx, bin, "session", "nudge", identity.Agent, "gchat: a delivery notice is waiting in your mail").Run(); err != nil {
			if wakeCtx.Err() == context.DeadlineExceeded {
				return inbound.RaiseFailure("wake_timeout")
			}
			return inbound.RaiseFailure("wake_exit")
		}
		return nil
	}
	return inbound.RaiseFailure("mail_no_result")
}
