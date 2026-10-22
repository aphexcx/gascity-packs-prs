package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/token"

	"cloud.google.com/go/pubsub/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	workspace "google.golang.org/api/workspaceevents/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func (a *application) generation(ctx context.Context, source *token.Source, reloaded bool, registered *atomic.Bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := gcclient.New(a.cfg.Secrets.GCAPIBase, a.cfg.Secrets.CityName)
	a.s.SetClients(nil, g)
	g.SetConnectionRefusedHandler(func(err error) { a.gcDown.Store(true); a.s.ObserveGCError(err) })
	if err := a.s.Register(ctx); err != nil {
		return err
	}
	registered.Store(true)
	a.gcDown.Store(false)
	hc, err := source.Client(ctx)
	if err != nil {
		return err
	}
	c, err := chat.New(ctx, hc, a.cfg.Endpoints)
	if err != nil {
		return err
	}
	if err = a.seedIdentity(source.IdentityUserID()); err != nil {
		return err
	}
	a.mu.Lock()
	id := a.l.IdentityUserID
	a.mu.Unlock()
	if id != "" {
		if err = c.SetIdentityUserID(id); err != nil {
			return err
		}
	}
	a.chatClient.Store(c)
	a.s.SetClients(c, g)
	g.SetConnectionRefusedHandler(func(err error) { a.gcDown.Store(true); a.s.ObserveGCError(err) })
	es, err := workspace.NewService(ctx, option.WithHTTPClient(hc), option.WithEndpoint(strings.TrimRight(a.cfg.Endpoints.EventsAPIBase, "/")+"/"))
	if err != nil {
		return err
	}
	m, err := events.New(events.Options{Service: es, Topic: a.cfg.Secrets.Topic, Ledger: a.l, LedgerPath: a.cfg.Endpoints.StateFile, LedgerMu: &a.mu, Log: a.log})
	if err != nil {
		return err
	}
	a.manager.Store(m)
	proc, err := inbound.New(inbound.Options{Config: a.cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: a.cfg.Endpoints.StateFile, GC: g, Chat: c, Logger: a.log, RetryDelay: a.opts.RetryDelay})
	if err != nil {
		return err
	}
	r, err := catchup.New(catchup.Options{Config: a.cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: a.cfg.Endpoints.StateFile, Chat: c, Processor: proc, Logger: a.log})
	if err != nil {
		return err
	}
	a.s.SetPoll(proc.Poll)
	defer a.s.SetPoll(nil)
	if err = a.retry(ctx, "subscription", func() error {
		var e error
		if reloaded {
			_, e = m.TokenReloaded(ctx)
		} else {
			_, e = m.Reconcile(ctx)
		}
		return e
	}); err != nil {
		return err
	}
	// A create notification is covered by this mandatory startup Freeze. Future
	// notifications must trigger a new Freeze, even during unfinished recovery.
	select {
	case <-m.RecoveryNeeded():
	default:
	}
	if err = a.retry(ctx, "freeze", func() error { return r.Freeze(ctx, time.Now()) }); err != nil {
		return err
	}
	pc, err := newPubSub(ctx, a.cfg, source)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait(); _ = pc.Close() }()
	fatal := make(chan error, 3)
	var caught atomic.Bool
	streamStarted := make(chan struct{})
	workers.Go(func() { close(streamStarted); fatal <- a.pull(ctx, pc, m, proc, c) })
	<-streamStarted
	a.health("catching-up", "recovering retained history")
	workers.Go(func() { m.RenewLoop(ctx) })
	workers.Go(func() { a.poll(ctx, proc, fatal) })
	workers.Go(func() { fatal <- a.catchup(ctx, r, m, &caught) })
	monitor := time.NewTicker(time.Second)
	defer monitor.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-fatal:
			return err
		case <-monitor.C:
			if err = a.persistIdentity(c); err != nil {
				return err
			}
			last, failures, _, state := m.Status()
			if credentialsError(last) || source.State() == token.NeedsSignIn {
				return token.ErrNeedsSignIn
			}
			if failures > 0 {
				a.health("catching-up", safeReason("subscription reconciliation pending", last))
				var ge *googleapi.Error
				if errors.As(last, &ge) && ge.Code >= 400 && ge.Code < 500 {
					a.notice("subscription")
				}
			} else if a.gcDown.Load() {
				a.health("catching-up", "gc unavailable; deliveries retained for retry")
			} else if state.State != "ACTIVE" {
				a.health("catching-up", "subscription is not active")
			} else if c.IdentityUserID() == "" && a.lastEvent.Load() != 0 {
				a.health("catching-up", "Chat identity not yet known; run gc gchat auth")
			} else if caught.Load() {
				a.health("ready", "")
			}
		}
	}
}

func newPubSub(ctx context.Context, cfg *config.Config, source *token.Source) (*pubsub.Client, error) {
	opts := []option.ClientOption{option.WithTokenSource(source.TokenSource(ctx))}
	if cfg.Endpoints.PubSubEndpoint != "" {
		opts = []option.ClientOption{option.WithEndpoint(cfg.Endpoints.PubSubEndpoint), option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	}
	return pubsub.NewClient(ctx, cfg.Secrets.ProjectID, opts...)
}

func (a *application) pull(ctx context.Context, pc *pubsub.Client, m *events.Manager, p *inbound.Processor, c *chat.Client) error {
	consecutiveCredentials := 0
	handler := func(ctx context.Context, ev *puller.Event) (puller.Outcome, error) {
		a.lastEvent.Store(time.Now().UnixNano())
		if strings.HasPrefix(ev.Type, "google.workspace.events.subscription.") {
			if err := m.HandleLifecycle(ctx, ev.Type); err != nil {
				a.log.Warn("lifecycle reconciliation pending", "cause", safeReason("retry scheduled", err))
			}
			return puller.Ack, nil // RenewLoop owns retry/backoff, never redelivery.
		}
		outcome, err := p.Handle(ctx, ev)
		if credentialsError(err) {
			consecutiveCredentials++
			if !strings.Contains(err.Error(), "reaction parent") || consecutiveCredentials >= 2 {
				return puller.Nack, err
			}
		} else {
			consecutiveCredentials = 0
		}
		if err != nil && strings.Contains(err.Error(), "gc temporarily unavailable") {
			a.gcDown.Store(true)
		}
		if saveErr := a.persistIdentity(c); saveErr != nil {
			if errors.Is(saveErr, errIdentityChanged) {
				return puller.Nack, saveErr
			}
			return puller.Nack, errors.New("app: identity persistence failed")
		}
		if outcome == puller.Nack {
			_ = sleep(ctx, time.Second)
		}
		return outcome, err
	}
	// Handle credential errors out of band: returning Nack alone would keep
	// Receive running indefinitely with a revoked token and permit new acks.
	pullCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var held error
	wrapped := func(ctx context.Context, ev *puller.Event) (puller.Outcome, error) {
		out, e := handler(ctx, ev)
		if errors.Is(e, errIdentityChanged) || (credentialsError(e) && (!strings.Contains(e.Error(), "reaction parent") || consecutiveCredentials >= 2)) {
			held = e
			cancel()
		}
		return out, e
	}
	err := puller.Run(pullCtx, pc.Subscriber(a.cfg.Secrets.Subscription), wrapped, a.log, puller.RunOptions{MediaDir: a.cfg.Endpoints.MediaDir, RecordQuarantine: a.quarantine})
	if held != nil {
		return held
	}
	if status.Code(err) == codes.Unauthenticated {
		return token.ErrNeedsSignIn
	}
	if status.Code(err) == codes.NotFound {
		a.health("catching-up", "Pub/Sub subscription not found; reconciliation required")
		_, _ = m.Reconcile(ctx)
	}
	// Including Canceled with a live parent: the generation closes and rebuilds
	// this client, then startup Freeze recovers any stream gap before reconnect.
	return err
}

func (a *application) catchup(ctx context.Context, r *catchup.Runner, m *events.Manager, caught *atomic.Bool) error {
	for {
		_, remaining, err := r.Pass(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if credentialsError(err) {
			return err
		}
		caught.Store(err == nil && remaining == 0)
		if err != nil {
			a.log.Warn("catch-up pass deferred", "cause", safeReason("retrying", err))
		}
		if !caught.Load() {
			a.health("catching-up", "recovering retained history")
		}
		delay := time.Second
		if caught.Load() {
			delay = time.Minute
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-m.RecoveryNeeded():
			t.Stop()
			caught.Store(false)
			a.health("catching-up", "subscription recovered; recovering history")
			if err = a.retry(ctx, "freeze", func() error { return r.Freeze(ctx, time.Now()) }); err != nil {
				return err
			}
		case <-t.C:
			// Periodic inventory refresh also retries left/rejoined spaces.
			if caught.Load() {
				if err = a.retry(ctx, "freeze", func() error { return r.Freeze(ctx, time.Now()) }); err != nil {
					return err
				}
			}
		}
	}
}

func (a *application) poll(ctx context.Context, p *inbound.Processor, fatal chan<- error) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var lastLog time.Time
	for {
		err := p.Poll(ctx)
		if err != nil && strings.Contains(err.Error(), "gc temporarily unavailable") {
			a.gcDown.Store(true)
		}
		if credentialsError(err) {
			select {
			case fatal <- err:
			case <-ctx.Done():
			}
			return
		}
		if err != nil && ctx.Err() == nil && time.Since(lastLog) >= time.Minute {
			a.log.Warn("inbound recovery deferred", "operation", "poll")
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *application) retry(ctx context.Context, op string, fn func() error) error {
	for ceiling := time.Second; ; ceiling = min(2*ceiling, time.Minute) {
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if credentialsError(err) {
			return err
		}
		a.log.Warn("initialization deferred", "operation", op, "cause", safeReason("retrying", err))
		var ge *googleapi.Error
		if op == "subscription" && errors.As(err, &ge) && ge.Code >= 400 && ge.Code < 500 {
			a.notice("subscription")
		}
		if err = sleep(ctx, ceiling/2+time.Duration(rand.Int64N(int64(ceiling/2)))); err != nil {
			return err
		}
	}
}

func credentialsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, token.ErrNeedsSignIn) || errors.Is(err, inbound.ErrCredentials) {
		return true
	}
	kind, _ := chat.Classify(err)
	return kind == "auth"
}

func safeReason(reason string, err error) string {
	if err == nil {
		return reason
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return fmt.Sprintf("%s (HTTP %d)", reason, ge.Code)
	}
	if code := status.Code(err); code != codes.Unknown {
		return reason + " (" + code.String() + ")"
	}
	return reason
}
