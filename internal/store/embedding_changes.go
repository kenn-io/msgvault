package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EmbeddingChangeKind identifies the source mutation that invalidated one or
// more context-coupled embedding documents.
type EmbeddingChangeKind string

const (
	EmbeddingChangeMessageInsert           EmbeddingChangeKind = "message_insert"
	EmbeddingChangeMessageUpdate           EmbeddingChangeKind = "message_update"
	EmbeddingChangeMessageDelete           EmbeddingChangeKind = "message_delete"
	EmbeddingChangeMessageBody             EmbeddingChangeKind = "message_body"
	EmbeddingChangeConversationTitle       EmbeddingChangeKind = "conversation_title"
	EmbeddingChangeConversationParticipant EmbeddingChangeKind = "conversation_participant"
	EmbeddingChangeParticipantDisplayName  EmbeddingChangeKind = "participant_display_name"
)

// EmbeddingChange is one committed source mutation. Old and new coordinates
// remain in the journal after a message is deleted or moved, so a worker does
// not need the current message row to repair both affected document scopes.
type EmbeddingChange struct {
	Sequence int64
	Kind     EmbeddingChangeKind

	MessageID         sql.NullInt64
	OldMessageType    sql.NullString
	NewMessageType    sql.NullString
	OldConversationID sql.NullInt64
	NewConversationID sql.NullInt64
	OldSentAt         sql.NullTime
	NewSentAt         sql.NullTime
	ParticipantID     sql.NullInt64
}

// ScanEmbeddingChanges returns events after a durable cursor in commit order.
func (s *Store) ScanEmbeddingChanges(
	ctx context.Context, after int64, limit int,
) ([]EmbeddingChange, error) {
	if limit <= 0 {
		return []EmbeddingChange{}, nil
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		       new_conversation_id, old_sent_at, new_sent_at, participant_id
		FROM embedding_changes
		WHERE sequence > ?
		ORDER BY sequence
		LIMIT ?`), after, limit)
	if err != nil {
		return nil, fmt.Errorf("scan embedding changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	changes := make([]EmbeddingChange, 0, limit)
	for rows.Next() {
		var change EmbeddingChange
		if err := rows.Scan(
			&change.Sequence,
			&change.Kind,
			&change.MessageID,
			&change.OldMessageType,
			&change.NewMessageType,
			&change.OldConversationID,
			&change.NewConversationID,
			&change.OldSentAt,
			&change.NewSentAt,
			&change.ParticipantID,
		); err != nil {
			return nil, fmt.Errorf("scan embedding change: %w", err)
		}
		if change.OldSentAt.Valid {
			change.OldSentAt.Time = change.OldSentAt.Time.UTC()
		}
		if change.NewSentAt.Valid {
			change.NewSentAt.Time = change.NewSentAt.Time.UTC()
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan embedding changes: %w", err)
	}
	return changes, nil
}

// LatestEmbeddingChangeSequence returns the last sequence allocated by a
// committed source transaction.
func (s *Store) LatestEmbeddingChangeSequence(ctx context.Context) (int64, error) {
	var sequence int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT sequence FROM embedding_change_clock WHERE singleton = 1`).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("read embedding change sequence: %w", err)
	}
	return sequence, nil
}

// EnableEmbeddingChangeJournal starts commit-ordered mutation capture. A new
// contextual generation reconciles the complete current snapshot, so changes
// made before this point do not need historical journal rows.
//
// On PostgreSQL the enable must fence in-flight source transactions: every
// source mutation statement holds the shared clock advisory lock until its
// transaction ends, and a transaction whose statements ran while capture was
// disabled produced no journal rows. Taking the exclusive form first waits
// for those transactions to finish, so each source transaction either commits
// before capture starts (visible to the reconciliation snapshot) or journals.
// SQLite needs no fence: its single-writer lock means no source transaction
// can be in flight while the enable statement runs.
func (s *Store) EnableEmbeddingChangeJournal(ctx context.Context) error {
	const enable = `UPDATE embedding_change_clock SET enabled = TRUE WHERE singleton = 1`
	if s.dialect.DriverName() != "pgx" {
		if _, err := s.db.ExecContext(ctx, enable); err != nil {
			return fmt.Errorf("enable embedding change journal: %w", err)
		}
		return nil
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.Exec(
			`SELECT pg_advisory_xact_lock(hashtextextended('msgvault.embedding_change_clock', 0))`); err != nil {
			return fmt.Errorf("fence in-flight source transactions: %w", err)
		}
		if _, err := tx.Exec(enable); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("enable embedding change journal: %w", err)
	}
	return nil
}

// PruneEmbeddingChangesThrough removes the journal prefix consumed by every
// live contextual generation. The commit-order clock remains monotonic, so a
// fresh generation can pin the current sequence and reconcile current state.
func (s *Store) PruneEmbeddingChangesThrough(ctx context.Context, sequence int64) (int64, error) {
	if sequence <= 0 {
		return 0, nil
	}
	result, err := s.db.ExecContext(ctx, s.Rebind(
		`DELETE FROM embedding_changes WHERE sequence <= ?`), sequence)
	if err != nil {
		return 0, fmt.Errorf("prune embedding changes through %d: %w", sequence, err)
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned embedding changes through %d: %w", sequence, err)
	}
	return pruned, nil
}

func coalesceLatestMessageChanges(q querier, dialect Dialect, messageID int64) error {
	query := dialect.Rebind(`
		SELECT sequence, kind, message_id, old_message_type, new_message_type, old_conversation_id,
		       new_conversation_id, old_sent_at, new_sent_at, participant_id
		  FROM embedding_changes
		 WHERE message_id = ?
		 ORDER BY sequence DESC
		 LIMIT 1 OFFSET ?`)
	changes := make([]EmbeddingChange, 0, 2)
	for offset := range 2 {
		var change EmbeddingChange
		err := q.QueryRow(query, messageID, offset).Scan(
			&change.Sequence, &change.Kind, &change.MessageID,
			&change.OldMessageType, &change.NewMessageType,
			&change.OldConversationID, &change.NewConversationID,
			&change.OldSentAt, &change.NewSentAt, &change.ParticipantID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return fmt.Errorf("scan latest message journal change: %w", err)
		}
		changes = append(changes, change)
	}
	if len(changes) != 2 || !sameEmbeddingChangeScope(changes[0], changes[1]) {
		return nil
	}
	if _, err := q.Exec(dialect.Rebind(
		`DELETE FROM embedding_changes WHERE sequence = ?`), changes[1].Sequence); err != nil {
		return fmt.Errorf("coalesce duplicate message journal change: %w", err)
	}
	return nil
}

func sameEmbeddingChangeScope(left, right EmbeddingChange) bool {
	return left.MessageID == right.MessageID &&
		left.OldMessageType == right.OldMessageType &&
		left.NewMessageType == right.NewMessageType &&
		left.OldConversationID == right.OldConversationID &&
		left.NewConversationID == right.NewConversationID &&
		nullTimesEqual(left.OldSentAt, right.OldSentAt) &&
		nullTimesEqual(left.NewSentAt, right.NewSentAt)
}

func nullTimesEqual(left, right sql.NullTime) bool {
	return left.Valid == right.Valid && (!left.Valid || left.Time.Equal(right.Time))
}
