package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// ListPersonKnownEvidenceMessages returns unique archive message IDs whose
// evidence is still supported by the latest status event and is cited by a
// non-invalid fact claim or a supported observation. Evidence is ordered by
// its persisted event time, newest first. Unsupported rows are paged past so
// a burst of retracted evidence cannot hide later usable anchors.
func (s *Store) ListPersonKnownEvidenceMessages(
	ctx context.Context,
	r peoplesweep.KnownEvidenceRequest,
) ([]int64, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}

	messageIDs := make([]int64, 0, r.Limit)
	seen := make(map[int64]struct{}, r.Limit)
	pageSize := r.Limit * 4
	scopeKind := string(r.TargetKind)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		for offset := 0; len(messageIDs) < r.Limit; offset += pageSize {
			rows, err := tx.QueryContext(ctx, `
				SELECT e.id, e.message_id
				  FROM person_fact_evidence e
				 WHERE e.person_id = ? AND e.source_class = 'archive' AND e.message_id IS NOT NULL
				   AND (EXISTS (SELECT 1
				                  FROM person_fact_claim_evidence ce
				                  JOIN person_fact_claims c ON c.id = ce.claim_id
				                 WHERE ce.evidence_id = e.id AND c.person_id = e.person_id
				                   AND c.relation <> 'invalid'
				                   AND (? = '' OR (c.target_kind = ? AND c.target_key = ?)))
				        OR EXISTS (SELECT 1
				                     FROM person_observation_evidence oe
				                     JOIN person_observations o ON o.id = oe.observation_id
				                    WHERE oe.evidence_id = e.id AND o.person_id = e.person_id
				                      AND o.status IN ('active', 'superseded', 'rejected')
				                      AND oe.exclusion_reason = ''
				                      AND (? = '' OR (o.target_kind = ? AND o.target_key = ?))))
				 ORDER BY e.event_time DESC, e.id DESC
				 LIMIT ? OFFSET ?`,
				r.PersonID,
				scopeKind, scopeKind, r.TargetKey,
				scopeKind, scopeKind, r.TargetKey,
				pageSize, offset)
			if err != nil {
				return fmt.Errorf("list person known evidence: %w", err)
			}

			evidenceIDs := make([]int64, 0, pageSize)
			pageMessages := make([]int64, 0, pageSize)
			for rows.Next() {
				var evidenceID, messageID int64
				if err := rows.Scan(&evidenceID, &messageID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan person known evidence: %w", err)
				}
				evidenceIDs = append(evidenceIDs, evidenceID)
				pageMessages = append(pageMessages, messageID)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterate person known evidence: %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close person known evidence: %w", err)
			}

			supported, err := s.latestEvidenceSupportedTx(ctx, tx, evidenceIDs)
			if err != nil {
				return err
			}
			for index, messageID := range pageMessages {
				if !supported[evidenceIDs[index]] {
					continue
				}
				if _, ok := seen[messageID]; ok {
					continue
				}
				seen[messageID] = struct{}{}
				messageIDs = append(messageIDs, messageID)
				if len(messageIDs) == r.Limit {
					break
				}
			}
			if len(evidenceIDs) < pageSize {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return messageIDs, nil
}
