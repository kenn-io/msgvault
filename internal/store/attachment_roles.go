package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// AttachmentRole is the source-authoritative role of one attachment
// occurrence. Unknown fails closed for hosted processing.
type AttachmentRole string

const (
	AttachmentRoleStandalone AttachmentRole = "standalone"
	AttachmentRoleInline     AttachmentRole = "inline"
	AttachmentRoleAvatar     AttachmentRole = "avatar"
	AttachmentRoleThumbnail  AttachmentRole = "thumbnail"
	AttachmentRolePreview    AttachmentRole = "preview"
	AttachmentRoleSticker    AttachmentRole = "sticker"
	AttachmentRoleUIAsset    AttachmentRole = "ui_asset"
	AttachmentRoleUnknown    AttachmentRole = "unknown"
)

// AttachmentRoleSource records why an attachment role is trusted. It is
// evidence provenance, not an eligibility decision by itself.
type AttachmentRoleSource string

const (
	AttachmentRoleSourceMIMEDisposition   AttachmentRoleSource = "mime_disposition"
	AttachmentRoleSourceProviderExplicit  AttachmentRoleSource = "provider_explicit"
	AttachmentRoleSourceImporterSemantics AttachmentRoleSource = "importer_semantics"
	AttachmentRoleSourceLegacyAPI         AttachmentRoleSource = "legacy_api"
	AttachmentRoleSourceRawMIMERepair     AttachmentRoleSource = "raw_mime_repair"
	AttachmentRoleSourceUnknown           AttachmentRoleSource = "unknown"
)

// AttachmentWrite is the complete typed write contract for one attachment
// occurrence. SourcePartKey is stable within the owning source message; when
// unavailable it stays empty and the row retains legacy best-effort hash
// identity.
type AttachmentWrite struct {
	Filename           string
	MIMEType           string
	StoragePath        string
	ContentHash        string
	Size               int64
	SourceAttachmentID string
	MediaType          string
	Width              int64
	Height             int64
	DurationMS         int64
	Metadata           string
	Role               AttachmentRole
	RoleSource         AttachmentRoleSource
	SourcePartKey      string
	ContentID          string
	// State and SkipReason distinguish unfinished fetches from deliberate policy exclusions.
	State      attachmentpolicy.DownloadState
	SkipReason attachmentpolicy.SkipReason
}

func (r AttachmentRole) valid() bool {
	switch r {
	case AttachmentRoleStandalone, AttachmentRoleInline, AttachmentRoleAvatar,
		AttachmentRoleThumbnail, AttachmentRolePreview, AttachmentRoleSticker,
		AttachmentRoleUIAsset, AttachmentRoleUnknown:
		return true
	default:
		return false
	}
}

func (s AttachmentRoleSource) valid() bool {
	switch s {
	case AttachmentRoleSourceMIMEDisposition,
		AttachmentRoleSourceProviderExplicit,
		AttachmentRoleSourceImporterSemantics,
		AttachmentRoleSourceLegacyAPI,
		AttachmentRoleSourceRawMIMERepair,
		AttachmentRoleSourceUnknown:
		return true
	default:
		return false
	}
}

// AttachmentRoleFromMIME maps only explicit MIME occurrence evidence. A
// filename or media type is never enough to promote an ambiguous part.
func AttachmentRoleFromMIME(
	disposition string,
	isInline bool,
	contentID string,
) (AttachmentRole, AttachmentRoleSource) {
	switch {
	case isInline || contentID != "" || disposition == "inline":
		return AttachmentRoleInline, AttachmentRoleSourceMIMEDisposition
	case disposition == "attachment":
		return AttachmentRoleStandalone, AttachmentRoleSourceMIMEDisposition
	default:
		return AttachmentRoleUnknown, AttachmentRoleSourceUnknown
	}
}

func (w AttachmentWrite) normalized() AttachmentWrite {
	if w.Role == "" {
		w.Role = AttachmentRoleUnknown
	}
	if w.RoleSource == "" {
		w.RoleSource = AttachmentRoleSourceUnknown
	}
	return w
}

func (w AttachmentWrite) validate() error {
	if !w.Role.valid() {
		return fmt.Errorf("invalid attachment role %q", w.Role)
	}
	if !w.RoleSource.valid() {
		return fmt.Errorf("invalid attachment role source %q", w.RoleSource)
	}
	if w.Size < 0 {
		return errors.New("attachment size must not be negative")
	}
	return nil
}

// UpsertAttachmentRecord stores one attachment occurrence through the typed
// role/provenance contract. A stable source-part key updates the same logical
// occurrence on resync, even when its bytes change. Rows without such a key
// retain the legacy content-hash behavior.
func (s *Store) UpsertAttachmentRecord(
	ctx context.Context,
	messageID int64,
	write AttachmentWrite,
) error {
	write = write.normalized()
	if err := write.validate(); err != nil {
		return err
	}
	if s.syncGeneration != nil {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
				return err
			}
			return s.upsertAttachmentRecord(tx, messageID, write)
		})
	}
	return s.upsertAttachmentRecord(boundQuerier{ctx: ctx, q: s.db}, messageID, write)
}

// UpdateAttachmentMediaMetadataContext records provider media dimensions on
// attachment occurrences belonging to one message.
func (s *Store) UpdateAttachmentMediaMetadataContext(
	ctx context.Context,
	messageID int64,
	contentHash, mediaType string,
	width, height, durationMS sql.NullInt64,
) error {
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		_, err := q.Exec(`
			UPDATE attachments SET media_type = ?, width = ?, height = ?, duration_ms = ?
			WHERE message_id = ? AND (content_hash = ? OR content_hash IS NULL)
		`, mediaType, width, height, durationMS, messageID, contentHash)
		return err
	})
}

// DeleteLegacyHashlessAttachmentsContext removes obsolete unstored rows that
// predate stable source-part keys.
func (s *Store) DeleteLegacyHashlessAttachmentsContext(ctx context.Context, messageID int64) error {
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		_, err := q.Exec(`
			DELETE FROM attachments
			WHERE message_id = ?
			  AND (content_hash IS NULL OR content_hash = '')
			  AND storage_path = ''
		`, messageID)
		return err
	})
}

// DeleteUnstoredAttachmentByHashContext removes an obsolete placeholder for
// one exact content or synthetic hash.
func (s *Store) DeleteUnstoredAttachmentByHashContext(
	ctx context.Context, messageID int64, contentHash string,
) error {
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		_, err := q.Exec(`
			DELETE FROM attachments
			WHERE message_id = ? AND content_hash = ? AND storage_path = ''
		`, messageID, contentHash)
		return err
	})
}

// DeleteUnstoredAttachmentByMetadataContext removes an obsolete hashed
// placeholder selected by its stable display metadata.
func (s *Store) DeleteUnstoredAttachmentByMetadataContext(
	ctx context.Context, messageID int64, filename, mimeType string,
) error {
	return s.withSyncMessageWriteContext(ctx, messageID, func(q querier) error {
		_, err := q.Exec(`
			DELETE FROM attachments
			WHERE message_id = ? AND filename = ? AND mime_type = ?
			  AND storage_path = '' AND content_hash <> '' AND LENGTH(content_hash) = 64
		`, messageID, filename, mimeType)
		return err
	})
}

type attachmentConflictPolicy int

const (
	updateAttachmentConflicts attachmentConflictPolicy = iota
	preserveProviderAttachmentConflicts
)

func (s *Store) upsertAttachmentRecord(q querier, messageID int64, write AttachmentWrite) error {
	return s.upsertAttachmentRecordWithPolicy(q, messageID, write, updateAttachmentConflicts)
}

func (s *Store) upsertAttachmentRecordWithPolicy(
	q querier, messageID int64, write AttachmentWrite, conflictPolicy attachmentConflictPolicy,
) error {
	legacyOwnershipPredicate := ""
	keyedConflictPredicate := ""
	if conflictPolicy == preserveProviderAttachmentConflicts {
		legacyOwnershipPredicate = " AND source_attachment_id IS NULL"
		keyedConflictPredicate = " WHERE attachments.source_attachment_id IS NULL"
	}

	if write.SourcePartKey != "" && write.ContentHash != "" {
		// Upgrade the pre-provenance row in place when a resync supplies a
		// stable source occurrence for the same bytes. This preserves its row
		// identity and prevents a later raw-MIME repair from colliding with a
		// separately inserted keyed row.
		result, err := q.Exec(fmt.Sprintf(`
			UPDATE attachments SET
				filename = ?, mime_type = ?, storage_path = ?, content_hash = ?, size = ?,
				source_attachment_id = ?, media_type = ?, width = ?, height = ?, duration_ms = ?,
				attachment_metadata = %s, attachment_role = ?, role_source = ?,
				source_part_key = ?, content_id = ?,
				attachment_state = ?, attachment_skip_reason = ?
			WHERE message_id = ?
			  AND source_part_key IS NULL
			  AND content_hash = ?
			  AND attachment_role = 'unknown'
			  %s
			  AND NOT EXISTS (
				SELECT 1 FROM attachments keyed
				WHERE keyed.message_id = ? AND keyed.source_part_key = ?
			  )
		`, s.dialect.JSONBindExpr(), legacyOwnershipPredicate),
			write.Filename, write.MIMEType, write.StoragePath, write.ContentHash, write.Size,
			nullIfEmpty(write.SourceAttachmentID), nullIfEmpty(write.MediaType),
			nullIfZero(write.Width), nullIfZero(write.Height), nullIfZero(write.DurationMS),
			nullIfEmpty(write.Metadata), string(write.Role), string(write.RoleSource),
			write.SourcePartKey, nullIfEmpty(write.ContentID),
			nullIfEmpty(string(write.State)), nullIfEmpty(string(write.SkipReason)),
			messageID, write.ContentHash, messageID, write.SourcePartKey,
		)
		if err != nil {
			return err
		}
		if count, countErr := result.RowsAffected(); countErr == nil && count > 0 {
			return nil
		}
	}

	args := []any{
		messageID,
		write.Filename,
		write.MIMEType,
		write.StoragePath,
		write.ContentHash,
		write.Size,
		nullIfEmpty(write.SourceAttachmentID),
		nullIfEmpty(write.MediaType),
		nullIfZero(write.Width),
		nullIfZero(write.Height),
		nullIfZero(write.DurationMS),
		nullIfEmpty(write.Metadata),
		string(write.Role),
		string(write.RoleSource),
		nullIfEmpty(write.SourcePartKey),
		nullIfEmpty(write.ContentID),
		nullIfEmpty(string(write.State)),
		nullIfEmpty(string(write.SkipReason)),
	}
	const columns = `(message_id, filename, mime_type, storage_path, content_hash,
		size, source_attachment_id, media_type, width, height, duration_ms,
		attachment_metadata, attachment_role, role_source, source_part_key,
		content_id, attachment_state, attachment_skip_reason, created_at)`
	values := fmt.Sprintf(`VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, %s, ?, ?, ?, ?, ?, ?, %s)`,
		s.dialect.JSONBindExpr(), s.dialect.Now())

	if write.SourcePartKey != "" {
		result, err := q.Exec(`
			INSERT INTO attachments `+columns+`
			`+values+`
			ON CONFLICT (message_id, source_part_key) WHERE source_part_key IS NOT NULL
			DO UPDATE SET
				filename = EXCLUDED.filename,
				mime_type = EXCLUDED.mime_type,
				storage_path = EXCLUDED.storage_path,
				content_hash = EXCLUDED.content_hash,
				size = EXCLUDED.size,
				source_attachment_id = EXCLUDED.source_attachment_id,
				media_type = EXCLUDED.media_type,
				width = EXCLUDED.width,
				height = EXCLUDED.height,
				duration_ms = EXCLUDED.duration_ms,
				attachment_metadata = EXCLUDED.attachment_metadata,
				attachment_role = EXCLUDED.attachment_role,
				role_source = EXCLUDED.role_source,
				content_id = EXCLUDED.content_id,
				attachment_state = EXCLUDED.attachment_state,
				attachment_skip_reason = EXCLUDED.attachment_skip_reason`+keyedConflictPredicate, args...)
		if err != nil {
			return err
		}
		if conflictPolicy != preserveProviderAttachmentConflicts {
			return nil
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect MIME attachment source-part upsert: %w", err)
		}
		if rowsAffected == 0 {
			return fmt.Errorf(
				"provider-owned attachment source-part collision: message %d part %q",
				messageID, write.SourcePartKey,
			)
		}
		return nil
	}

	if write.ContentHash != "" {
		result, err := q.Exec(`
			INSERT INTO attachments `+columns+`
			`+values+`
			ON CONFLICT (message_id, content_hash)
				WHERE source_part_key IS NULL
				  AND content_hash IS NOT NULL
				  AND content_hash != ''
			DO NOTHING`, args...)
		if err != nil || conflictPolicy != preserveProviderAttachmentConflicts {
			return err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect MIME attachment content-hash upsert: %w", err)
		}
		if rowsAffected > 0 {
			return nil
		}
		var providerID int64
		err = q.QueryRow(`
			SELECT id FROM attachments
			WHERE message_id = ? AND content_hash = ?
			  AND source_part_key IS NULL
			  AND source_attachment_id IS NOT NULL
		`, messageID, write.ContentHash).Scan(&providerID)
		if err == nil {
			return fmt.Errorf(
				"provider-owned attachment content-hash collision: message %d hash %q",
				messageID, write.ContentHash,
			)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("inspect MIME attachment content-hash collision: %w", err)
	}

	var existingID int64
	err := q.QueryRow(`
		SELECT id FROM attachments
		WHERE message_id = ?
		  AND source_part_key IS NULL
		  AND (content_hash IS NULL OR content_hash = '')
	`, messageID).Scan(&existingID)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = q.Exec(`INSERT INTO attachments `+columns+` `+values, args...)
	return err
}

func (s *Store) replaceMIMEAttachmentsWith(
	q querier, messageID int64, replacement *[]AttachmentWrite,
) error {
	if replacement == nil {
		return nil
	}
	if _, err := q.Exec(`
		DELETE FROM attachments
		WHERE message_id = ? AND source_attachment_id IS NULL
	`, messageID); err != nil {
		return err
	}
	for _, attachment := range *replacement {
		attachment = attachment.normalized()
		if err := attachment.validate(); err != nil {
			return err
		}
		if err := s.upsertAttachmentRecordWithPolicy(
			q, messageID, attachment, preserveProviderAttachmentConflicts,
		); err != nil {
			return err
		}
	}
	return nil
}
