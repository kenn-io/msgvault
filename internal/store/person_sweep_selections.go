package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/novelty"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func validatePersonSweepSelections(records []peoplesweep.SelectionRecord) error {
	seen := make(map[[2]string]struct{}, len(records))
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("selection %s/%s: %w", record.Purpose, record.TargetKey, err)
		}
		key := [2]string{string(record.Purpose), record.TargetKey}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate selection %s/%s", record.Purpose, record.TargetKey)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (s *Store) insertPersonSweepSelectionsTx(
	ctx context.Context, tx *loggedTx, attemptID string, records []peoplesweep.SelectionRecord,
) error {
	if err := validatePersonSweepSelections(records); err != nil {
		return err
	}
	for _, record := range records {
		var recency, novel, fill int
		for _, item := range record.Items {
			switch item.Lane {
			case novelty.LaneRecency:
				recency++
			case novelty.LaneNovelty:
				novel++
			case novelty.LaneFill:
				fill++
			case novelty.LaneMandatory:
				// Mandatory picks contribute only to selected_count.
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO person_sweep_selections
   (attempt_id, purpose, target_key, policy_version, mode, fallback_reason,
    params_fingerprint, vector_generation_id, vector_fingerprint, anchor_set_sha256,
    pool_count, scored_count, anchor_count, anchor_scored_count, selected_count,
    recency_count, novelty_count, fill_count, selection_sha256, elapsed_microseconds)
   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			attemptID, record.Purpose, record.TargetKey, record.PolicyVersion, record.Mode,
			record.FallbackReason, record.ParamsFingerprint, record.VectorGenerationID,
			record.VectorFingerprint, record.AnchorSetSHA256, record.PoolCount, record.ScoredCount,
			record.AnchorCount, record.AnchorScoredCount, len(record.Items), recency, novel, fill,
			record.SelectionSHA256, record.ElapsedMicroseconds)
		if err != nil {
			return fmt.Errorf("insert selection %s/%s: %w", record.Purpose, record.TargetKey, err)
		}
		for _, item := range record.Items {
			sent := 0
			if item.Sent() {
				sent = 1
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO person_sweep_selection_items
    (attempt_id, purpose, target_key, ordinal, message_id, evidence_id, lane, sent,
     batch_ordinal, novelty_bp, support_bp, score_bp)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, attemptID, record.Purpose,
				record.TargetKey, item.Ordinal, item.MessageID, item.EvidenceID, item.Lane, sent,
				item.BatchOrdinal, item.NoveltyBP, item.SupportBP, item.ScoreBP)
			if err != nil {
				return fmt.Errorf("insert selection item %s/%s/%d: %w", record.Purpose, record.TargetKey, item.Ordinal, err)
			}
		}
	}
	return nil
}

// RecordPersonSweepSelections replaces each named target while its attempt is running.
// Other target records remain intact, and the replacement is atomic.
// Callers must not replace a target's record after any batch carrying its
// recorded items has started, so sent novelty picks remain in cooldown history.
func (s *Store) RecordPersonSweepSelections(
	ctx context.Context, attemptID string, records []peoplesweep.SelectionRecord,
) error {
	if err := validatePersonSweepSelections(records); err != nil {
		return fmt.Errorf("record person sweep selections: %w", err)
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		if attempt.status != peoplesweep.AttemptRunning {
			return errors.New("person sweep attempt is not running")
		}
		for _, record := range records {
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_sweep_selection_items
    WHERE attempt_id = ? AND purpose = ? AND target_key = ?`, attemptID,
				record.Purpose, record.TargetKey); err != nil {
				return fmt.Errorf("delete previous selection items: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_sweep_selections
    WHERE attempt_id = ? AND purpose = ? AND target_key = ?`, attemptID,
				record.Purpose, record.TargetKey); err != nil {
				return fmt.Errorf("delete previous selection: %w", err)
			}
		}
		return s.insertPersonSweepSelectionsTx(ctx, tx, attemptID, records)
	})
	if err != nil {
		return fmt.Errorf("record person sweep selections: %w", err)
	}
	return nil
}

// ListPersonSweepSelections returns target records and picks in priority order
// from one read snapshot.
func (s *Store) ListPersonSweepSelections(
	ctx context.Context, attemptID string,
) ([]peoplesweep.SelectionRecord, error) {
	var records []peoplesweep.SelectionRecord
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT purpose, target_key, policy_version, mode,
   fallback_reason, params_fingerprint, vector_generation_id, vector_fingerprint,
   anchor_set_sha256, pool_count, scored_count, anchor_count, anchor_scored_count,
   selection_sha256, elapsed_microseconds
   FROM person_sweep_selections WHERE attempt_id = ? ORDER BY purpose, target_key`, attemptID)
		if err != nil {
			return err
		}
		if err := func() (err error) {
			defer func() {
				if closeErr := rows.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("close person sweep selections: %w", closeErr))
				}
			}()
			for rows.Next() {
				var record peoplesweep.SelectionRecord
				if err := rows.Scan(&record.Purpose, &record.TargetKey, &record.PolicyVersion,
					&record.Mode, &record.FallbackReason, &record.ParamsFingerprint,
					&record.VectorGenerationID, &record.VectorFingerprint, &record.AnchorSetSHA256,
					&record.PoolCount, &record.ScoredCount, &record.AnchorCount,
					&record.AnchorScoredCount, &record.SelectionSHA256, &record.ElapsedMicroseconds); err != nil {
					return err
				}
				records = append(records, record)
			}
			return rows.Err()
		}(); err != nil {
			return err
		}
		for i := range records {
			if err := listPersonSweepSelectionItemsTx(ctx, tx, attemptID, &records[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list person sweep selections: %w", err)
	}
	return records, nil
}

func listPersonSweepSelectionItemsTx(
	ctx context.Context, tx *loggedTx, attemptID string, record *peoplesweep.SelectionRecord,
) (err error) {
	rows, err := tx.QueryContext(ctx, `SELECT ordinal, message_id, evidence_id, lane,
  batch_ordinal, novelty_bp, support_bp, score_bp FROM person_sweep_selection_items
  WHERE attempt_id = ? AND purpose = ? AND target_key = ? ORDER BY ordinal`,
		attemptID, record.Purpose, record.TargetKey)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close person sweep selection items: %w", closeErr))
		}
	}()
	for rows.Next() {
		var item peoplesweep.SelectionItem
		if err := rows.Scan(&item.Ordinal, &item.MessageID, &item.EvidenceID, &item.Lane,
			&item.BatchOrdinal, &item.NoveltyBP, &item.SupportBP, &item.ScoreBP); err != nil {
			return err
		}
		record.Items = append(record.Items, item)
	}
	return rows.Err()
}

// ListRecentNoveltyPicks returns only picks whose carrying call was dispatched.
// A failed dispatched call counts; a reserved or cancelled call does not.
func (s *Store) ListRecentNoveltyPicks(
	ctx context.Context, personID int64, since time.Time,
) (picked map[int64]struct{}, err error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT i.message_id
  FROM person_sweep_attempts a
  JOIN person_sweep_selection_items i ON i.attempt_id = a.id
  WHERE a.person_id = ? AND a.started_at >= ?
   AND i.lane = 'novelty' AND i.batch_ordinal IS NOT NULL
   AND EXISTS (SELECT 1 FROM person_sweep_batches b
    WHERE b.attempt_id = i.attempt_id AND b.batch_ordinal = i.batch_ordinal
     AND b.status IN ('running', 'succeeded', 'failed')
     AND ((i.purpose = 'brief' AND b.purpose IN ('brief', 'brief_repair'))
      OR (i.purpose = 'extraction_context' AND b.purpose IN ('primary', 'repair'))))`),
		personID, s.dialect.TimestampParam(since))
	if err != nil {
		return nil, fmt.Errorf("list recent novelty picks: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close recent novelty picks: %w", closeErr))
		}
	}()
	picked = make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan recent novelty pick: %w", err)
		}
		picked[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent novelty picks: %w", err)
	}
	return picked, nil
}
