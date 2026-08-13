package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type AttachmentChangeConsumer struct {
	ConsumerKey            string
	BaselineSequence       int64
	LastSequence           int64
	ReconciliationComplete bool
}

type AttachmentChange struct {
	Sequence         int64
	EventKind        string
	OldMessageID     *int64
	NewMessageID     *int64
	OldAttachmentID  *int64
	NewAttachmentID  *int64
	OldContentHash   *string
	NewContentHash   *string
	OldSourcePartKey *string
	NewSourcePartKey *string
	OldRole          *string
	NewRole          *string
	CreatedAt        time.Time
}

var ErrAttachmentChangeConsumerMissing = errors.New("attachment change consumer is not registered")

// RegisterAttachmentChangeConsumer establishes a race-free journal boundary.
// PostgreSQL explicitly waits out and blocks attachment/message writers;
// SQLite's first INSERT takes its one database writer lock. Changes committed
// before that boundary are covered by the required full reconciliation, while
// every later relevant change is journaled.
func (s *Store) RegisterAttachmentChangeConsumer(
	ctx context.Context,
	consumerKey string,
) (AttachmentChangeConsumer, bool, error) {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil {
		return AttachmentChangeConsumer{}, false, err
	}
	var consumer AttachmentChangeConsumer
	var created bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if s.IsPostgreSQL() {
			if _, err := q.Exec(`LOCK TABLE attachments, messages IN SHARE MODE`); err != nil {
				return fmt.Errorf("lock attachment change registration boundary: %w", err)
			}
		}
		result, err := q.Exec(`
			INSERT INTO attachment_change_consumers
				(consumer_key, baseline_sequence, last_sequence, reconciliation_complete)
			VALUES (?, 0, 0, FALSE)
			ON CONFLICT (consumer_key) DO NOTHING`, consumerKey)
		if err != nil {
			return fmt.Errorf("register attachment change consumer: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read attachment consumer registration result: %w", err)
		}
		created = rows == 1
		if created {
			var baseline int64
			if err := q.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM attachment_change_log`).Scan(&baseline); err != nil {
				return fmt.Errorf("capture attachment consumer baseline: %w", err)
			}
			if _, err := q.Exec(`
				UPDATE attachment_change_consumers
				SET baseline_sequence = ?, last_sequence = ?, updated_at = `+s.dialect.Now()+`
				WHERE consumer_key = ?`, baseline, baseline, consumerKey); err != nil {
				return fmt.Errorf("record attachment consumer baseline: %w", err)
			}
		}
		return scanAttachmentChangeConsumer(q, consumerKey, &consumer)
	})
	return consumer, created, err
}

func (s *Store) CompleteAttachmentChangeReconciliation(
	ctx context.Context,
	consumerKey string,
	baselineSequence int64,
) error {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil || baselineSequence < 0 {
		if err != nil {
			return err
		}
		return errors.New("attachment reconciliation baseline cannot be negative")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		result, err := q.Exec(`
			UPDATE attachment_change_consumers
			SET reconciliation_complete = TRUE, updated_at = `+s.dialect.Now()+`
			WHERE consumer_key = ? AND baseline_sequence = ?
			  AND reconciliation_complete = FALSE`, consumerKey, baselineSequence)
		if err != nil {
			return fmt.Errorf("complete attachment change reconciliation: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read attachment reconciliation result: %w", err)
		}
		if rows != 1 {
			var storedBaseline int64
			var complete bool
			if err := q.QueryRow(`
				SELECT baseline_sequence, reconciliation_complete
				FROM attachment_change_consumers WHERE consumer_key = ?`, consumerKey).
				Scan(&storedBaseline, &complete); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAttachmentChangeConsumerMissing
				}
				return fmt.Errorf("recheck attachment reconciliation completion: %w", err)
			}
			if storedBaseline == baselineSequence && complete {
				return nil
			}
			return errors.New("attachment change consumer baseline changed concurrently")
		}
		return nil
	})
}

func (s *Store) GetAttachmentChangeConsumer(
	ctx context.Context,
	consumerKey string,
) (AttachmentChangeConsumer, error) {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil {
		return AttachmentChangeConsumer{}, err
	}
	q := boundQuerier{ctx: ctx, q: s.db}
	var consumer AttachmentChangeConsumer
	if err := scanAttachmentChangeConsumer(q, consumerKey, &consumer); err != nil {
		return AttachmentChangeConsumer{}, err
	}
	return consumer, nil
}

func (s *Store) ListAttachmentChanges(
	ctx context.Context,
	consumerKey string,
	limit int,
) ([]AttachmentChange, error) {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 10_000 {
		return nil, errors.New("attachment change page limit must be between 1 and 10000")
	}
	consumer, err := s.GetAttachmentChangeConsumer(ctx, consumerKey)
	if err != nil {
		return nil, err
	}
	if !consumer.ReconciliationComplete {
		return nil, errors.New("attachment change consumer requires full reconciliation before replay")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT sequence, event_kind, old_message_id, new_message_id,
		       old_attachment_id, new_attachment_id,
		       old_content_hash, new_content_hash,
		       old_source_part_key, new_source_part_key,
		       old_role, new_role, created_at
		FROM attachment_change_log
		WHERE sequence > ? ORDER BY sequence LIMIT ?`), consumer.LastSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("list attachment changes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	changes := make([]AttachmentChange, 0, limit)
	for rows.Next() {
		var change AttachmentChange
		var oldMessageID, newMessageID, oldAttachmentID, newAttachmentID sql.NullInt64
		var oldHash, newHash, oldPart, newPart, oldRole, newRole sql.NullString
		if err := rows.Scan(
			&change.Sequence, &change.EventKind, &oldMessageID, &newMessageID,
			&oldAttachmentID, &newAttachmentID, &oldHash, &newHash,
			&oldPart, &newPart, &oldRole, &newRole, &change.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan attachment change: %w", err)
		}
		change.OldMessageID = nullInt64Pointer(oldMessageID)
		change.NewMessageID = nullInt64Pointer(newMessageID)
		change.OldAttachmentID = nullInt64Pointer(oldAttachmentID)
		change.NewAttachmentID = nullInt64Pointer(newAttachmentID)
		change.OldContentHash = nullStringPointer(oldHash)
		change.NewContentHash = nullStringPointer(newHash)
		change.OldSourcePartKey = nullStringPointer(oldPart)
		change.NewSourcePartKey = nullStringPointer(newPart)
		change.OldRole = nullStringPointer(oldRole)
		change.NewRole = nullStringPointer(newRole)
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachment changes: %w", err)
	}
	return changes, nil
}

// AdvanceAttachmentChangeConsumer acknowledges an inclusive, previously
// journaled sequence and prunes only the prefix consumed by every registered
// feature. Advancing one consumer can never discard another's unread events.
func (s *Store) AdvanceAttachmentChangeConsumer(
	ctx context.Context,
	consumerKey string,
	sequence int64,
) error {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil {
		return err
	}
	if sequence < 0 {
		return errors.New("attachment change sequence cannot be negative")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var current int64
		var complete bool
		if err := q.QueryRow(`
			SELECT last_sequence, reconciliation_complete
			FROM attachment_change_consumers WHERE consumer_key = ?`, consumerKey).Scan(&current, &complete); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAttachmentChangeConsumerMissing
			}
			return fmt.Errorf("read attachment consumer cursor: %w", err)
		}
		if !complete {
			return errors.New("attachment change consumer reconciliation is incomplete")
		}
		if sequence <= current {
			return pruneAttachmentChanges(q)
		}
		if sequence > current {
			var exists bool
			if err := q.QueryRow(`SELECT EXISTS (
				SELECT 1 FROM attachment_change_log WHERE sequence = ?
			)`, sequence).Scan(&exists); err != nil {
				return fmt.Errorf("verify attachment change acknowledgement: %w", err)
			}
			if !exists {
				return errors.New("attachment change acknowledgement is not a retained event")
			}
			result, err := q.Exec(`
				UPDATE attachment_change_consumers
				SET last_sequence = ?, updated_at = `+s.dialect.Now()+`
				WHERE consumer_key = ? AND last_sequence = ?`, sequence, consumerKey, current)
			if err != nil {
				return fmt.Errorf("advance attachment change consumer: %w", err)
			}
			updated, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read attachment consumer advance result: %w", err)
			}
			if updated != 1 {
				var advanced int64
				if err := q.QueryRow(`
					SELECT last_sequence FROM attachment_change_consumers
					WHERE consumer_key = ?`, consumerKey).Scan(&advanced); err != nil {
					return fmt.Errorf("recheck attachment consumer cursor: %w", err)
				}
				if advanced >= sequence {
					return pruneAttachmentChanges(q)
				}
				return errors.New("attachment change consumer cursor changed concurrently")
			}
		}
		return pruneAttachmentChanges(q)
	})
}

func (s *Store) UnregisterAttachmentChangeConsumer(ctx context.Context, consumerKey string) error {
	if err := validateAttachmentConsumerKey(consumerKey); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		if s.IsPostgreSQL() {
			if _, err := q.Exec(`LOCK TABLE attachments, messages IN SHARE MODE`); err != nil {
				return fmt.Errorf("lock attachment change unregistration boundary: %w", err)
			}
		}
		if _, err := q.Exec(`DELETE FROM attachment_change_consumers WHERE consumer_key = ?`, consumerKey); err != nil {
			return fmt.Errorf("unregister attachment change consumer: %w", err)
		}
		return pruneAttachmentChanges(q)
	})
}

func scanAttachmentChangeConsumer(q querier, key string, consumer *AttachmentChangeConsumer) error {
	err := q.QueryRow(`
		SELECT consumer_key, baseline_sequence, last_sequence, reconciliation_complete
		FROM attachment_change_consumers WHERE consumer_key = ?`, key).Scan(
		&consumer.ConsumerKey, &consumer.BaselineSequence,
		&consumer.LastSequence, &consumer.ReconciliationComplete,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAttachmentChangeConsumerMissing
	}
	if err != nil {
		return fmt.Errorf("read attachment change consumer: %w", err)
	}
	return nil
}

func pruneAttachmentChanges(q querier) error {
	if _, err := q.Exec(`
		DELETE FROM attachment_change_log
		WHERE sequence <= COALESCE(
			(SELECT MIN(last_sequence) FROM attachment_change_consumers),
			(SELECT COALESCE(MAX(sequence), 0) FROM attachment_change_log)
		)`); err != nil {
		return fmt.Errorf("prune consumed attachment changes: %w", err)
	}
	return nil
}

func validateAttachmentConsumerKey(key string) error {
	if key == "" || len(key) > 200 || strings.TrimSpace(key) != key {
		return errors.New("attachment change consumer key is invalid")
	}
	for _, character := range key {
		if unicode.IsControl(character) {
			return errors.New("attachment change consumer key is invalid")
		}
	}
	return nil
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}
