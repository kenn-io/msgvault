package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ObserveInboxState stores only provider metadata. It never updates local UI
// read state or fetches message bodies. Callers hold the source execution lease
// so observations and local sync cannot race membership reconciliation.
func (s *Store) ObserveInboxState(ctx context.Context, state inboxcontrol.State) (bool, error) {
	changed := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		changed, err = observeInboxStateTx(ctx, tx, state)
		return err
	})
	return changed, err
}

func observeInboxStateTx(ctx context.Context, tx *loggedTx, state inboxcontrol.State) (bool, error) {
	if state.Target == (inboxcontrol.Target{}) || state.Source != (inboxcontrol.SourceIdentity{}) || state.ObservedAt.IsZero() {
		return false, fmt.Errorf("%w: item observation requires target and time", inboxcontrol.ErrInvalid)
	}
	revision, err := inboxcontrol.SemanticFingerprint(state)
	if err != nil {
		return false, err
	}
	key, err := inboxStateKey(state.Target)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("encode inbox provider observation: %w", err)
	}
	if err := validateInboxArchiveTarget(ctx, tx, state.Target); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO inbox_provider_states
			(target_key, source_id, scope, item_id, provider_id, mailbox, uidvalidity, uid, is_inbox, provider_read, marked_unread, semantic_hash, state_json, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (target_key) DO UPDATE SET is_inbox = excluded.is_inbox,
			provider_read = excluded.provider_read, marked_unread = excluded.marked_unread,
			semantic_hash = excluded.semantic_hash, state_json = excluded.state_json, observed_at = excluded.observed_at
			WHERE inbox_provider_states.observed_at < excluded.observed_at`,
		key, state.Target.SourceID, state.Target.Scope, state.Target.ItemID, state.Target.ProviderID, state.Target.Mailbox, state.Target.UIDValidity, state.Target.UID,
		state.Inbox, state.Read, state.MarkedUnread, revision, string(encoded), state.ObservedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("store inbox provider observation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 1 {
		if err := bumpInboxSourceRevisionTx(ctx, tx, state.Target.SourceID); err != nil {
			return false, err
		}
	}
	return rows == 1, nil
}

func inboxStateKey(target inboxcontrol.Target) (string, error) {
	return inboxcontrol.SemanticFingerprint(inboxcontrol.State{Target: target})
}

func validateInboxArchiveTarget(ctx context.Context, tx *loggedTx, target inboxcontrol.Target) error {
	if err := validateInboxArchiveIdentity(ctx, tx, target); err != nil {
		return err
	}
	if target.SourceType != sourceTypeIMAP {
		return nil
	}
	return validateInboxIMAPMembership(ctx, tx, target)
}

func validateInboxArchiveIdentity(ctx context.Context, tx *loggedTx, target inboxcontrol.Target) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE id = ? AND identifier = ?
		AND (source_type = ? OR (source_type = '' AND ? = 'gmail'))`, target.SourceID, target.SourceIdentifier, target.SourceType, target.SourceType).Scan(&count); err != nil {
		return fmt.Errorf("resolve inbox observation source: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: observation source differs from archive", inboxcontrol.ErrInvalid)
	}
	query := `SELECT COUNT(*) FROM messages WHERE id = ? AND source_id = ? AND source_message_id = ?`
	if target.Scope == inboxcontrol.ScopeChat {
		query = `SELECT COUNT(*) FROM conversations WHERE id = ? AND source_id = ? AND source_conversation_id = ?`
	}
	if err := tx.QueryRowContext(ctx, query, target.ItemID, target.SourceID, target.ProviderID).Scan(&count); err != nil {
		return fmt.Errorf("resolve inbox observation item: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: observation item differs from archive", inboxcontrol.ErrInvalid)
	}
	return nil
}

func validateInboxIMAPMembership(ctx context.Context, tx *loggedTx, target inboxcontrol.Target) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM imap_message_memberships
		WHERE source_id = ? AND message_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`, target.SourceID, target.ItemID, target.Mailbox, target.UIDValidity, target.UID).Scan(&count); err != nil {
		return fmt.Errorf("resolve inbox observation membership: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: observation membership differs from archive", inboxcontrol.ErrInvalid)
	}
	return nil
}

func (s *Store) GetInboxProviderState(ctx context.Context, target inboxcontrol.Target) (*inboxcontrol.State, error) {
	key, err := inboxStateKey(target)
	if err != nil {
		return nil, err
	}
	var encoded string
	err = s.db.QueryRowContext(ctx, `SELECT state_json FROM inbox_provider_states WHERE target_key = ?`, key).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // A missing observation is distinct from a read failure.
	}
	if err != nil {
		return nil, fmt.Errorf("read inbox provider observation: %w", err)
	}
	var state inboxcontrol.State
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return nil, fmt.Errorf("decode inbox provider observation: %w", err)
	}
	return &state, nil
}
