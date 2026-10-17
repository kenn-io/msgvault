package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SourceParticipantRef identifies a participant within one source account.
type SourceParticipantRef struct {
	SourceID      int64
	ParticipantID int64
}

// PeopleNativeOwnersContext reports candidate participants with any archived,
// source-native outbound message on the same source. It does not depend on the
// messages selected for a retrieval request.
func (s *Store) PeopleNativeOwnersContext(
	ctx context.Context,
	refs []SourceParticipantRef,
) (map[SourceParticipantRef]bool, error) {
	owners := make(map[SourceParticipantRef]bool)
	if len(refs) == 0 {
		return owners, nil
	}
	unique := make(map[SourceParticipantRef]struct{}, len(refs))
	ordered := make([]SourceParticipantRef, 0, len(refs))
	for _, ref := range refs {
		if ref.SourceID <= 0 || ref.ParticipantID <= 0 {
			return nil, errors.New("people native owners: source and participant IDs must be positive")
		}
		if _, exists := unique[ref]; exists {
			continue
		}
		unique[ref] = struct{}{}
		ordered = append(ordered, ref)
	}
	slices.SortFunc(ordered, func(a, b SourceParticipantRef) int {
		if n := cmp.Compare(a.SourceID, b.SourceID); n != 0 {
			return n
		}
		return cmp.Compare(a.ParticipantID, b.ParticipantID)
	})

	const batchSize = 400
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		for start := 0; start < len(ordered); start += batchSize {
			end := min(start+batchSize, len(ordered))
			values := make([]string, end-start)
			args := make([]any, 0, 2*(end-start))
			for i, ref := range ordered[start:end] {
				values[i] = "(CAST(? AS BIGINT),CAST(? AS BIGINT))"
				args = append(args, ref.SourceID, ref.ParticipantID)
			}
			query := `WITH candidates(source_id,participant_id) AS (VALUES ` + strings.Join(values, ",") + `)
SELECT DISTINCT c.source_id,c.participant_id
FROM candidates c
WHERE EXISTS (
 SELECT 1 FROM messages m
 WHERE m.source_id=c.source_id
   AND COALESCE(m.source_is_from_me,FALSE)=TRUE
   AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
   AND (
    m.sender_id=c.participant_id
    OR (
     m.sender_id IS NULL
     AND EXISTS (
      SELECT 1 FROM message_recipients owner_from
      WHERE owner_from.message_id=m.id
        AND LOWER(owner_from.recipient_type)='from'
        AND owner_from.participant_id=c.participant_id
     )
     AND (
      SELECT COUNT(DISTINCT owner_from.participant_id)
      FROM message_recipients owner_from
      WHERE owner_from.message_id=m.id
        AND LOWER(owner_from.recipient_type)='from'
        AND owner_from.participant_id IS NOT NULL
     )=1
    )
   )
)`
			rows, err := tx.QueryContext(ctx, s.dialect.Rebind(query), args...)
			if err != nil {
				return fmt.Errorf("read source-native owner candidates: %w", err)
			}
			for rows.Next() {
				var ref SourceParticipantRef
				if err := rows.Scan(&ref.SourceID, &ref.ParticipantID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan source-native owner candidate: %w", err)
				}
				owners[ref] = true
			}
			rowsErr := rows.Err()
			if err := rows.Close(); err != nil && rowsErr == nil {
				rowsErr = err
			}
			if rowsErr != nil {
				return fmt.Errorf("iterate source-native owner candidates: %w", rowsErr)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return owners, nil
}
