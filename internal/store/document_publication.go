package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

var (
	ErrDocumentExtractionClaimed   = errors.New("document extraction owner is already claimed")
	ErrDocumentExtractionCurrent   = errors.New("document extraction owner already has a current head")
	ErrDocumentExtractionFenceLost = errors.New("document extraction claim fence is no longer current")
)

type DocumentExtractionClaimInput struct {
	ExtractionID           string
	ProfileID              string
	RebuildID              string
	CanonicalBlobHash      string
	ExtractionInputKey     string
	OccurrenceAttachmentID int64
	OccurrenceMIMEType     string
	OccurrenceMessageType  string
	LeaseOwner             string
	LeaseUntil             time.Time
	LocalBytes             int64
	SourceSequence         int64
	RequireNoHead          bool
}

type DocumentExtractionClaim struct {
	DocumentExtractionClaimInput

	LeaseFence int64
}

type DocumentPublishedUnit struct {
	Index        int
	Kind         string
	Text         string
	Header       string
	Footer       string
	Width        int
	Height       int
	DPI          int
	Checksum     string
	CharCount    int
	Truncated    bool
	HeadingMarks []document.HeadingMark
}

type DocumentPublishedSpan struct {
	UnitIndex int
	CharStart int
	CharEnd   int
	Synthetic bool
}

type DocumentPublishedChunk struct {
	Key                string
	Ordinal            int
	Text               string
	HeadingPath        []string
	FirstUnitIndex     int
	LastUnitIndex      int
	SyntheticPrefixLen int
	Checksum           string
	CharCount          int
	TableChunk         bool
	CodeChunk          bool
	Truncated          bool
	Spans              []DocumentPublishedSpan
}

type DocumentExtractionPublication struct {
	ExtractionID           string
	ProfileID              string
	CanonicalBlobHash      string
	ExtractionInputKey     string
	LeaseOwner             string
	LeaseFence             int64
	OccurrenceAttachmentID int64
	OccurrenceMIMEType     string
	OccurrenceMessageType  string
	ReturnedModel          string
	ProviderBytes          *int64
	UnitsProcessed         int
	RequestCount           int
	RetryCount             int
	ProviderLatencyMS      int64
	ManifestChecksum       string
	NormalizationVersion   int
	DocumentFamily         string
	UnitKind               string
	NormalizedTruncated    bool
	Units                  []DocumentPublishedUnit
	Chunks                 []DocumentPublishedChunk
}

// ClaimDocumentExtraction creates an immutable staging revision and acquires
// the one owner-level lease. A later claimant can replace only an expired
// lease and receives a larger monotonic fence.
func (s *Store) ClaimDocumentExtraction(
	ctx context.Context,
	input DocumentExtractionClaimInput,
) (DocumentExtractionClaim, error) {
	if err := validateDocumentClaimInput(input); err != nil {
		return DocumentExtractionClaim{}, err
	}
	claim := DocumentExtractionClaim{DocumentExtractionClaimInput: input}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var eligible bool
		if err := q.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM document_extraction_profiles p
				JOIN document_provider_consents c ON c.profile_id = p.id
				WHERE p.id = ? AND p.enabled = TRUE AND p.retired_at IS NULL
				  AND c.profile_fingerprint = p.fingerprint
				  AND c.retention_posture = p.retention_posture
				  AND c.training_posture = p.training_posture
			)`, input.ProfileID).Scan(&eligible); err != nil {
			return fmt.Errorf("check document extraction profile authority: %w", err)
		}
		if !eligible {
			return errors.New("document extraction profile is not enabled with exact consent")
		}
		if err := q.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM document_occurrences o
				JOIN attachments a ON a.id = o.attachment_id
				JOIN messages m ON m.id = o.message_id
				WHERE o.attachment_id = ? AND o.canonical_blob_hash = ?
				  AND o.attachment_role = 'standalone'
				  AND `+authoritativeDocumentRoleSourceSQL("o")+`
				  AND a.attachment_role = 'standalone'
				  AND `+authoritativeDocumentRoleSourceSQL("a")+`
				  AND (COALESCE(a.content_hash, '') = ? OR
				       (COALESCE(a.content_hash, '') = '' AND a.storage_path = ?))
				  AND COALESCE(o.mime_type, '') = ?
				  AND COALESCE(a.mime_type, '') = ?
				  AND COALESCE(m.message_type, '') = ?
				  AND `+LiveMessagesWhere("m", true)+`
			)`, input.OccurrenceAttachmentID, input.CanonicalBlobHash,
			input.CanonicalBlobHash, canonicalCASPath(input.CanonicalBlobHash), input.OccurrenceMIMEType,
			input.OccurrenceMIMEType, input.OccurrenceMessageType).Scan(&eligible); err != nil {
			return fmt.Errorf("check document extraction occurrence: %w", err)
		}
		if !eligible {
			return errors.New("document extraction owner has no eligible occurrence")
		}
		if input.RebuildID != "" {
			if err := q.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM document_extraction_rebuilds r
					JOIN document_extraction_rebuild_targets t ON t.rebuild_id = r.id
					WHERE r.id = ? AND r.profile_id = ? AND r.extraction_input_key = ?
					  AND r.state = 'building' AND t.canonical_blob_hash = ?
				)`, input.RebuildID, input.ProfileID, input.ExtractionInputKey,
				input.CanonicalBlobHash).Scan(&eligible); err != nil {
				return fmt.Errorf("check document extraction rebuild target: %w", err)
			}
			if !eligible {
				return errors.New("document extraction owner is not in the active rebuild")
			}
		}
		if input.RequireNoHead {
			if err := q.QueryRow(`
				SELECT NOT EXISTS (
					SELECT 1 FROM document_extraction_heads
					WHERE profile_id = ? AND canonical_blob_hash = ?
					  AND extraction_input_key = ?
				)`, input.ProfileID, input.CanonicalBlobHash, input.ExtractionInputKey).Scan(&eligible); err != nil {
				return fmt.Errorf("check current document extraction head: %w", err)
			}
			if !eligible {
				return ErrDocumentExtractionCurrent
			}
		}
		if _, err := q.Exec(`
			INSERT INTO document_extractions
				(id, profile_id, rebuild_id, canonical_blob_hash, extraction_input_key,
				 state, lease_owner, lease_until, local_bytes, source_sequence)
			VALUES (?, ?, ?, ?, ?, 'staging', ?, ?, ?, ?)`,
			input.ExtractionID, input.ProfileID, nullIfEmpty(input.RebuildID), input.CanonicalBlobHash,
			input.ExtractionInputKey, input.LeaseOwner, input.LeaseUntil,
			input.LocalBytes, input.SourceSequence,
		); err != nil {
			return fmt.Errorf("create staging document extraction: %w", err)
		}
		claimSQL := `
			INSERT INTO document_extraction_claims
				(profile_id, canonical_blob_hash, extraction_input_key,
				 extraction_id, lease_owner, lease_fence, lease_until)
			VALUES (?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT (profile_id, canonical_blob_hash, extraction_input_key)
			DO UPDATE SET
				extraction_id = EXCLUDED.extraction_id,
				lease_owner = EXCLUDED.lease_owner,
				lease_fence = document_extraction_claims.lease_fence + 1,
				lease_until = EXCLUDED.lease_until,
				updated_at = ` + s.dialect.Now() + `
			WHERE document_extraction_claims.lease_until <= ` + s.dialect.Now() + `
			RETURNING lease_fence`
		if err := q.QueryRow(claimSQL,
			input.ProfileID, input.CanonicalBlobHash, input.ExtractionInputKey,
			input.ExtractionID, input.LeaseOwner, input.LeaseUntil,
		).Scan(&claim.LeaseFence); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDocumentExtractionClaimed
			}
			return fmt.Errorf("claim document extraction owner: %w", err)
		}
		if _, err := q.Exec(`
			UPDATE document_extractions
			SET next_retry_at = NULL, updated_at = `+s.dialect.Now()+`
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND state = 'tombstoned'
			  AND next_retry_at IS NOT NULL AND id != ?`,
			input.ProfileID, input.CanonicalBlobHash, input.ExtractionInputKey,
			input.ExtractionID,
		); err != nil {
			return fmt.Errorf("consume document extraction retry markers: %w", err)
		}
		if _, err := q.Exec(`
			UPDATE document_extractions
			SET state = 'tombstoned', updated_at = `+s.dialect.Now()+`
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND state = 'staging' AND id != ?`,
			input.ProfileID, input.CanonicalBlobHash, input.ExtractionInputKey,
			input.ExtractionID,
		); err != nil {
			return fmt.Errorf("tombstone superseded document extraction: %w", err)
		}
		if _, err := q.Exec(`
			UPDATE document_extractions SET lease_fence = ? WHERE id = ?`,
			claim.LeaseFence, input.ExtractionID,
		); err != nil {
			return fmt.Errorf("record document extraction fence: %w", err)
		}
		return nil
	})
	return claim, err
}

type DocumentExtractionFailure struct {
	Claim             DocumentExtractionClaim
	ReasonCode        string
	Terminal          bool
	RetryAt           time.Time
	RequestCount      int
	RetryCount        int
	ProviderLatencyMS int64
}

// FailDocumentExtraction records only a bounded reason code, releases the
// exact fenced claim, and either suppresses the owner for this immutable
// profile or schedules a later retry. Provider bodies and extracted content
// are never stored on failure.
func (s *Store) FailDocumentExtraction(ctx context.Context, failure DocumentExtractionFailure) error {
	if err := validateDocumentFailure(failure); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		state := "tombstoned"
		var terminalReason any
		var nextRetry any = failure.RetryAt
		var hadServingHead bool
		if failure.Terminal {
			state = "terminal"
			terminalReason = failure.ReasonCode
			nextRetry = nil
			if err := q.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM document_extraction_heads
					WHERE canonical_blob_hash = ? AND extraction_input_key = ?
				)`, failure.Claim.CanonicalBlobHash, failure.Claim.ExtractionInputKey,
			).Scan(&hadServingHead); err != nil {
				return fmt.Errorf("check document heads before terminal suppression: %w", err)
			}
		}
		result, err := q.Exec(`
			UPDATE document_extractions
			SET state = ?, attempt_count = attempt_count + 1,
			    request_count = ?, retry_count = ?, provider_latency_ms = ?,
			    next_retry_at = ?, terminal_reason = ?, lease_owner = NULL,
			    lease_until = NULL, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND state = 'staging'
			  AND lease_owner = ? AND lease_fence = ?`,
			state, failure.RequestCount, failure.RetryCount, failure.ProviderLatencyMS,
			nextRetry, terminalReason, failure.Claim.ExtractionID,
			failure.Claim.ProfileID, failure.Claim.CanonicalBlobHash,
			failure.Claim.ExtractionInputKey, failure.Claim.LeaseOwner,
			failure.Claim.LeaseFence,
		)
		if err != nil {
			return fmt.Errorf("record document extraction failure: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document extraction failure result: %w", err)
		}
		if updated != 1 {
			return ErrDocumentExtractionFenceLost
		}
		result, err = q.Exec(`
			DELETE FROM document_extraction_claims
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND extraction_id = ?
			  AND lease_owner = ? AND lease_fence = ?`,
			failure.Claim.ProfileID, failure.Claim.CanonicalBlobHash,
			failure.Claim.ExtractionInputKey, failure.Claim.ExtractionID,
			failure.Claim.LeaseOwner, failure.Claim.LeaseFence,
		)
		if err != nil {
			return fmt.Errorf("release failed document extraction claim: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil || deleted != 1 {
			return ErrDocumentExtractionFenceLost
		}
		if failure.Terminal {
			result, err = q.Exec(`
				DELETE FROM document_extraction_heads
				WHERE profile_id = ? AND canonical_blob_hash = ?
				  AND extraction_input_key = ? AND extraction_id = ?`,
				failure.Claim.ProfileID, failure.Claim.CanonicalBlobHash,
				failure.Claim.ExtractionInputKey, failure.Claim.ExtractionID,
			)
			if err != nil {
				return fmt.Errorf("suppress terminal document extraction head: %w", err)
			}
			if _, rowsErr := result.RowsAffected(); rowsErr != nil {
				return fmt.Errorf("read terminal document head suppression result: %w", rowsErr)
			}
			if hadServingHead {
				return bumpDocumentIndexRevision(q)
			}
		}
		return nil
	})
}

func (s *Store) RenewDocumentExtractionClaim(
	ctx context.Context,
	claim DocumentExtractionClaim,
	leaseUntil time.Time,
) error {
	if leaseUntil.IsZero() || !leaseUntil.After(time.Now().UTC()) ||
		leaseUntil.After(time.Now().UTC().Add(time.Hour)) {
		return errors.New("document extraction renewal has invalid lease deadline")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		query := `
			UPDATE document_extraction_claims
			SET lease_until = ?, updated_at = ` + s.dialect.Now() + `
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND extraction_id = ?
			  AND lease_owner = ? AND lease_fence = ?
			  AND lease_until > ` + s.dialect.Now() + `
			RETURNING lease_fence`
		var fence int64
		if err := q.QueryRow(query, leaseUntil, claim.ProfileID, claim.CanonicalBlobHash,
			claim.ExtractionInputKey, claim.ExtractionID, claim.LeaseOwner,
			claim.LeaseFence).Scan(&fence); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDocumentExtractionFenceLost
			}
			return fmt.Errorf("renew document extraction claim: %w", err)
		}
		if _, err := q.Exec(`UPDATE document_extractions SET lease_until = ? WHERE id = ?`,
			leaseUntil, claim.ExtractionID); err != nil {
			return fmt.Errorf("record renewed document extraction lease: %w", err)
		}
		return nil
	})
}

// PublishDocumentExtraction atomically writes canonical derivatives, marks
// the staging revision ready, switches the owner head, and advances the search
// revision. Provider work must already be complete before this call.
func (s *Store) PublishDocumentExtraction(
	ctx context.Context,
	publication DocumentExtractionPublication,
) error {
	if err := validateDocumentPublication(publication); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		claimQuery := `
			SELECT EXISTS (
				SELECT 1 FROM document_extraction_claims
				WHERE profile_id = ? AND canonical_blob_hash = ?
				  AND extraction_input_key = ? AND extraction_id = ?
				  AND lease_owner = ? AND lease_fence = ?
				  AND lease_until > ` + s.dialect.Now() + `
			)`
		var current bool
		if err := q.QueryRow(claimQuery,
			publication.ProfileID, publication.CanonicalBlobHash,
			publication.ExtractionInputKey, publication.ExtractionID,
			publication.LeaseOwner, publication.LeaseFence,
		).Scan(&current); err != nil {
			return fmt.Errorf("check document extraction publication fence: %w", err)
		}
		if !current {
			return ErrDocumentExtractionFenceLost
		}
		var sourceSequence int64
		if err := q.QueryRow(`
			SELECT COALESCE(MAX(o.source_sequence), -1)
			FROM document_occurrences o
			JOIN attachments a ON a.id = o.attachment_id
			JOIN messages m ON m.id = o.message_id
			WHERE o.attachment_id = ? AND o.canonical_blob_hash = ?
			  AND o.attachment_role = 'standalone'
			  AND `+authoritativeDocumentRoleSourceSQL("o")+`
			  AND a.attachment_role = 'standalone'
			  AND `+authoritativeDocumentRoleSourceSQL("a")+`
			  AND (COALESCE(a.content_hash, '') = ? OR
			       (COALESCE(a.content_hash, '') = '' AND a.storage_path = ?))
			  AND COALESCE(o.mime_type, '') = ?
			  AND COALESCE(a.mime_type, '') = ?
			  AND COALESCE(m.message_type, '') = ?
			  AND `+LiveMessagesWhere("m", true),
			publication.OccurrenceAttachmentID, publication.CanonicalBlobHash,
			publication.CanonicalBlobHash, canonicalCASPath(publication.CanonicalBlobHash), publication.OccurrenceMIMEType,
			publication.OccurrenceMIMEType, publication.OccurrenceMessageType,
		).Scan(&sourceSequence); err != nil {
			return fmt.Errorf("recheck document extraction occurrences: %w", err)
		}
		if sourceSequence < 0 {
			return errors.New("document extraction claimed occurrence is no longer eligible")
		}
		if _, err := q.Exec(`DELETE FROM document_units WHERE extraction_id = ?`, publication.ExtractionID); err != nil {
			return fmt.Errorf("clear staged document units: %w", err)
		}
		for _, unit := range publication.Units {
			headingMarks, err := json.Marshal(unit.HeadingMarks)
			if err != nil {
				return fmt.Errorf("encode document unit heading marks: %w", err)
			}
			if _, err := q.Exec(`
				INSERT INTO document_units
					(extraction_id, unit_index, unit_kind, text, header_text,
					 footer_text, width, height, dpi, checksum, char_count, truncated, heading_marks)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`)`,
				publication.ExtractionID, unit.Index, unit.Kind, unit.Text,
				nullIfEmpty(unit.Header), nullIfEmpty(unit.Footer), nullIfZero(int64(unit.Width)),
				nullIfZero(int64(unit.Height)), nullIfZero(int64(unit.DPI)), unit.Checksum,
				unit.CharCount, unit.Truncated, string(headingMarks),
			); err != nil {
				return fmt.Errorf("publish document unit %d: %w", unit.Index, err)
			}
		}
		for _, chunk := range publication.Chunks {
			headingPath, err := json.Marshal(chunk.HeadingPath)
			if err != nil {
				return fmt.Errorf("encode document chunk heading path: %w", err)
			}
			if _, err := q.Exec(`
				INSERT INTO document_chunks
					(extraction_id, chunk_key, ordinal, text, heading_path,
					 first_unit_index, last_unit_index, synthetic_prefix_len,
					 checksum, char_count, table_chunk, code_chunk, truncated)
				VALUES (?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?, ?, ?, ?, ?)`,
				publication.ExtractionID, chunk.Key, chunk.Ordinal, chunk.Text,
				string(headingPath), chunk.FirstUnitIndex, chunk.LastUnitIndex,
				chunk.SyntheticPrefixLen, chunk.Checksum, chunk.CharCount,
				chunk.TableChunk, chunk.CodeChunk, chunk.Truncated,
			); err != nil {
				return fmt.Errorf("publish document chunk %d: %w", chunk.Ordinal, err)
			}
			for spanOrdinal, span := range chunk.Spans {
				if _, err := q.Exec(`
					INSERT INTO document_chunk_spans
						(extraction_id, chunk_key, span_ordinal, unit_index,
						 start_char, end_char, synthetic)
					VALUES (?, ?, ?, ?, ?, ?, ?)`,
					publication.ExtractionID, chunk.Key, spanOrdinal, span.UnitIndex,
					span.CharStart, span.CharEnd, span.Synthetic,
				); err != nil {
					return fmt.Errorf("publish document chunk %d span %d: %w", chunk.Ordinal, spanOrdinal, err)
				}
			}
		}
		providerBytes := any(nil)
		if publication.ProviderBytes != nil {
			providerBytes = *publication.ProviderBytes
		}
		result, err := q.Exec(`
			UPDATE document_extractions
			SET state = 'ready', lease_owner = NULL, lease_until = NULL,
				provider_bytes = ?, units_processed = ?, returned_model = ?,
				request_count = ?, retry_count = ?, provider_latency_ms = ?,
				manifest_checksum = ?, normalization_version = ?, document_family = ?,
				unit_kind = ?, normalized_truncated = ?, source_sequence = ?,
				updated_at = `+s.dialect.Now()+`, published_at = `+s.dialect.Now()+`
			WHERE id = ? AND profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND state = 'staging'`,
			providerBytes, publication.UnitsProcessed, publication.ReturnedModel,
			publication.RequestCount, publication.RetryCount, publication.ProviderLatencyMS,
			publication.ManifestChecksum, publication.NormalizationVersion,
			publication.DocumentFamily, publication.UnitKind, publication.NormalizedTruncated,
			sourceSequence, publication.ExtractionID,
			publication.ProfileID, publication.CanonicalBlobHash,
			publication.ExtractionInputKey,
		)
		if err != nil {
			return fmt.Errorf("mark document extraction ready: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read document extraction publication result: %w", err)
		}
		if updated != 1 {
			return errors.New("document extraction staging revision is no longer publishable")
		}
		if _, err := q.Exec(`
			INSERT INTO document_extraction_heads
				(profile_id, canonical_blob_hash, extraction_input_key,
				 extraction_id, source_sequence)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (profile_id, canonical_blob_hash, extraction_input_key)
			DO UPDATE SET extraction_id = EXCLUDED.extraction_id,
				source_sequence = EXCLUDED.source_sequence,
				switched_at = `+s.dialect.Now(),
			publication.ProfileID, publication.CanonicalBlobHash,
			publication.ExtractionInputKey, publication.ExtractionID, sourceSequence,
		); err != nil {
			return fmt.Errorf("switch document extraction head: %w", err)
		}
		result, err = q.Exec(`
			DELETE FROM document_extraction_claims
			WHERE profile_id = ? AND canonical_blob_hash = ?
			  AND extraction_input_key = ? AND extraction_id = ?
			  AND lease_owner = ? AND lease_fence = ?`,
			publication.ProfileID, publication.CanonicalBlobHash,
			publication.ExtractionInputKey, publication.ExtractionID,
			publication.LeaseOwner, publication.LeaseFence,
		)
		if err != nil {
			return fmt.Errorf("release document extraction claim: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil || deleted != 1 {
			return ErrDocumentExtractionFenceLost
		}
		return bumpDocumentIndexRevision(q)
	})
}

func canonicalCASPath(contentHash string) string {
	return contentHash[:2] + "/" + contentHash
}

func validateDocumentClaimInput(input DocumentExtractionClaimInput) error {
	now := time.Now().UTC()
	if input.ExtractionID == "" || input.ProfileID == "" ||
		!validLowerSHA256(input.CanonicalBlobHash) || input.ExtractionInputKey == "" ||
		input.OccurrenceAttachmentID <= 0 || input.OccurrenceMIMEType == "" ||
		input.LeaseOwner == "" || input.LocalBytes <= 0 || input.SourceSequence < 0 ||
		!input.LeaseUntil.After(now) || input.LeaseUntil.After(now.Add(time.Hour)) {
		return errors.New("document extraction claim is incomplete or outside safety bounds")
	}
	return nil
}

func validateDocumentPublication(publication DocumentExtractionPublication) error {
	if publication.ExtractionID == "" || publication.ProfileID == "" ||
		!validLowerSHA256(publication.CanonicalBlobHash) || publication.ExtractionInputKey == "" ||
		publication.OccurrenceAttachmentID <= 0 || publication.OccurrenceMIMEType == "" ||
		publication.LeaseOwner == "" || publication.LeaseFence <= 0 ||
		publication.ReturnedModel == "" || publication.UnitsProcessed <= 0 ||
		!validLowerSHA256(publication.ManifestChecksum) || len(publication.Units) == 0 || len(publication.Chunks) == 0 {
		return errors.New("document extraction publication is incomplete")
	}
	if publication.ProviderBytes != nil && *publication.ProviderBytes < 0 {
		return errors.New("document extraction publication has invalid provider bytes")
	}
	if publication.RequestCount < 0 || publication.RetryCount < 0 ||
		publication.RetryCount > publication.RequestCount || publication.ProviderLatencyMS < 0 {
		return errors.New("document extraction publication has invalid provider request accounting")
	}
	if err := validateDocumentNormalizedIdentity(
		publication.NormalizationVersion, publication.DocumentFamily, publication.UnitKind,
	); err != nil {
		return fmt.Errorf("document extraction publication has invalid normalized identity: %w", err)
	}
	for index, unit := range publication.Units {
		if unit.Index != index || unit.Kind == "" || !utf8.ValidString(unit.Text) ||
			unit.CharCount != utf8.RuneCountInString(unit.Text) || !validLowerSHA256(unit.Checksum) ||
			unit.Width < 0 || unit.Height < 0 || unit.DPI < 0 {
			return fmt.Errorf("document extraction publication unit %d is invalid", index)
		}
	}
	for ordinal, chunk := range publication.Chunks {
		if chunk.Ordinal != ordinal || chunk.Key == "" || !utf8.ValidString(chunk.Text) ||
			chunk.CharCount != utf8.RuneCountInString(chunk.Text) || !validLowerSHA256(chunk.Checksum) ||
			chunk.FirstUnitIndex < 0 || chunk.LastUnitIndex < chunk.FirstUnitIndex ||
			chunk.LastUnitIndex >= len(publication.Units) || chunk.SyntheticPrefixLen < 0 ||
			chunk.SyntheticPrefixLen > chunk.CharCount || len(chunk.Spans) == 0 {
			return fmt.Errorf("document extraction publication chunk %d is invalid", ordinal)
		}
		for spanOrdinal, span := range chunk.Spans {
			if span.UnitIndex < chunk.FirstUnitIndex || span.UnitIndex > chunk.LastUnitIndex ||
				span.CharStart < 0 || span.CharEnd < span.CharStart ||
				span.CharEnd > publication.Units[span.UnitIndex].CharCount {
				return fmt.Errorf("document extraction publication chunk %d span %d is invalid", ordinal, spanOrdinal)
			}
		}
	}
	return nil
}

func validateDocumentFailure(failure DocumentExtractionFailure) error {
	claim := failure.Claim
	if claim.ExtractionID == "" || claim.ProfileID == "" ||
		!validLowerSHA256(claim.CanonicalBlobHash) || claim.ExtractionInputKey == "" ||
		claim.LeaseOwner == "" || claim.LeaseFence <= 0 ||
		failure.ReasonCode == "" || len(failure.ReasonCode) > 64 ||
		failure.RequestCount < 0 || failure.RetryCount < 0 ||
		failure.RetryCount > failure.RequestCount || failure.ProviderLatencyMS < 0 {
		return errors.New("document extraction failure is incomplete")
	}
	for _, character := range failure.ReasonCode {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') {
			return errors.New("document extraction failure reason code is invalid")
		}
	}
	if failure.Terminal {
		if !failure.RetryAt.IsZero() {
			return errors.New("terminal document extraction failure cannot retry")
		}
	} else if !failure.RetryAt.After(time.Now().UTC()) || failure.RetryAt.After(time.Now().UTC().Add(7*24*time.Hour)) {
		return errors.New("retryable document extraction failure has invalid retry deadline")
	}
	return nil
}
