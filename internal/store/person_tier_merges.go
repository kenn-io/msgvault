package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A surviving presence row keeps its original time; an absorbed manual grant
// upgrades only its provenance and actor so automation cannot remove it.
func (s *Store) mergePersonTierGrantProvenanceTx(ctx context.Context, tx *loggedTx, table string, survivorID, absorbedID int64) error {
	switch table {
	case "person_tracking", "person_brief_enrollments":
	default:
		return fmt.Errorf("merge person tier grant: unsupported table %q", table)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET provenance='manual',actor=(SELECT actor FROM `+table+` WHERE person_id=?)
  WHERE person_id=? AND provenance='auto' AND EXISTS(SELECT 1 FROM `+table+` WHERE person_id=? AND provenance='manual')`, absorbedID, survivorID, absorbedID); err != nil {
		return fmt.Errorf("merge %s manual provenance: %w", table, err)
	}
	return nil
}

func (s *Store) reconcilePersonTierMergeTx(ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,lane FROM person_tier_opt_outs WHERE person_id=? ORDER BY id`, absorbedID)
	if err != nil {
		return fmt.Errorf("load absorbed person tier opt-outs: %w", err)
	}
	type optOutRow struct {
		id   int64
		lane string
	}
	var optOuts []optOutRow
	for rows.Next() {
		var row optOutRow
		if err := rows.Scan(&row.id, &row.lane); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan absorbed person tier opt-out: %w", err)
		}
		optOuts = append(optOuts, row)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("iterate absorbed person tier opt-outs: %w", err)
	}
	for _, row := range optOuts {
		var survivorOptOutID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM person_tier_opt_outs WHERE person_id=? AND lane=?`, survivorID, row.lane).Scan(&survivorOptOutID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find survivor person tier opt-out: %w", err)
		}
		action := personMergeActionDeduplicated
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `UPDATE person_tier_opt_outs SET person_id=? WHERE id=?`, survivorID, row.id); err != nil {
				return fmt.Errorf("move absorbed person tier opt-out: %w", err)
			}
			survivorOptOutID = row.id
			action = "moved"
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM person_tier_opt_outs WHERE id=?`, row.id); err != nil {
			return fmt.Errorf("deduplicate absorbed person tier opt-out: %w", err)
		}
		if err := s.setPersonMergeRowDispositionTx(ctx, tx, mergeID, "person_tier_opt_outs", row.id, action, &survivorOptOutID); err != nil {
			return err
		}
	}
	if err := s.reconcilePersonTierOptOutsTx(ctx, tx, survivorID); err != nil {
		return err
	}
	// Opt-out reconciliation can remove a grant captured on either side.
	// Journal that deliberate removal in every unsplit merge that follows the
	// row, so either split order can restore its original grant from a snapshot.
	for _, table := range []string{"person_tracking", "person_brief_enrollments"} {
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE person_id=?)`, survivorID).Scan(&present); err != nil {
			return fmt.Errorf("inspect merged %s presence: %w", table, err)
		}
		if !present {
			if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows SET action='deleted_snapshot',current_row_id=NULL,current_row_key=NULL
    WHERE table_name=? AND current_row_id=? AND split_id IS NULL`, table, survivorID); err != nil {
				return fmt.Errorf("journal opt-out removal of merged %s: %w", table, err)
			}
		}
	}
	for _, table := range []string{"person_tier_state", "person_tier_observations"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE person_id=?`, absorbedID); err != nil {
			return fmt.Errorf("remove absorbed %s: %w", table, err)
		}
	}
	// Preserve the earliest absorbed origin when a profile is merged repeatedly.
	if _, err := tx.ExecContext(ctx, `UPDATE person_tier_events SET person_id=?,merged_from_person_id=COALESCE(merged_from_person_id,?) WHERE person_id=?`, survivorID, absorbedID, absorbedID); err != nil {
		return fmt.Errorf("move absorbed person tier history: %w", err)
	}
	return nil
}
