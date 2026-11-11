package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// appreciationMessageKeyTx resolves a message's native coordinates. A subset
// may retain an archive-only marker without the origin; that is not a native ID.
func appreciationMessageKeyTx(ctx context.Context, tx *loggedTx, messageID int64) (string, error) {
	var sourceType, identifier, message sql.NullString
	var archiveOnly, hasOrigin bool
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN lineage.message_id IS NOT NULL THEN origin.source_type ELSE cur_src.source_type END,
 CASE WHEN lineage.message_id IS NOT NULL THEN origin.identifier ELSE cur_src.identifier END,
 CASE WHEN lineage.message_id IS NOT NULL THEN lineage.original_message_id ELSE m.source_message_id END,
 EXISTS(SELECT 1 FROM source_merge_archive_only_messages a WHERE a.message_id = m.id),
 lineage.message_id IS NOT NULL
 FROM messages m JOIN sources cur_src ON cur_src.id = m.source_id
 LEFT JOIN source_merge_messages lineage ON lineage.message_id = m.id
 LEFT JOIN sources origin ON origin.id = lineage.original_source_id WHERE m.id = ?`, messageID).
		Scan(&sourceType, &identifier, &message, &archiveOnly, &hasOrigin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve appreciation message identity: %w", err)
	}
	if archiveOnly && !hasOrigin || !sourceType.Valid || !identifier.Valid || !message.Valid ||
		!personSweepSourceAuthenticatesSender(sourceType.String) {
		return "", nil
	}
	key, err := peoplesweep.AppreciationContributionKey(sourceType.String, identifier.String, message.String)
	if err != nil {
		return "", nil // Incomplete native coordinates are unsupported.
	}
	return key, nil
}

// copyAppreciationDismissalTx preserves both tombstones and the destination's
// first dismissal. A proof belongs only to its exact key and is never copied.
func copyAppreciationDismissalTx(ctx context.Context, tx *loggedTx, from, into string) error {
	if from == "" || into == "" || from == into {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_appreciation_state
 (contribution_key, dismissed_person_uid, dismissed_by, dismissed_at)
 SELECT ?, dismissed_person_uid, dismissed_by, dismissed_at FROM person_appreciation_state
 WHERE contribution_key = ? AND dismissed_at IS NOT NULL
 ON CONFLICT (contribution_key) DO UPDATE SET
 dismissed_person_uid = CASE WHEN person_appreciation_state.dismissed_at IS NULL THEN excluded.dismissed_person_uid ELSE person_appreciation_state.dismissed_person_uid END,
 dismissed_by = CASE WHEN person_appreciation_state.dismissed_at IS NULL THEN excluded.dismissed_by ELSE person_appreciation_state.dismissed_by END,
 dismissed_at = COALESCE(person_appreciation_state.dismissed_at, excluded.dismissed_at)`, into, from)
	if err != nil {
		return fmt.Errorf("copy appreciation dismissal: %w", err)
	}
	return nil
}

func copyAppreciationMessageDismissalTx(ctx context.Context, tx *loggedTx, from, into int64) error {
	oldKey, err := appreciationMessageKeyTx(ctx, tx, from)
	if err != nil {
		return err
	}
	newKey, err := appreciationMessageKeyTx(ctx, tx, into)
	if err != nil {
		return err
	}
	return copyAppreciationDismissalTx(ctx, tx, oldKey, newKey)
}
