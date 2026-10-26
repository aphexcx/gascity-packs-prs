// Package app owns the service lifecycle. The socket is served before any disk
// recovery or remote I/O; one cancellable generation owns all inbound workers.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/token"
)

type Options struct {
	EnvFile, SettingsPath string
	Logger                *slog.Logger
	// Notify sends a single operator notification per incident. Tests replace it.
	Notify func(context.Context, string, string) error
	// RetryDelay is for inbound's bounded delivery retries; zero keeps its 1s default.
	RetryDelay time.Duration
}

var errIdentityChanged = errors.New("app: observed identity changed; rebuilding client")

type application struct {
	cfg                *config.Config
	opts               Options
	log                *slog.Logger
	l                  *ledger.Ledger
	mu                 sync.Mutex
	stateMu            sync.Mutex // excludes app health transitions during POST /state
	s                  *server.Server
	manager            atomic.Pointer[events.Manager]
	chatClient         atomic.Pointer[chat.Client]
	loaded             atomic.Bool
	lastEvent          atomic.Int64
	gcDown             atomic.Bool
	registrationMu     sync.Mutex
	registrationNeeded atomic.Bool
	healthyGeneration  atomic.Bool
	restartDelay       time.Duration // owned by the run loop
	resume             chan struct{}
	notices            chan notification
	noticeMu           sync.Mutex
	notified           map[string]bool
	legacyWarning      sync.Once
	stateLock          *StateLock
	lockWarning        sync.Once
}

func Run(ctx context.Context, cfg *config.Config) error { return RunWithOptions(ctx, cfg, Options{}) }

func RunWithOptions(ctx context.Context, cfg *config.Config, opts Options) error {
	if cfg == nil {
		return errors.New("app: config is required")
	}
	copy := *cfg
	cfg = &copy
	if cfg.Secrets.ServiceSocket == "" {
		cfg.Secrets.ServiceSocket = filepath.Join(filepath.Dir(cfg.Endpoints.StateFile), "adapter.sock")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	lock, err := LockState(cfg.Endpoints.StateFile)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lock.RecordSocket(cfg.Secrets.ServiceSocket); err != nil {
		return err
	}
	a := &application{cfg: cfg, opts: opts, log: opts.Logger, l: ledger.New(), resume: make(chan struct{}, 1), notices: make(chan notification, 8), notified: map[string]bool{}}
	a.stateLock = lock
	a.l.SetSaveGuard(a.checkStateLock)
	s, err := server.New(server.Options{Config: cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: cfg.Endpoints.StateFile, SettingsPath: opts.SettingsPath, Logger: a.log, LastEvent: func() time.Time {
		if n := a.lastEvent.Load(); n != 0 {
			return time.Unix(0, n)
		}
		return time.Time{}
	}, SubscriptionStatus: func() (int, time.Time, ledger.SubscriptionState, error) {
		if m := a.manager.Load(); m != nil {
			err, failures, next, state := m.Status()
			return failures, next, state, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return 0, time.Time{}, a.l.Subscription, nil
	}, StateChanged: func(context.Context) error { signal(a.resume); return nil }})
	if err != nil {
		return err
	}
	a.s = s
	if err = prepareSocket(cfg.Secrets.ServiceSocket); err != nil {
		s.Close()
		return err
	}
	ln, err := s.Listen()
	if err != nil {
		s.Close()
		return err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.Close()
	h := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/state" {
			a.stateMu.Lock()
			defer a.stateMu.Unlock()
			if err := a.checkStateLock(); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.ServeHTTP(w, r)
	}), BaseContext: func(net.Listener) context.Context { return serviceCtx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 25 * time.Second, IdleTimeout: time.Minute}
	httpDone := make(chan error, 1)
	go func() { httpDone <- h.Serve(ln) }()
	workerDone := make(chan error, 1)
	go func() { workerDone <- a.run(serviceCtx) }()
	var result error
	workerStopped := false
	select {
	case result = <-workerDone:
		workerStopped = true
	case err = <-httpDone:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	case <-ctx.Done():
	}
	cancel()
	// gc allows only two seconds before SIGKILL. Stop intake and checkpoint
	// before waiting on any request or worker; request contexts share cancellation.
	s.Close()
	result = errors.Join(result, a.saveShutdownState(cfg.Endpoints.StateFile))
	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := h.Shutdown(shutdownCtx); err != nil {
		_ = h.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			result = errors.Join(result, err)
		}
	}
	_ = ln.Close()
	if !workerStopped {
		select {
		case err := <-workerDone:
			workerStopped = true
			result = errors.Join(result, err)
		case <-shutdownCtx.Done():
		}
	}
	// Capture any completion during the drain without depending on it for the
	// first durable checkpoint. The original path is immutable across reloads.
	if workerStopped && shutdownCtx.Err() == nil {
		result = errors.Join(result, a.saveShutdownState(cfg.Endpoints.StateFile))
	}
	return result
}

func (a *application) saveShutdownState(path string) error {
	if !a.loaded.Load() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.chatClient.Load(); c != nil {
		if id, _ := c.ObservedIdentityUserID(); id != "" {
			a.l.IdentityUserID = id
		}
	}
	return a.l.Save(path)
}

func prepareSocket(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("app: socket path %s is not a socket", path)
	}
	c, err := net.DialTimeout("unix", path, 50*time.Millisecond)
	if err == nil {
		c.Close()
		return fmt.Errorf("app: service already holds socket %s", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("app: cannot verify stale socket %s: %w", path, err)
	}
	return RemoveSocket(path, info)
}

func (a *application) health(state, reason string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.checkStateLock() != nil {
		return
	}
	if state == "ready" {
		a.healthyGeneration.Store(true)
	}
	if a.s.Health() == (server.Health{State: state, Reason: reason}) {
		return
	}
	a.s.SetHealth(server.Health{State: state, Reason: reason})
	if a.log != nil {
		a.log.Debug("adapter health changed", "state", state, "reason", reason)
	}
}

// Save callers may already hold the ledger or state mutex. Only the server's
// independent health mutex is used here, keeping the ownership check reentrant.
func (a *application) checkStateLock() error {
	if a.stateLock == nil {
		return nil
	}
	err := a.stateLock.Check()
	if err != nil {
		a.lockWarning.Do(func() {
			if a.log != nil {
				a.log.Error("state lock ownership lost", "operation", "state_lock")
			}
		})
		if a.s != nil {
			a.s.SetHealth(server.Health{State: "recovery-required", Reason: err.Error()})
		}
	}
	return err
}

func (a *application) run(ctx context.Context) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	l, err := ledger.Load(a.cfg.Endpoints.StateFile, ledger.Established(a.cfg.SecretsDir))
	recovery := errors.Is(err, ledger.ErrRecoveryRequired)
	if err != nil && !recovery {
		return err
	}
	if recovery {
		a.health("recovery-required", "ledger is missing or invalid; restore a backup or explicitly reset state")
	} else {
		a.mu.Lock()
		*a.l = *l
		a.l.SetSaveGuard(a.checkStateLock)
		err = a.l.Save(a.cfg.Endpoints.StateFile)
		a.mu.Unlock()
		if err != nil {
			return err
		}
		if err = ledger.MarkEstablished(a.cfg.SecretsDir); err != nil {
			return err
		}
		a.loaded.Store(true)
	}
	if err = canonicalize(a.cfg); err != nil {
		return err
	}
	a.s.SetConfig(*a.cfg)
	source, err := token.New(a.cfg.Secrets.OAuthClientFile, a.cfg.Identity().TokenFile, a.cfg.Endpoints, token.Scopes)
	if err != nil {
		return err
	}
	reload := make(chan struct{}, 1)
	stopWatch := watchToken(ctx, source, reload)
	defer func() { stopWatch() }()
	var auxiliaries sync.WaitGroup
	auxiliaries.Go(func() { a.notifyLoop(ctx) })
	// Sweeps belong to the service, not a credential generation. A reload does
	// not cancel a recording operation halfway through its gc callback.
	registered := &atomic.Bool{}
	g := gcclient.New(a.cfg.Secrets.GCAPIBase, a.cfg.Secrets.CityName)
	a.s.SetClients(nil, g)
	auxiliaries.Go(func() {
		_ = a.ensureRegistered(ctx, registered)
	})
	auxiliaries.Go(func() { a.retryPending(ctx, registered) })
	var generationCancel context.CancelFunc
	var generationDone chan error
	stopGeneration := func() {
		if generationCancel != nil {
			generationCancel()
			<-generationDone
			generationCancel = nil
			generationDone = nil
		}
	}
	defer func() {
		cancelRun()
		stopGeneration()
		auxiliaries.Wait()
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var retry <-chan time.Time
	var retryTimer *time.Timer
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()
	credentialHold := false
	var lastCredentialFailure time.Time
	reloaded := false
	settingsBytes, _ := os.ReadFile(a.opts.SettingsPath)
	start := func() {
		if a.checkStateLock() != nil || recovery || credentialHold || generationDone != nil {
			return
		}
		if source.State() != token.Ready {
			a.health("needs-sign-in", "credential missing or revoked; run gc gchat auth")
			a.notice("credentials")
			return
		}
		a.health("initializing", "connecting and reconciling subscriptions")
		if err := a.s.Drain(ctx); err != nil {
			return
		}
		if c := a.chatClient.Load(); c != nil {
			if err := a.persistIdentity(c); err != nil && !errors.Is(err, errIdentityChanged) {
				a.log.Warn("identity persistence failed", "operation", "identity")
				a.health("initializing", "identity persistence failed; retrying")
				retryTimer = time.NewTimer(time.Second)
				retry = retryTimer.C
				return
			}
		}
		gctx, cancel := context.WithCancel(ctx)
		generationCancel = cancel
		generationDone = make(chan error, 1)
		done := generationDone
		wasReloaded := reloaded
		reloaded = false
		go func() { done <- a.generation(gctx, source, wasReloaded, registered) }()
	}
	start()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.resume:
			if a.checkStateLock() != nil {
				continue
			}
			// /state is only admitted while recovery-required, when no generation
			// owns the ledger. It has already replaced and saved the shared value.
			recovery = false
			a.loaded.Store(true)
			credentialHold = false
			start()
		case <-reload:
			if !recovery {
				a.health("initializing", "credentials replaced; rebuilding clients")
			}
			stopGeneration()
			credentialHold = false
			lastCredentialFailure = time.Time{}
			reloaded = true
			a.noticeMu.Lock()
			delete(a.notified, "credentials")
			a.noticeMu.Unlock()
			a.restartDelay = time.Second
			start()
		case err := <-generationDone:
			generationCancel()
			generationCancel = nil
			generationDone = nil
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errIdentityChanged) {
				start()
				continue
			}
			bounded := false
			if credentialsError(err) && source.State() == token.Ready {
				a.health("initializing", "refreshing rejected credential")
				state, refreshErr := source.RefreshNow(ctx)
				if ctx.Err() != nil {
					return nil
				}
				if state == token.Ready && refreshErr == nil {
					now := time.Now()
					bounded = !lastCredentialFailure.IsZero() && now.Sub(lastCredentialFailure) <= 5*time.Minute
					lastCredentialFailure = now
					if !bounded {
						a.mu.Lock()
						a.l.Counters["credential_refresh_restarts"]++
						saveErr := a.l.Save(a.cfg.Endpoints.StateFile)
						a.mu.Unlock()
						if saveErr != nil {
							return saveErr
						}
						start()
						continue
					}
				}
			} else if !credentialsError(err) {
				lastCredentialFailure = time.Time{}
			}
			if source.State() != token.Ready || bounded {
				credentialHold = true
				reason := "credential rejected; run gc gchat auth"
				if bounded {
					reason = "two consecutive credential rejections within five minutes after refresh; run gc gchat auth"
					a.log.Warn("credential refresh recovery exhausted", "cause", safeReason("repeated credential rejection", err))
				}
				a.health("needs-sign-in", reason)
				a.notice("credentials")
			} else {
				a.health("initializing", safeReason("connection interrupted; retrying", err))
				a.log.Warn("adapter connection interrupted", "operation", "connect", "cause", safeReason("retrying", err))
				retryTimer = time.NewTimer(a.nextRestartDelay())
				retry = retryTimer.C
			}
		case <-retry:
			retry = nil
			start()
		case <-ticker.C:
			if a.checkStateLock() != nil {
				recovery = true
				stopGeneration()
				continue
			}
			if a.opts.EnvFile != "" && a.opts.SettingsPath != "" {
				data, readErr := os.ReadFile(a.opts.SettingsPath)
				if readErr == nil && !bytes.Equal(data, settingsBytes) {
					settingsBytes = data
					next, loadErr := a.loadSettings()
					if loadErr != nil {
						a.log.Warn("settings replacement rejected; keeping previous settings", "operation", "settings_reload")
					} else {
						if !recovery {
							a.health("initializing", "settings changed; rebuilding clients")
						}
						stopGeneration()
						if next.Identity().TokenFile != a.cfg.Identity().TokenFile {
							fresh, tokenErr := token.New(next.Secrets.OAuthClientFile, next.Identity().TokenFile, next.Endpoints, token.Scopes)
							if tokenErr != nil {
								a.log.Warn("settings credential rejected; keeping previous settings", "operation", "settings_reload")
								start()
								continue
							}
							stopWatch()
							source = fresh
							stopWatch = watchToken(ctx, source, reload)
							credentialHold = false
							lastCredentialFailure = time.Time{}
							reloaded = true
						}
						a.cfg = next
						a.s.SetConfig(*next)
						a.registrationNeeded.Store(true)
						if !recovery {
							a.mu.Lock()
							for _, space := range a.l.Spaces {
								space.Wake = ""
							}
							err = a.l.Save(a.cfg.Endpoints.StateFile)
							a.mu.Unlock()
							if err != nil {
								return err
							}
						}
						start()
					}
				}
			}
			if !recovery && source.State() == token.NeedsSignIn && !credentialHold {
				a.health("needs-sign-in", "credential rejected; run gc gchat auth")
				stopGeneration()
				credentialHold = true
				a.notice("credentials")
			}
		}
	}
}

func (a *application) nextRestartDelay() time.Duration {
	healthy := a.healthyGeneration.Swap(false)
	if a.restartDelay == 0 || healthy {
		a.restartDelay = time.Second
	}
	delay := a.restartDelay/2 + time.Duration(rand.Int64N(int64(a.restartDelay/2)))
	a.restartDelay = min(2*a.restartDelay, time.Minute)
	return delay
}

// The initial registration also runs without credentials. Generations join
// that attempt, rather than posting a second registration at startup.
func (a *application) ensureRegistered(ctx context.Context, registered *atomic.Bool) error {
	a.registrationMu.Lock()
	defer a.registrationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if registered.Load() && !a.gcDown.Load() && !a.registrationNeeded.Load() {
		return nil
	}
	a.registrationNeeded.Store(false)
	if err := a.s.Register(ctx); err != nil {
		a.registrationNeeded.Store(true)
		return err
	}
	registered.Store(true)
	a.gcDown.Store(false)
	return nil
}

func watchToken(ctx context.Context, source *token.Source, reload chan struct{}) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); source.Watch(ctx, func() { signal(reload) }) }()
	return func() { cancel(); <-done }
}

func (a *application) loadSettings() (*config.Config, error) {
	loaded, err := config.Load(a.opts.EnvFile, a.opts.SettingsPath, a.cfg.SecretsDir)
	if err != nil {
		return nil, err
	}
	// Process/service coordinates are immutable for this run. Only TOML settings
	// are watched; changing a secrets file requires a supervised service restart.
	next := *a.cfg
	next.Settings = loaded.Settings
	next.Endpoints.MediaMaxBytes = loaded.Endpoints.MediaMaxBytes
	if loaded.Settings.Media.InboundDir != a.cfg.Settings.Media.InboundDir {
		next.Endpoints.MediaDir = loaded.Endpoints.MediaDir
	}
	if loaded.Settings.Media.UploadRoot != a.cfg.Settings.Media.UploadRoot {
		next.Endpoints.UploadRoot = loaded.Endpoints.UploadRoot
	}
	if err = canonicalize(&next); err != nil {
		return nil, err
	}
	return &next, nil
}

func canonicalize(cfg *config.Config) error {
	for _, p := range []*string{&cfg.Endpoints.MediaDir, &cfg.Endpoints.UploadRoot} {
		if *p == "" {
			return errors.New("app: media and upload directories are required")
		}
		if err := os.MkdirAll(*p, 0700); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(*p)
		if err != nil {
			return err
		}
		*p, err = filepath.Abs(resolved)
		if err != nil {
			return err
		}
	}
	return nil
}
