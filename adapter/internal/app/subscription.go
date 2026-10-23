package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
		for _, item := range ge.Errors {
			switch item.Reason {
			case "accessNotConfigured", "SERVICE_DISABLED":
				cause = "Workspace Events API not enabled"
			case "iamPermissionDenied", "IAM_PERMISSION_DENIED":
				cause = "topic Publisher IAM permission denied"
			}
		}
		return fmt.Sprintf("subscription refused: %s (HTTP %d)", cause, ge.Code)
	}
	return safeReason("subscription reconciliation pending", err)
}

func (a *application) subscriptionFailure(err error) {
	reason := subscriptionReason(err)
	a.health("catching-up", reason)
	a.notice("subscription", reason)
}

func (a *application) subscriptionRecovered() {
	a.noticeMu.Lock()
	hadFailure := a.notified["subscription"]
	delete(a.notified, "subscription")
	a.noticeMu.Unlock()
	if hadFailure {
		a.log.Info("subscription recovered", "operation", "subscription")
	}
}

// Keep the subscriber's diagnostics on the same sanitized logging boundary as
// app-owned calls, including RenewLoop errors after startup has completed.
type subscriptionLogHandler struct{ slog.Handler }

func safeSubscriptionAttr(a slog.Attr) slog.Attr {
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
