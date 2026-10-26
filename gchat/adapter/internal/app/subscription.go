package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/token"
	"google.golang.org/api/googleapi"
)

// Events permission/not-found failures describe subscription setup, not a
// revoked OAuth credential. Explicit credential sentinels still take priority.
func subscriptionCredentialsError(err error) bool {
	if errors.Is(err, token.ErrNeedsSignIn) || errors.Is(err, inbound.ErrCredentials) {
		return true
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code == 401
	}
	return credentialsError(err)
}

func subscriptionReason(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) && (ge.Code == 403 || ge.Code == 404) {
		cause := "permission denied; check enabled APIs and topic Publisher IAM"
		if ge.Code == 404 {
			cause = "subscription or configured resource not found"
		}
		// Only known provider reason codes are rendered. Free-form messages,
		// response bodies and metadata can contain credentials or private URLs.
		setCause := func(reason string) {
			switch reason {
			case "accessNotConfigured", "SERVICE_DISABLED":
				cause = "Workspace Events API not enabled"
			case "iamPermissionDenied", "IAM_PERMISSION_DENIED":
				cause = "topic Publisher IAM permission denied"
			}
		}
		for _, item := range ge.Errors {
			setCause(item.Reason)
		}
		for _, detail := range ge.Details {
			if info, ok := detail.(map[string]any); ok && info["@type"] == "type.googleapis.com/google.rpc.ErrorInfo" {
				if reason, ok := info["reason"].(string); ok {
					setCause(reason)
				}
			}
		}
		return fmt.Sprintf("subscription refused: %s (HTTP %d)", cause, ge.Code)
	}
	return safeReason("subscription reconciliation pending", err)
}

func (a *application) subscriptionFailure(err error) {
	reason := subscriptionReason(err)
	a.health("catching-up", reason)
	var ge *googleapi.Error
	notify := errors.As(err, &ge) && (ge.Code == 403 || ge.Code == 404)
	if m := a.manager.Load(); m != nil {
		// A successful reconcile may have been followed by more failures
		// between monitor ticks. Preserve that incident boundary for dedup.
		select {
		case <-m.FailureRecovered():
			a.subscriptionRecovered()
		default:
		}
		// The manager counts completed attempts and resets on success. Reading
		// its count keeps monitor polls from counting the same failure again.
		_, failures, _, _ := m.Status()
		notify = notify || failures >= 3
	}
	if notify {
		a.notice("subscription", reason)
	}
}

func (a *application) subscriptionRecovered() {
	// Healthy monitor ticks and startup retries also consume the signal;
	// leaving it queued could clear dedup twice for the same recovery.
	if m := a.manager.Load(); m != nil {
		select {
		case <-m.FailureRecovered():
		default:
		}
	}
	a.noticeMu.Lock()
	hadFailure := false
	for key := range a.notified {
		if key == "subscription" || strings.HasPrefix(key, "subscription:") {
			hadFailure = true
			delete(a.notified, key)
		}
	}
	a.noticeMu.Unlock()
	if hadFailure {
		a.log.Info("subscription recovered", "operation", "subscription")
	}
}

// Keep the subscriber's diagnostics on the same sanitized logging boundary as
// app-owned calls, including RenewLoop errors after startup has completed.
type subscriptionLogHandler struct{ slog.Handler }

func safeSubscriptionAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, child := range attrs {
			clean[i] = safeSubscriptionAttr(child)
		}
		a.Value = slog.GroupValue(clean...)
		return a
	}
	if err, ok := a.Value.Any().(error); ok {
		return slog.String(a.Key, subscriptionReason(err))
	}
	return a
}

func (h subscriptionLogHandler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool { clean.AddAttrs(safeSubscriptionAttr(a)); return true })
	return h.Handler.Handle(ctx, clean)
}

func (h subscriptionLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = safeSubscriptionAttr(a)
	}
	return subscriptionLogHandler{h.Handler.WithAttrs(clean)}
}

func (h subscriptionLogHandler) WithGroup(name string) slog.Handler {
	return subscriptionLogHandler{h.Handler.WithGroup(name)}
}
