package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func mergeParticipantLoopSuppressionsTx(
	ctx context.Context, tx *loggedTx, absorbedID, survivorID int64,
) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_suppressions
		WHERE participant_id = ? AND EXISTS (
			SELECT 1 FROM loop_suppressions survivor
			WHERE survivor.participant_id = ?
		)`, absorbedID, survivorID); err != nil {
		return fmt.Errorf("deduplicate absorbed participant loop suppression: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions
		SET participant_id = ? WHERE participant_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("transfer absorbed participant loop suppression: %w", err)
	}
	return nil
}

func mergeParticipantLoopCounterpartiesTx(
	ctx context.Context, tx *loggedTx, absorbedID, survivorID int64,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE open_loops
		SET counterparty_participant_id = ?
		WHERE counterparty_participant_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("transfer absorbed participant loop counterparties: %w", err)
	}
	return nil
}

func (s *Store) mergePersonLoopSuppressionsTx(
	ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64,
) error {
	var absorbedSuppressionID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM loop_suppressions WHERE person_id = ?`, absorbedID).
		Scan(&absorbedSuppressionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load absorbed person loop suppression: %w", err)
	}

	var survivorSuppressionID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM loop_suppressions WHERE person_id = ?`, survivorID).
		Scan(&survivorSuppressionID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `UPDATE loop_suppressions
			SET person_id = ? WHERE id = ?`, survivorID, absorbedSuppressionID); err != nil {
			return fmt.Errorf("transfer absorbed person loop suppression: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load survivor person loop suppression: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loop_suppressions WHERE id = ?`, absorbedSuppressionID); err != nil {
		return fmt.Errorf("deduplicate absorbed person loop suppression: %w", err)
	}
	if err := s.setPersonMergeRowDispositionTx(
		ctx, tx, mergeID, "loop_suppressions", absorbedSuppressionID,
		personMergeActionDeduplicated, &survivorSuppressionID,
	); err != nil {
		return fmt.Errorf("record deduplicated person loop suppression: %w", err)
	}
	return nil
}

func mergePersonLoopCounterpartiesTx(
	ctx context.Context, tx *loggedTx, survivorID, absorbedID int64,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE open_loops
		SET counterparty_person_id = ?
		WHERE counterparty_person_id = ?`, survivorID, absorbedID); err != nil {
		return fmt.Errorf("transfer absorbed person loop counterparties: %w", err)
	}
	return nil
}
