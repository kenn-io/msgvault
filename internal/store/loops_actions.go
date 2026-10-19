package store

import (
	"context"
	"fmt"
	"time"
)

// withLiveLoopContext rereads owner state after acquiring the conversation write lock.
func (s *Store) withLiveLoopContext(ctx context.Context, id int64, fn func(*loggedTx, OpenLoop) error) (OpenLoop, error) {
	initial, err := s.GetOpenLoopContext(ctx, id)
	if err != nil {
		return OpenLoop{}, err
	}
	result, err := retryBusyWrite(ctx, s, "update open loop", func() (*OpenLoop, error) {
		var updated OpenLoop
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockLoopConversationContext(ctx, tx, initial.ConversationID); err != nil {
				return err
			}
			live, err := scanOpenLoop(tx.QueryRowContext(ctx, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", id))
			if err != nil {
				return err
			}
			if live.Status == "closed" {
				return ErrLoopAlreadyClosed
			}
			if err := fn(tx, live); err != nil {
				return err
			}
			updated, err = scanOpenLoop(tx.QueryRowContext(ctx, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", id))
			return err
		})
		if err != nil {
			return nil, err
		}
		return &updated, nil
	})
	if err != nil {
		return OpenLoop{}, err
	}
	return *result, nil
}

func (s *Store) CloseLoopContext(ctx context.Context, id int64, note string, now time.Time) (OpenLoop, error) {
	if now.IsZero() {
		return OpenLoop{}, fmt.Errorf("%w: time required", ErrInvalidLoop)
	}
	return s.withLiveLoopContext(ctx, id, func(tx *loggedTx, live OpenLoop) error {
		if note != "" {
			evidence, err := loopEvidenceMetadata(live.EvidenceJSON, "owner_note", note)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE open_loops SET evidence_json=? WHERE id=?", string(evidence), id); err != nil {
				return err
			}
		}
		return s.closeLoopTx(ctx, tx, id, "done", "user", now, nil)
	})
}
func (s *Store) SnoozeLoopContext(ctx context.Context, id int64, until, now time.Time) (OpenLoop, error) {
	if now.IsZero() || !until.After(now) {
		return OpenLoop{}, fmt.Errorf("%w: snooze must be in the future", ErrInvalidLoop)
	}
	return s.withLiveLoopContext(ctx, id, func(tx *loggedTx, _ OpenLoop) error {
		_, err := tx.ExecContext(ctx, "UPDATE open_loops SET snoozed_until=?,updated_at=? WHERE id=? AND status<>'closed'", s.dialect.TimestampParam(until), s.dialect.TimestampParam(now), id)
		return err
	})
}
func (s *Store) UnsnoozeLoopContext(ctx context.Context, id int64, now time.Time) (OpenLoop, error) {
	if now.IsZero() {
		return OpenLoop{}, fmt.Errorf("%w: time required", ErrInvalidLoop)
	}
	return s.withLiveLoopContext(ctx, id, func(tx *loggedTx, _ OpenLoop) error {
		_, err := tx.ExecContext(ctx, "UPDATE open_loops SET snoozed_until=NULL,updated_at=? WHERE id=? AND status<>'closed'", s.dialect.TimestampParam(now), id)
		return err
	})
}
