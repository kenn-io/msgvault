package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MessageDateRepairCandidate is an email row whose canonical sent time is
// missing or outside the configured plausibility range.
type MessageDateRepairCandidate struct {
	ID             int64
	SentAt         sql.NullTime
	InternalDate   sql.NullTime
	LastModifiedAt string
}

// MessageDateRepair updates one message's canonical sent time.
type MessageDateRepair struct {
	ID                     int64
	SentAt                 time.Time
	ExpectedLastModifiedAt string
}

func (s *Store) messageDateOutsideBoundsPredicate(column string) string {
	boundExpr := "?"
	if !s.IsPostgreSQL() {
		column = "julianday(" + column + ")"
		boundExpr = "julianday(?)"
	}
	return column + " < " + boundExpr + " OR " + column + " > " + boundExpr
}

// ListMessageDateRepairCandidates returns email rows whose canonical sent time
// is missing or outside the inclusive bounds.
func (s *Store) ListMessageDateRepairCandidates(
	ctx context.Context,
	lowerBound, upperBound time.Time,
) ([]MessageDateRepairCandidate, error) {
	query := fmt.Sprintf(`
		SELECT m.id, m.sent_at, m.internal_date, CAST(m.last_modified AS TEXT)
		FROM messages m
		WHERE m.message_type = 'email'
		  AND (
			m.sent_at IS NULL
			OR %s
		  )
		ORDER BY m.id
	`, s.messageDateOutsideBoundsPredicate("m.sent_at"))
	rows, err := s.db.QueryContext(ctx, query, lowerBound.UTC(), upperBound.UTC())
	if err != nil {
		return nil, fmt.Errorf("query date repair candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []MessageDateRepairCandidate
	for rows.Next() {
		var candidate MessageDateRepairCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.SentAt,
			&candidate.InternalDate,
			&candidate.LastModifiedAt,
		); err != nil {
			return nil, fmt.Errorf("scan date repair candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate date repair candidates: %w", err)
	}
	return candidates, nil
}

// ApplyMessageDateRepairs atomically updates candidates that are still missing
// or implausible. A row changed after planning aborts the batch instead of
// overwriting newer data.
func (s *Store) ApplyMessageDateRepairs(
	ctx context.Context,
	repairs []MessageDateRepair,
	lowerBound, upperBound time.Time,
) error {
	updateSQL := fmt.Sprintf(`
		UPDATE messages
		SET sent_at = ?
		WHERE id = ?
		  AND (
			sent_at IS NULL
			OR %s
		  )
		  AND CAST(last_modified AS TEXT) = ?
	`, s.messageDateOutsideBoundsPredicate("sent_at"))

	return s.withTx(func(tx *loggedTx) error {
		for _, repair := range repairs {
			result, err := tx.ExecContext(
				ctx,
				updateSQL,
				repair.SentAt.UTC(),
				repair.ID,
				lowerBound.UTC(),
				upperBound.UTC(),
				repair.ExpectedLastModifiedAt,
			)
			if err != nil {
				return fmt.Errorf("update message %d sent_at: %w", repair.ID, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("count message %d date repair: %w", repair.ID, err)
			}
			if affected != 1 {
				return fmt.Errorf(
					"message %d changed after date repair planning",
					repair.ID,
				)
			}
		}
		return nil
	})
}
