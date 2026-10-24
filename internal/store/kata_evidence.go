package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/mime"
)

// LoadKataEvidenceSource reads a named representation of one live occurrence.
// It never changes the active extraction or starts processing.
func (s *Store) LoadKataEvidenceSource(ctx context.Context, selector kataevidence.Selector) (kataevidence.SourceRecord, error) {
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return kataevidence.SourceRecord{}, err
	}
	return s.loadKataEvidenceSource(ctx, uid, selector)
}

func (s *Store) loadKataEvidenceSource(ctx context.Context, uid string, selector kataevidence.Selector) (kataevidence.SourceRecord, error) {
	var result kataevidence.SourceRecord
	if selector.Kind == "document_chunk" {
		return result, kataevidence.ErrUnavailable
	}
	result.Reference = kataevidence.Reference{Version: kataevidence.Version, Kind: selector.Kind, ArchiveUID: uid, MessageID: selector.MessageID}
	ref := &result.Reference
	// A message cites its stored body text or the raw MIME fallback.
	if err := s.kataMessageSource(ctx, ref, &result.Display); err != nil {
		return result, err
	}
	switch selector.Kind {
	case "message":
		var plain, html sql.NullString
		err := s.db.QueryRowContext(ctx, s.Rebind("SELECT body_text,body_html FROM message_bodies WHERE message_id=?"), selector.MessageID).Scan(&plain, &html)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("read Kata evidence body: %w", err)
		}
		// HTML-only bodies are quoted as the same plain text embeddings index.
		result.Text = embeddingBodyValue(plain, html)
		// Without a stored body, quote the raw MIME text the reader falls back to.
		if nullStringValue(plain) == "" && nullStringValue(html) == "" {
			if result.Text, err = s.kataRawBody(ctx, selector.MessageID); err != nil {
				return result, err
			}
		}
		digest := sha256.Sum256([]byte(result.Text))
		ref.Message = &kataevidence.MessageReference{BodySHA256: hex.EncodeToString(digest[:])}
	default:
		return result, kataevidence.ErrUnsupported
	}
	return result, nil
}

// KataCitationSource names the sources a message, or one of its attachments,
// would be cited by, without reading bodies or extracted text: the message, or
// the attachment as an extracted file and as each Docbank delivery of it.
// Messages deleted at their source and revoked deliveries stay findable.
func (s *Store) KataCitationSource(ctx context.Context, messageID, attachmentID int64) ([]kataevidence.Reference, error) {
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return nil, err
	}
	ref := kataevidence.Reference{Version: kataevidence.Version, Kind: "message", ArchiveUID: uid, MessageID: messageID}
	var display kataevidence.Display
	if err := s.kataMessageSource(ctx, &ref, &display); err != nil {
		return nil, err
	}
	if attachmentID == 0 {
		return []kataevidence.Reference{ref}, nil
	}
	ref.AttachmentID = attachmentID
	var refs []kataevidence.Reference
	var partKey string
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(source_part_key,'') FROM attachments WHERE id=? AND message_id=?`), attachmentID, messageID).Scan(&partKey)
	if err == nil {
		file := ref
		file.Kind, file.OccurrenceKey = "document_chunk", legacyDocumentOccurrenceKey(messageID, partKey, attachmentID)
		refs = append(refs, file)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read Kata attachment: %w", err)
	}
	// Historical deliveries follow the source tuple regardless of current bytes or eligibility.
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT o.occurrence_ref FROM beeper_media_occurrences o`+beeperMediaTupleJoin+`
		  AND a.id=? AND m.id=? ORDER BY o.occurrence_ref`), attachmentID, messageID)
	if err != nil {
		return nil, fmt.Errorf("read Kata Docbank occurrences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		delivered := ref
		delivered.Kind = "docbank_rendition"
		if err := rows.Scan(&delivered.OccurrenceKey); err != nil {
			return nil, fmt.Errorf("scan Kata Docbank occurrence: %w", err)
		}
		// Each destination keeps its own row for the same occurrence.
		if last := len(refs) - 1; last < 0 || refs[last].OccurrenceKey != delivered.OccurrenceKey {
			refs = append(refs, delivered)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Kata Docbank occurrences: %w", err)
	}
	if len(refs) == 0 {
		return nil, kataevidence.ErrUnavailable
	}
	return refs, nil
}

// legacyDocumentOccurrenceKey preserves the digest stored in existing Kata
// citations. Its encoding must stay unchanged after the document index retires.
func legacyDocumentOccurrenceKey(messageID int64, sourcePartKey string, attachmentID int64) string {
	identity := fmt.Sprintf("unstable\x00%d\x00%d", messageID, attachmentID)
	if sourcePartKey != "" {
		identity = fmt.Sprintf("stable\x00%d\x00%s", messageID, sourcePartKey)
	}
	digest := sha256.Sum256([]byte(identity))
	return "dococc_" + hex.EncodeToString(digest[:])
}

// KataDocbankBinding reads the one retained delivery of an attachment to
// destination, without reconciling delivery state or starting processing.
func (s *Store) KataDocbankBinding(ctx context.Context, destination string, attachmentID int64) (kataevidence.DocbankBinding, error) {
	var result kataevidence.DocbankBinding
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return kataevidence.DocbankBinding{}, err
	}
	var occurred nullableTimestamp
	ref := &result.Reference
	ref.Version, ref.Kind, ref.ArchiveUID, ref.AttachmentID = kataevidence.Version, "docbank_rendition", uid, attachmentID
	// The producer hashes the source tuple; reconciliation revokes its older revisions.
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT o.vault_uid, o.content_version_id, o.source_sha256,
		       o.revision, o.source_id, o.source_version_id, COALESCE(d.supplied_input_id,''),
		       COALESCE(NULLIF(d.profile,''),'supplied-transcript'),
		       m.id, src.source_type, src.identifier, m.source_message_id, o.occurrence_ref,
		       COALESCE(a.filename,''), COALESCE(m.subject,''), COALESCE(m.sent_at,m.received_at,m.internal_date)
		FROM beeper_media_occurrences o
		LEFT JOIN beeper_media_deliveries d ON d.destination_key=o.destination_key AND d.processing_key=o.processing_key
		`+beeperMediaCurrentJoin+`
		  AND o.destination_key=? AND a.id=? AND o.retention_state='retained'
		`), destination, attachmentID).Scan(&result.VaultUID, &result.ContentVersionID, &result.ContentSHA256,
		&result.Revision, &result.SourceID, &result.SourceVersionID, &result.SuppliedInputID, &result.Profile,
		&ref.MessageID, &ref.SourceType, &ref.SourceIdentifier, &ref.SourceMessageID, &ref.OccurrenceKey,
		&result.Display.Filename, &result.Display.ContainingTitle, &occurred)
	if errors.Is(err, sql.ErrNoRows) {
		return kataevidence.DocbankBinding{}, kataevidence.ErrUnavailable
	}
	if err != nil {
		return kataevidence.DocbankBinding{}, fmt.Errorf("read Kata Docbank binding: %w", err)
	}
	if occurred.Valid {
		result.Display.Timestamp = occurred.Time
	}
	if result.VaultUID == "" || result.ContentVersionID == "" || result.Revision == "" || result.SourceID == "" || result.SourceVersionID == "" || result.Profile == "supplied-transcript" && result.SuppliedInputID == "" {
		return kataevidence.DocbankBinding{}, kataevidence.ErrUnavailable
	}
	return result, nil
}

// kataMessageSource fills ref's source tuple and display from its message.
func (s *Store) kataMessageSource(ctx context.Context, ref *kataevidence.Reference, display *kataevidence.Display) error {
	var occurred nullableTimestamp
	var sourceMessageID sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT src.source_type, src.identifier, m.source_message_id,
		       COALESCE(m.subject,''), COALESCE(m.sent_at,m.received_at,m.internal_date)
		FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE m.id=? AND `+LiveMessagesWhere("m", false)), ref.MessageID).Scan(
		&ref.SourceType, &ref.SourceIdentifier, &sourceMessageID, &display.ContainingTitle, &occurred)
	if errors.Is(err, sql.ErrNoRows) {
		return kataevidence.ErrUnavailable
	}
	if err != nil {
		return fmt.Errorf("read Kata evidence occurrence: %w", err)
	}
	// A citation names its source by this ID, so a message without one cannot be cited.
	if !sourceMessageID.Valid || strings.TrimSpace(sourceMessageID.String) == "" {
		return kataevidence.ErrUnsupported
	}
	ref.SourceMessageID = sourceMessageID.String
	if occurred.Valid {
		display.Timestamp = occurred.Time
	}
	return nil
}

// ReadKataEvidenceSource requires both the original occurrence and exact
// representation. Numeric row reuse never authorizes a different source tuple.
// A source whose text changed still returns its display with ErrChanged.
func (s *Store) ReadKataEvidenceSource(ctx context.Context, ref kataevidence.Reference) (kataevidence.SourceRecord, error) {
	if ref.Kind == "document_chunk" {
		return kataevidence.SourceRecord{}, kataevidence.ErrUnavailable
	}
	var empty kataevidence.SourceRecord
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return empty, err
	}
	if uid != ref.ArchiveUID {
		return empty, kataevidence.ErrUnavailable
	}
	selector := kataevidence.Selector{Kind: ref.Kind, MessageID: ref.MessageID, AttachmentID: ref.AttachmentID}
	record, err := s.loadKataEvidenceSource(ctx, uid, selector)
	if err != nil {
		return empty, err
	}
	actual := record.Reference
	if actual.SourceType != ref.SourceType ||
		actual.SourceIdentifier != ref.SourceIdentifier || actual.SourceMessageID != ref.SourceMessageID ||
		actual.OccurrenceKey != ref.OccurrenceKey {
		return empty, kataevidence.ErrUnavailable
	}
	switch ref.Kind {
	case "message":
		if ref.Message == nil || actual.Message == nil {
			return empty, kataevidence.ErrInvalidReference
		}
		if !strings.EqualFold(ref.Message.BodySHA256, actual.Message.BodySHA256) {
			return kataevidence.SourceRecord{Display: record.Display}, kataevidence.ErrChanged
		}
	default:
		return empty, kataevidence.ErrUnsupported
	}
	record.Reference = ref
	return record, nil
}

// kataRawBody reads a message's text from raw MIME, choosing as the reader
// does; a message without raw MIME, or whose MIME does not parse, has none.
func (s *Store) kataRawBody(ctx context.Context, messageID int64) (string, error) {
	raw, err := s.GetMessageRawContext(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", kataevidence.ErrUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("read Kata evidence MIME: %w", err)
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return "", kataevidence.ErrUnavailable
	}
	return parsed.GetBodyText(), nil
}
