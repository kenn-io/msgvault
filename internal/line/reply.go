package line

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// ReplyReceipt is safe to return to the owner. Acceptance is not delivery.
// Retrying a receipt always refers to its original admitted draft revision.
type ReplyReceipt struct {
	DraftID         string     `json:"draft_id"`
	Revision        int64      `json:"revision"`
	SourceID        int64      `json:"source_id"`
	ConversationID  int64      `json:"conversation_id"`
	Status          string     `json:"status"`
	Accepted        bool       `json:"accepted"`
	RemoteMessageID string     `json:"remote_message_id,omitempty"`
	MessageID       int64      `json:"message_id,omitempty"`
	RequestID       string     `json:"request_id,omitempty"`
	RetryAfter      *time.Time `json:"retry_after,omitempty"`
}

// ReplySender serializes one source's sends with sync and maintenance.
// Client checks owner policy and prepares credentials without network I/O.
type ReplySender struct {
	Store  *store.Store
	Client func(context.Context, store.LineReplyAttempt) (*Client, error)
	Now    func() time.Time
}

func replyReceipt(a store.LineReplyAttempt) ReplyReceipt {
	r := ReplyReceipt{DraftID: a.DraftID, Revision: a.Revision, SourceID: a.SourceID, ConversationID: a.ConversationID,
		Status: a.State, RemoteMessageID: a.RemoteMessageID, MessageID: a.MessageID, RequestID: a.RequestID}
	switch a.State {
	case store.LineReplySent, store.LineReplyRemoteAccepted, store.LineReplyAcceptedUnresolved, store.LineReplyAcceptedUnresolvedExpired:
		r.Accepted = true
	}
	if a.State == store.LineReplyRemoteAccepted {
		r.Status = "sent_archive_pending"
	}
	if a.State == store.LineReplyAdmitted || a.State == store.LineReplyRemoteUnknown || a.State == store.LineReplyAcceptedUnresolved {
		deadline := a.NextRetryAt
		r.RetryAfter = &deadline
	}
	return r
}

func replyDone(a store.LineReplyAttempt) bool {
	switch a.State {
	case store.LineReplySent, store.LineReplyProviderRejected, store.LineReplyRemoteUnknownExpired, store.LineReplyAcceptedUnresolvedExpired:
		return true
	}
	return false
}

func replyRequest(a store.LineReplyAttempt) (PushTextRequest, error) {
	prefix, id, ok := strings.Cut(a.SourceConversationID, ":")
	valid := ok && validPushRecipient(id)
	switch prefix {
	case "user":
		valid = valid && id[0] == 'U' && a.ConversationType == "direct_chat"
	case "group":
		valid = valid && id[0] == 'C' && a.ConversationType == "group_chat"
	case "room":
		valid = valid && id[0] == 'R' && a.ConversationType == "group_chat"
	default:
		valid = false
	}
	if !valid {
		return PushTextRequest{}, store.ErrChatDraftInvalidDestination
	}
	if err := ValidateReplyText(a.Body); err != nil {
		return PushTextRequest{}, err
	}
	return PushTextRequest{To: id, Text: a.Body, QuoteToken: a.QuoteToken}, nil
}

func (s *ReplySender) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Send admits once, then recovers only that immutable request and retry UUID.
// A nonempty receipt remains authoritative even if local archive work fails.
func (s *ReplySender) Send(ctx context.Context, id string, revision int64) (receipt ReplyReceipt, err error) {
	if s.Store == nil {
		return receipt, errors.New("line: reply store unavailable")
	}
	if id == "" || revision <= 0 {
		return receipt, errors.New("line: draft ID and positive revision required")
	}
	a, err := s.Store.GetLineReplyAttemptContext(ctx, id)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	if exists {
		if a.Revision != revision {
			return receipt, store.ErrChatDraftRevisionConflict
		}
		if replyDone(a) {
			return replyReceipt(a), nil
		}
	} else {
		draft, lookupErr := s.Store.GetChatDraftContext(ctx, id)
		if lookupErr != nil {
			if !errors.Is(lookupErr, store.ErrChatDraftNotFound) {
				return receipt, lookupErr
			}
			// Another sender can commit the receipt and delete the draft
			// between our two reads. Resolve its admitted revision again.
			current, readErr := s.Store.GetLineReplyAttemptContext(ctx, id)
			if readErr != nil {
				if errors.Is(readErr, sql.ErrNoRows) {
					return receipt, lookupErr
				}
				return receipt, readErr
			}
			if current.Revision != revision {
				return receipt, store.ErrChatDraftRevisionConflict
			}
			if replyDone(current) {
				return replyReceipt(current), nil
			}
			a = current
		} else {
			if draft.SourceType != "line" {
				return receipt, store.ErrChatDraftUnsupportedSource
			}
			a.SourceID = draft.SourceID
		}
	}
	execution, err := s.Store.AcquireSyncExecutionContext(ctx, a.SourceID)
	if err != nil {
		// A concurrent sender or retirement may have finished since our first read.
		if current, readErr := s.Store.GetLineReplyAttemptContext(ctx, id); readErr == nil && current.Revision == revision && replyDone(current) {
			return replyReceipt(current), nil
		}
		return replyReceipt(a), err
	}
	defer func() { err = errors.Join(err, execution.Release()) }()
	a, err = s.Store.GetLineReplyAttemptContext(ctx, id)
	exists = err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	var client *Client
	authorize := func(candidate store.LineReplyAttempt) error {
		if _, err := replyRequest(candidate); err != nil {
			return err
		}
		if s.Client == nil {
			return errors.New("line: reply sending is not configured")
		}
		var err error
		client, err = s.Client(ctx, candidate)
		if err != nil {
			return err
		}
		if client == nil {
			return errors.New("line: reply client unavailable")
		}
		return nil
	}
	if !exists {
		a, err = s.Store.AdmitLineReplyContext(ctx, id, revision, s.now(), authorize)
		if err != nil {
			return receipt, err
		}
	}
	if a.Revision != revision {
		return receipt, store.ErrChatDraftRevisionConflict
	}
	if replyDone(a) {
		return replyReceipt(a), nil
	}
	if a.RemoteMessageID != "" {
		return s.complete(ctx, a)
	}
	now := s.now()
	if !now.Before(a.RetryUntil) {
		a, err = s.Store.StartLineReplyRequestContext(ctx, id, now, now)
		if errors.Is(err, store.ErrLineReplyExpired) {
			err = nil
		}
		return replyReceipt(a), err
	}
	if now.Before(a.NextRetryAt) {
		return replyReceipt(a), nil
	}
	if err = s.Store.ValidateLineReplyContext(ctx, id); err != nil {
		return replyReceipt(a), err
	}
	// Admission already authorized its exact snapshot; recovery must recheck.
	if client == nil {
		if err = authorize(a); err != nil {
			return replyReceipt(a), err
		}
	}
	input, err := replyRequest(a)
	if err != nil {
		return replyReceipt(a), err
	}
	first := a.State == store.LineReplyAdmitted
	now = s.now()
	a, err = s.Store.StartLineReplyRequestContext(ctx, id, now, now.Add(30*time.Second))
	if errors.Is(err, store.ErrLineReplyExpired) || errors.Is(err, store.ErrLineReplyRetryWait) {
		return replyReceipt(a), nil
	}
	if err != nil {
		return replyReceipt(a), err
	}
	timeout := min(30*time.Second, a.RetryUntil.Sub(now))
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	result, pushErr := client.PushText(requestCtx, a.RetryKey, input)
	cancel()
	status, retryAfter := result.StatusCode, result.RetryAfter
	if apiErr, ok := errors.AsType[*APIError](pushErr); ok {
		if status == 0 {
			status = apiErr.StatusCode
		}
		if apiErr.RetryAfter.After(retryAfter) {
			retryAfter = apiErr.RetryAfter
		}
	}
	deadline := s.now().Add(30 * time.Second)
	if retryAfter.After(deadline) {
		deadline = retryAfter
	}
	evidenceCtx, cleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanup()
	beforeOutcome := a
	a, err = s.Store.RecordLineReplyOutcomeContext(evidenceCtx, id, store.LineReplyOutcome{
		Accepted: result.Accepted, DefiniteRejection: first && !result.Accepted && status >= 400 && status < 500 && status != 409,
		StatusCode: status, MessageID: result.MessageID, QuoteToken: result.QuoteToken, RequestID: result.RequestID, NextRetryAt: deadline})
	if err != nil {
		// Preserve the admitted identity even if the Store could not read at all.
		receipt = replyReceipt(beforeOutcome)
		if result.Accepted && !errors.Is(err, store.ErrLineReplyReceiptConflict) {
			receipt.Accepted = true
			receipt.RemoteMessageID = result.MessageID
			receipt.RequestID = result.RequestID
			receipt.Status = "accepted_receipt_pending"
			receipt.RetryAfter = &deadline
		}
		return receipt, err
	}
	if a.RemoteMessageID != "" {
		return s.complete(evidenceCtx, a)
	}
	return replyReceipt(a), nil
}

func (s *ReplySender) complete(ctx context.Context, a store.LineReplyAttempt) (ReplyReceipt, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	completed, err := s.Store.CompleteLineReplyContext(cleanupCtx, a.DraftID)
	if err != nil {
		return replyReceipt(a), err
	}
	return replyReceipt(completed), nil
}
