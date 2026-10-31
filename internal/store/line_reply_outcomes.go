package store

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// LineReplyOutcome contains only safe receipt fields, never provider error
// bodies. DefiniteRejection is valid only for the live first submission;
// a recovered or previously uncertain submission must leave it false.
type LineReplyOutcome struct {
	Accepted          bool
	DefiniteRejection bool
	StatusCode        int
	MessageID         string
	QuoteToken        string
	RequestID         string
	NextRetryAt       time.Time
}

func lineReplyHasAcceptance(state string) bool {
	return state == LineReplyAcceptedUnresolved || state == LineReplyAcceptedUnresolvedExpired || state == LineReplyRemoteAccepted || state == LineReplySent
}

func lineReplyExpired(state string) bool {
	return state == LineReplyRemoteUnknownExpired || state == LineReplyAcceptedUnresolvedExpired
}

func lineReplyTerminal(state string) bool {
	return state == LineReplySent || state == LineReplyProviderRejected
}

// Every state writer takes source-before-attempt ownership. Evidence writes
// remain possible after retirement; admission and actual requests do not.
func (s *Store) changeLineReply(ctx context.Context, id string, change func(*loggedTx, *LineReplyAttempt) error) (LineReplyAttempt, error) {
	a, err := s.GetLineReplyAttemptContext(ctx, id)
	if err != nil {
		return a, err
	}
	if err := s.requireSyncSource(a.SourceID); err != nil {
		return a, err
	}
	committed := a
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockSyncSourceTx(ctx, tx, a.SourceID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE line_reply_attempts SET state=state WHERE draft_id=?`, id); err != nil {
			return err
		}
		current, err := loadLineReply(ctx, tx, id)
		if err != nil {
			return err
		}
		committed = current
		if err := change(tx, &current); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE line_reply_attempts SET state=?,next_retry_at=?,status_code=?,request_id=?,remote_message_id=?,remote_quote_token=? WHERE draft_id=?`,
			current.State, current.NextRetryAt, current.StatusCode, current.RequestID, current.RemoteMessageID, current.RemoteQuoteToken, id)
		if err != nil {
			return fmt.Errorf("record LINE reply state: %w", err)
		}
		a = current
		return nil
	})
	if err != nil {
		return committed, err
	}
	return a, err
}

// StartLineReplyRequestContext commits possible submission before provider I/O.
// Expiry is durable, and acceptance evidence never becomes merely unknown.
func (s *Store) StartLineReplyRequestContext(ctx context.Context, id string, now, nextRetry time.Time) (LineReplyAttempt, error) {
	var resultErr error
	now = now.UTC().Truncate(time.Microsecond)
	a, err := s.changeLineReply(ctx, id, func(tx *loggedTx, a *LineReplyAttempt) error {
		if lineReplyTerminal(a.State) || a.RemoteMessageID != "" {
			return ErrLineReplyInvalidState
		}
		if lineReplyExpired(a.State) || !now.Before(a.RetryUntil) {
			accepted := lineReplyHasAcceptance(a.State)
			a.State = LineReplyRemoteUnknownExpired
			if accepted {
				a.State = LineReplyAcceptedUnresolvedExpired
			}
			resultErr = ErrLineReplyExpired
			return nil // persist the stop before returning the caller-facing error
		}
		if now.Before(a.NextRetryAt) {
			resultErr = ErrLineReplyRetryWait
			return nil
		}
		if err := validateLineReplyBinding(ctx, tx, *a, true); err != nil {
			return err
		}
		if a.State == LineReplyAdmitted {
			a.State = LineReplyRemoteUnknown
		}
		minimum := now.Add(30 * time.Second)
		if nextRetry.Before(minimum) {
			nextRetry = minimum
		}
		nextRetry = nextRetry.UTC().Truncate(time.Microsecond)
		if nextRetry.After(a.NextRetryAt) {
			a.NextRetryAt = nextRetry
		}
		return nil
	})
	if err != nil {
		return a, err
	}
	return a, resultErr
}

// RecordLineReplyOutcomeContext preserves known acceptance on later failures.
// The only path that unfreezes a draft is a definite first-request rejection.
func (s *Store) RecordLineReplyOutcomeContext(ctx context.Context, id string, outcome LineReplyOutcome) (LineReplyAttempt, error) {
	prior, err := s.GetLineReplyAttemptContext(ctx, id)
	if err != nil || lineReplyTerminal(prior.State) {
		return prior, err
	}
	return s.changeLineReply(ctx, id, func(_ *loggedTx, a *LineReplyAttempt) error {
		if lineReplyTerminal(a.State) {
			return nil
		}
		known := lineReplyHasAcceptance(a.State)
		expired := lineReplyExpired(a.State)
		if outcome.Accepted {
			if a.RemoteMessageID != "" && outcome.MessageID != "" && a.RemoteMessageID != outcome.MessageID {
				return ErrLineReplyReceiptConflict
			}
			if known && a.RequestID != "" && outcome.RequestID != "" && a.RequestID != outcome.RequestID {
				return ErrLineReplyReceiptConflict
			}
			if outcome.MessageID != "" {
				a.RemoteMessageID = outcome.MessageID
			}
			if a.RemoteQuoteToken == "" {
				a.RemoteQuoteToken = outcome.QuoteToken
			}
			if outcome.RequestID != "" {
				a.RequestID = outcome.RequestID
			}
			a.StatusCode = outcome.StatusCode
			a.State = LineReplyAcceptedUnresolved
			if expired {
				a.State = LineReplyAcceptedUnresolvedExpired
			}
			if a.RemoteMessageID != "" {
				a.State = LineReplyRemoteAccepted
			}
		} else if !known && outcome.DefiniteRejection && outcome.StatusCode >= 400 && outcome.StatusCode < 500 && outcome.StatusCode != http.StatusConflict {
			a.State = LineReplyProviderRejected
			a.StatusCode = outcome.StatusCode
		} else {
			if a.State == LineReplyAdmitted {
				a.State = LineReplyRemoteUnknown
			}
			if !known {
				a.StatusCode = outcome.StatusCode
			}
		}
		deadline := outcome.NextRetryAt.UTC().Truncate(time.Microsecond)
		if deadline.After(a.NextRetryAt) {
			a.NextRetryAt = deadline
		}
		return nil
	})
}
