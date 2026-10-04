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
	result.Reference = kataevidence.Reference{Version: kataevidence.Version, Kind: selector.Kind, ArchiveUID: uid, MessageID: selector.MessageID}
	ref := &result.Reference
	var occurred nullableTimestamp
	var sourceMessageID sql.NullString
	// A file cites only what document search can find, which leaves out
	// messages deleted at their source. A message cites its stored body text,
	// or the raw MIME text the reader falls back to without one.
	live := LiveMessagesWhere("m", selector.Kind == "document_chunk")
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT src.source_type, src.identifier, m.source_message_id,
		       COALESCE(m.subject,''), COALESCE(m.sent_at,m.received_at,m.internal_date)
		FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE m.id=? AND `+live), selector.MessageID).Scan(
		&ref.SourceType, &ref.SourceIdentifier, &sourceMessageID, &result.Display.ContainingTitle, &occurred)
	if errors.Is(err, sql.ErrNoRows) {
		return result, kataevidence.ErrUnavailable
	}
	if err != nil {
		return result, fmt.Errorf("read Kata evidence occurrence: %w", err)
	}
	// A citation names its source by this ID, so a message without one cannot be cited.
	if !sourceMessageID.Valid || strings.TrimSpace(sourceMessageID.String) == "" {
		return result, kataevidence.ErrUnsupported
	}
	ref.SourceMessageID = sourceMessageID.String
	if occurred.Valid {
		result.Display.Timestamp = occurred.Time
	}
	switch selector.Kind {
	case "message":
		var plain, html sql.NullString
		err = s.db.QueryRowContext(ctx, s.Rebind("SELECT body_text,body_html FROM message_bodies WHERE message_id=?"), selector.MessageID).Scan(&plain, &html)
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
	case "document_chunk":
		ref.AttachmentID = selector.AttachmentID
		var hash string
		err = s.db.QueryRowContext(ctx, s.Rebind(`
			SELECT o.occurrence_key,o.canonical_blob_hash,COALESCE(a.filename,'')
			FROM document_occurrences o JOIN attachments a ON a.id=o.attachment_id
			WHERE o.attachment_id=? AND o.message_id=? AND a.message_id=o.message_id
			  AND (a.content_hash=o.canonical_blob_hash OR
			       (COALESCE(a.content_hash,'')='' AND a.storage_path=SUBSTR(o.canonical_blob_hash,1,2)||'/'||o.canonical_blob_hash))
			  AND a.attachment_role=o.attachment_role
		`), selector.AttachmentID, selector.MessageID).Scan(&ref.OccurrenceKey, &hash, &result.Display.Filename)
		if errors.Is(err, sql.ErrNoRows) {
			return result, kataevidence.ErrUnavailable
		}
		if err != nil {
			return result, fmt.Errorf("read Kata document occurrence: %w", err)
		}
		// Read only the cited chunk, from its own extraction, so a citation keeps
		// working after the attachment is reprocessed.
		var manifest, chunkText, chunkChecksum sql.NullString
		var version sql.NullInt64
		err = s.db.QueryRowContext(ctx, s.Rebind(`
			SELECT e.manifest_checksum, e.normalization_version, c.text, c.checksum
			FROM document_extractions e
			LEFT JOIN document_chunks c ON c.extraction_id=e.id AND c.chunk_key=?
			WHERE e.id=? AND e.canonical_blob_hash=? AND e.state='ready'
		`), selector.ChunkKey, selector.ExtractionID, hash).Scan(&manifest, &version, &chunkText, &chunkChecksum)
		if errors.Is(err, sql.ErrNoRows) {
			return result, kataevidence.ErrUnavailable
		}
		if err != nil {
			return result, fmt.Errorf("read Kata document chunk: %w", err)
		}
		if !version.Valid || !manifest.Valid || manifest.String == "" {
			return result, kataevidence.ErrUnprocessed
		}
		if !chunkText.Valid || !chunkChecksum.Valid || chunkChecksum.String == "" {
			return result, kataevidence.ErrUnavailable
		}
		ref.DocumentChunk = &kataevidence.DocumentReference{
			CanonicalBlobHash: hash, ExtractionID: selector.ExtractionID, ManifestChecksum: manifest.String,
			ChunkKey: selector.ChunkKey, ChunkChecksum: chunkChecksum.String,
		}
		result.Text = chunkText.String
		return result, nil
	default:
		return result, kataevidence.ErrUnsupported
	}
	return result, nil
}

// ReadKataEvidenceSource requires both the original occurrence and exact
// representation. Numeric row reuse never authorizes a different source tuple.
func (s *Store) ReadKataEvidenceSource(ctx context.Context, ref kataevidence.Reference) (kataevidence.SourceRecord, error) {
	var empty kataevidence.SourceRecord
	uid, err := s.ArchiveUIDContext(ctx)
	if err != nil {
		return empty, err
	}
	if uid != ref.ArchiveUID {
		return empty, kataevidence.ErrUnavailable
	}
	selector := kataevidence.Selector{Kind: ref.Kind, MessageID: ref.MessageID, AttachmentID: ref.AttachmentID}
	if ref.DocumentChunk != nil {
		selector.ExtractionID, selector.ChunkKey = ref.DocumentChunk.ExtractionID, ref.DocumentChunk.ChunkKey
	}
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
			return empty, kataevidence.ErrChanged
		}
	case "document_chunk":
		if ref.DocumentChunk == nil || actual.DocumentChunk == nil {
			return empty, kataevidence.ErrInvalidReference
		}
		want, got := ref.DocumentChunk, actual.DocumentChunk
		if !strings.EqualFold(want.CanonicalBlobHash, got.CanonicalBlobHash) ||
			!strings.EqualFold(want.ManifestChecksum, got.ManifestChecksum) || !strings.EqualFold(want.ChunkChecksum, got.ChunkChecksum) {
			return empty, kataevidence.ErrChanged
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
