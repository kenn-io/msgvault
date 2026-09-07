package store

import (
	"fmt"
	"strings"
)

// BeeperAttachmentClassification contains the derived metadata and role
// evidence for one attachment in an archived Beeper payload.
type BeeperAttachmentClassification struct {
	Metadata  string
	IsPreview bool
	IsSticker bool
}

// SetBeeperAttachmentClassifications refreshes per-attachment Beeper metadata
// and roles without replacing rows or touching occurrence identity. Attachments
// absent from the archived payload become unknown so stale rows fail closed.
func (s *Store) SetBeeperAttachmentClassifications(
	messageID int64, classifications map[string]BeeperAttachmentClassification,
) (int64, error) {
	var changed int64
	err := s.withTx(func(tx *loggedTx) error {
		resetQuery := fmt.Sprintf(`
			UPDATE attachments
			SET attachment_metadata = %s,
			    attachment_role = ?,
			    role_source = ?
			WHERE message_id = ? AND source_attachment_id LIKE 'beeper:%%'
			  AND (
			      %s
			      OR attachment_role != ?
			      OR role_source != ?
			  )
		`, s.dialect.JSONBindExpr(), s.dialect.JSONIsDistinctExpr("attachment_metadata"))
		resetArgs := []any{
			nullIfEmpty(""), string(AttachmentRoleUnknown), string(AttachmentRoleSourceUnknown),
			messageID, nullIfEmpty(""), string(AttachmentRoleUnknown), string(AttachmentRoleSourceUnknown),
		}
		if len(classifications) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(classifications)), ",")
			resetQuery += " AND source_attachment_id NOT IN (" + placeholders + ")"
			for sourceAttachmentID := range classifications {
				resetArgs = append(resetArgs, sourceAttachmentID)
			}
		}
		result, err := tx.Exec(resetQuery, resetArgs...)
		if err != nil {
			return fmt.Errorf("clear stale Beeper attachment classifications: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count stale Beeper attachment classifications: %w", err)
		}
		changed += n

		for sourceAttachmentID, classification := range classifications {
			role := AttachmentRoleStandalone
			roleSource := AttachmentRoleSourceImporterSemantics
			if classification.IsSticker {
				role = AttachmentRoleSticker
				roleSource = AttachmentRoleSourceProviderExplicit
			} else if classification.IsPreview {
				role = AttachmentRolePreview
			}
			result, err := tx.Exec(fmt.Sprintf(`
				UPDATE attachments
				SET attachment_metadata = %s,
				    attachment_role = ?,
				    role_source = ?
				WHERE message_id = ? AND source_attachment_id = ?
				  AND (
				      %s
				      OR attachment_role != ?
				      OR role_source != ?
				  )
			`, s.dialect.JSONBindExpr(), s.dialect.JSONIsDistinctExpr("attachment_metadata")),
				nullIfEmpty(classification.Metadata), string(role), string(roleSource),
				messageID, sourceAttachmentID, nullIfEmpty(classification.Metadata), string(role), string(roleSource))
			if err != nil {
				return fmt.Errorf("classify Beeper attachment %s: %w", sourceAttachmentID, err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("count Beeper attachment %s: %w", sourceAttachmentID, err)
			}
			changed += n
		}
		if changed > 0 {
			if err := s.bumpDerivedDataRevision(tx); err != nil {
				return fmt.Errorf("advance Beeper attachment cache revision: %w", err)
			}
		}

		return nil
	})
	return changed, err
}
