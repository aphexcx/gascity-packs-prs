package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
)

// Accept the message name, the provenance item, and the change IDs emitted by
// the adapter. Never treat an arbitrary suffix as part of a resource name.
var messageReference = regexp.MustCompile(`^(?:gchat:)?(spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*)(#deleted|#updated#([^#]+)|/reactions/[A-Za-z0-9_-]+#(?:created|deleted))?$`)
var threadReference = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/threads/[A-Za-z0-9_.-]+$`)

type referenceLookupError struct{ error }

func (e referenceLookupError) Unwrap() error { return e.error }

func parseMessageReference(value string) (message, space string, err error) {
	m := messageReference.FindStringSubmatch(value)
	if m == nil {
		return "", "", errors.New("--to must name a Chat message: spaces/SPACE/messages/MESSAGE, or its gchat: item or adapter change id")
	}
	if m[3] != "" {
		if _, err := time.Parse(time.RFC3339Nano, m[3]); err != nil {
			return "", "", errors.New("--to has an invalid message update time")
		}
	}
	return m[1], strings.Split(m[1], "/messages/")[0], nil
}

func validateThread(space, thread string) error {
	if thread == "" {
		return nil
	}
	if !threadReference.MatchString(thread) || !strings.HasPrefix(thread, space+"/threads/") {
		return fmt.Errorf("thread %q does not belong to conversation %q", thread, space)
	}
	return nil
}

// Legacy intents must obey the same address checks as fresh requests before
// restoring their payload or contacting Google during retry and recovery.
func validateSendAddress(send *ledger.SendState) error {
	if err := validateThread(send.Space, send.Thread); err != nil {
		return err
	}
	if send.Request != nil {
		if send.Request.Conversation.ConversationID != send.Space {
			return errors.New("saved request conversation disagrees with send conversation")
		}
		return validateThread(send.Space, send.Request.ReplyToMessageID)
	}
	return nil
}

// resolveReference runs before any delivery. The caller's optional identifiers
// are assertions, never alternative destinations.
func (s *Server) resolveReference(ctx context.Context, cmd *commandRequest) (string, error) {
	if cmd.To == "" {
		if cmd.TopLevel && cmd.Thread != "" {
			return "", errors.New("--top-level cannot be combined with --thread")
		}
		return "", validateThread(cmd.ConversationID, cmd.Thread)
	}
	message, space, err := parseMessageReference(cmd.To)
	if err != nil {
		return "", err
	}
	if cmd.ConversationID != "" && cmd.ConversationID != space {
		return "", fmt.Errorf("conversation %q disagrees with --to conversation %q", cmd.ConversationID, space)
	}
	if err := validateThread(space, cmd.Thread); err != nil {
		return "", err
	}
	l, err := s.snapshot(ctx)
	if err != nil {
		return "", err
	}
	thread := ""
	if old := l.Sends[cmd.IdempotencyKey]; old != nil {
		if old.Space != space {
			return "", errors.New("idempotency key already belongs to another conversation")
		}
		if err := validateSendAddress(old); err != nil {
			return "", err
		}
	}
	// The first durable intent is also the address on retries, including when
	// the original message is no longer readable from Google.
	if old := l.Sends[cmd.IdempotencyKey]; old != nil && old.InReplyTo == message && old.Request != nil {
		thread = old.Request.ReplyToMessageID
	} else if known := l.Messages[message]; known != nil {
		thread = known.Thread
	} else {
		if err := s.available(); err != nil {
			return "", referenceLookupError{err}
		}
		c, _ := s.clients()
		m, err := c.GetMessage(ctx, message)
		if err != nil {
			return "", referenceLookupError{err}
		}
		if m.Name != message {
			return "", fmt.Errorf("message %q was returned for --to %q", m.Name, message)
		}
		if m.Thread != nil {
			thread = m.Thread.Name
		}
	}
	if err := validateThread(space, thread); err != nil {
		return "", err
	}
	if cmd.TopLevel {
		thread = ""
	}
	if cmd.Thread != "" && cmd.Thread != thread {
		return "", fmt.Errorf("thread %q disagrees with --to thread %q", cmd.Thread, thread)
	}
	cmd.ConversationID, cmd.Thread = space, thread
	return message, nil
}

func addressResult(result *CommandResult, send *ledger.SendState) {
	result.ConversationID, result.SpaceName = send.Space, send.SpaceName
	result.Thread, result.InReplyTo = send.Thread, send.InReplyTo
}

func receiptAddress(result *CommandResult, metadata map[string]string) {
	if thread, ok := metadata["thread"]; ok {
		result.Thread = thread
	}
}
