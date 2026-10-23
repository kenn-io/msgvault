package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

const personFactEvidenceMessageBackfillChunk = 1000

// personFactEvidenceMessageID caches a recognized versioned archive coordinate.
// Alignment validates evidence; opaque or undecodable refs have no local link.
func personFactEvidenceMessageID(input personfacts.EvidenceInput) *int64 {
	if input.SourceClass != personfacts.EvidenceArchive || !strings.HasPrefix(input.SourceRef, "person-sweep/v1:") {
		return nil
	}
	ref, err := peoplesweep.DecodePersonSweepEvidenceRef(input.SourceRef)
	if err != nil {
		return nil
	}
	return &ref.MessageID
}

// migratePersonFactEvidenceMessageIDV1 runs after legacy column migrations.
// Each chunk commits separately; the nullable link itself is the resume marker.
func (s *Store) migratePersonFactEvidenceMessageIDV1(ctx context.Context) error {
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_person_fact_evidence_message ON person_fact_evidence(person_id, message_id)`); err != nil {
			return fmt.Errorf("index person fact evidence messages: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	var after int64
	for {
		count, skipped := 0, 0
		err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			rows, err := tx.QueryContext(ctx, `SELECT id, source_ref FROM person_fact_evidence
    WHERE source_class='archive' AND source_ref LIKE 'person-sweep/v1:%' AND message_id IS NULL AND id > ?
    ORDER BY id LIMIT ?`, after, personFactEvidenceMessageBackfillChunk)
			if err != nil {
				return fmt.Errorf("read person fact evidence message backfill: %w", err)
			}
			type link struct{ id, messageID int64 }
			links := make([]link, 0, personFactEvidenceMessageBackfillChunk)
			for rows.Next() {
				var id int64
				var ref string
				if err := rows.Scan(&id, &ref); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan person fact evidence message backfill: %w", err)
				}
				count++
				after = id
				decoded, err := peoplesweep.DecodePersonSweepEvidenceRef(ref)
				if err != nil {
					skipped++
					continue
				}
				links = append(links, link{id, decoded.MessageID})
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterate person fact evidence message backfill: %w", err)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, item := range links {
				if _, err := tx.ExecContext(ctx, `UPDATE person_fact_evidence SET message_id=? WHERE id=? AND message_id IS NULL`, item.messageID, item.id); err != nil {
					return fmt.Errorf("backfill person fact evidence message: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if skipped > 0 {
			slog.Warn("skipped malformed person fact evidence message links during migration", "count", skipped)
		}
		if count < personFactEvidenceMessageBackfillChunk {
			return nil
		}
	}
}
