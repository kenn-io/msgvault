package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// DirectoryPeopleForParticipantsContext resolves a batch of observed
// participant IDs to the durable Directory summaries that cover them. The
// lookup is cluster-safe because person_participants is populated for every
// member of a promoted identity cluster. It returns one summary per bound
// participant and omits unsaved participants.
//
// The read uses the same projection snapshot and cadence hydration as the
// Directory endpoint. Callers therefore preserve cadence freshness and must
// not infer a schedule from analytical interaction timestamps when a
// participant is absent from the result.
func (s *Store) DirectoryPeopleForParticipantsContext(
	ctx context.Context, participantIDs []int64,
) (map[int64]DirectoryPersonSummary, error) {
	ids := uniquePositiveIDs(participantIDs)
	result := make(map[int64]DirectoryPersonSummary)
	if len(ids) == 0 {
		return result, nil
	}

	err := s.withFreshDirectorySnapshotContext(ctx, func(tx *loggedTx) error {
		placeholders, args := sortedIDPlaceholders(ids)
		rows, err := tx.QueryContext(ctx, `SELECT participant_id, person_id
			FROM person_participants
			WHERE participant_id IN (`+placeholders+`)
			ORDER BY participant_id, person_id`, args...)
		if err != nil {
			return fmt.Errorf("resolve directory people for participants: %w", err)
		}
		personIDs := make([]int64, 0, len(ids))
		participantsByPerson := make(map[int64][]int64)
		personByParticipant := make(map[int64]int64)
		seenPersons := make(map[int64]struct{})
		for rows.Next() {
			var participantID, personID int64
			if err := rows.Scan(&participantID, &personID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan directory person binding: %w", err)
			}
			if previous, ok := personByParticipant[participantID]; ok && previous != personID {
				_ = rows.Close()
				return fmt.Errorf("participant %d is bound to multiple directory people", participantID)
			}
			personByParticipant[participantID] = personID
			participantsByPerson[personID] = append(participantsByPerson[personID], participantID)
			if _, seen := seenPersons[personID]; !seen {
				seenPersons[personID] = struct{}{}
				personIDs = append(personIDs, personID)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate directory person bindings: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close directory person bindings: %w", err)
		}
		if len(personIDs) == 0 {
			return nil
		}
		slices.Sort(personIDs)
		candidates := make([]directoryPersonCandidate, len(personIDs))
		for i, personID := range personIDs {
			candidates[i].summary.ID = personID
		}
		if err := s.hydrateDirectoryPeopleTx(ctx, tx, candidates); err != nil {
			return err
		}
		if err := s.hydrateDirectoryCadenceTx(ctx, tx, candidates, time.Now().UTC()); err != nil {
			return err
		}
		byPerson := make(map[int64]DirectoryPersonSummary, len(candidates))
		for _, candidate := range candidates {
			byPerson[candidate.summary.ID] = candidate.summary
		}
		for personID, members := range participantsByPerson {
			summary, ok := byPerson[personID]
			if !ok {
				return fmt.Errorf("directory person %d disappeared during hydration", personID)
			}
			for _, participantID := range members {
				result[participantID] = summary
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func uniquePositiveIDs(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		ids = append(ids, value)
	}
	slices.Sort(ids)
	return ids
}
