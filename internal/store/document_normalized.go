package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
)

var ErrDocumentNormalizedIdentityUnavailable = errors.New("document normalized identity is unavailable")

func currentDocumentNormalizationVersion() (int, error) {
	policy, err := document.NewNormalizePolicy(1)
	if err != nil {
		return 0, fmt.Errorf("load current document normalization policy: %w", err)
	}
	return policy.Identity().Version, nil
}

func validateDocumentNormalizedIdentity(version int, family, unitKind string) error {
	currentVersion, err := currentDocumentNormalizationVersion()
	if err != nil {
		return err
	}
	if version != currentVersion || strings.TrimSpace(family) == "" || strings.TrimSpace(unitKind) == "" {
		return fmt.Errorf("%w: require normalization version %d, document family, and unit kind",
			ErrDocumentNormalizedIdentityUnavailable, currentVersion)
	}
	return nil
}

func documentNormalizedIdentityRebuildError(subject string, cause error) error {
	return fmt.Errorf("%s: %w; run `msgvault documents build --full-rebuild --capabilities PATH --yes` before document vector consent or build",
		subject, cause)
}

// LoadNormalizedDocument reconstructs the complete immutable Docbank evidence
// published for one extraction. The stored v3 identities are validated before
// the document can be used to prepare provider inputs.
func (s *Store) LoadNormalizedDocument(ctx context.Context, extractionID string) (document.NormalizedDocument, error) {
	if extractionID == "" {
		return document.NormalizedDocument{}, errors.New("document extraction id is required")
	}
	var normalized document.NormalizedDocument
	var normalizationVersion sql.NullInt64
	var documentFamily, unitKind sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT normalization_version, document_family, unit_kind,
		       manifest_checksum, normalized_truncated
		FROM document_extractions WHERE id = ? AND state = 'ready'`), extractionID).Scan(
		&normalizationVersion, &documentFamily, &unitKind,
		&normalized.Checksum, &normalized.Truncated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return document.NormalizedDocument{}, fmt.Errorf("ready document extraction %q not found", extractionID)
	}
	if err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("read normalized document identity: %w", err)
	}
	if !normalizationVersion.Valid || !documentFamily.Valid || !unitKind.Valid {
		return document.NormalizedDocument{}, documentNormalizedIdentityRebuildError(
			fmt.Sprintf("document extraction %q", extractionID), ErrDocumentNormalizedIdentityUnavailable,
		)
	}
	normalized.PolicyVersion = int(normalizationVersion.Int64)
	normalized.Family = documentFamily.String
	normalized.UnitKind = unitKind.String
	if err := validateDocumentNormalizedIdentity(normalized.PolicyVersion, normalized.Family, normalized.UnitKind); err != nil {
		return document.NormalizedDocument{}, documentNormalizedIdentityRebuildError(
			fmt.Sprintf("document extraction %q", extractionID), err,
		)
	}
	unitRows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT unit_index, unit_kind, text, COALESCE(header_text, ''), COALESCE(footer_text, ''),
		       COALESCE(width, 0), COALESCE(height, 0), COALESCE(dpi, 0), checksum,
		       char_count, truncated, CAST(heading_marks AS TEXT)
		FROM document_units WHERE extraction_id = ? ORDER BY unit_index`), extractionID)
	if err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("read normalized document units: %w", err)
	}
	for unitRows.Next() {
		var unit document.NormalizedUnit
		var headingMarks string
		if err := unitRows.Scan(&unit.Index, &unit.Kind, &unit.Text, &unit.Header, &unit.Footer,
			&unit.Dimensions.Width, &unit.Dimensions.Height, &unit.Dimensions.DPI,
			&unit.Checksum, &unit.CharCount, &unit.Truncated, &headingMarks); err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("scan normalized document unit: %w", err)
		}
		unit.SourceKey = fmt.Sprintf("%s:%06d", normalized.UnitKind, unit.Index)
		if err := json.Unmarshal([]byte(headingMarks), &unit.HeadingMarks); err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("decode normalized document unit headings: %w", err)
		}
		normalized.Units = append(normalized.Units, unit)
	}
	if err := unitRows.Err(); err != nil {
		_ = unitRows.Close()
		return document.NormalizedDocument{}, fmt.Errorf("iterate normalized document units: %w", err)
	}
	if err := unitRows.Close(); err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("close normalized document units: %w", err)
	}

	chunkRows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT chunk_key, ordinal, text, CAST(heading_path AS TEXT), checksum,
		       char_count, truncated
		FROM document_chunks WHERE extraction_id = ? ORDER BY ordinal`), extractionID)
	if err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("read normalized document chunks: %w", err)
	}
	for chunkRows.Next() {
		var chunk document.Chunk
		var headingPath string
		if err := chunkRows.Scan(&chunk.Key, &chunk.Ordinal, &chunk.Text, &headingPath,
			&chunk.Checksum, &chunk.CharCount, &chunk.Truncated); err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("scan normalized document chunk: %w", err)
		}
		if err := json.Unmarshal([]byte(headingPath), &chunk.HeadingPath); err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("decode normalized document chunk headings: %w", err)
		}
		normalized.Chunks = append(normalized.Chunks, chunk)
	}
	if err := chunkRows.Err(); err != nil {
		_ = chunkRows.Close()
		return document.NormalizedDocument{}, fmt.Errorf("iterate normalized document chunks: %w", err)
	}
	if err := chunkRows.Close(); err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("close normalized document chunks: %w", err)
	}
	for index := range normalized.Chunks {
		chunk := &normalized.Chunks[index]
		spanRows, err := s.db.QueryContext(ctx, s.Rebind(`
			SELECT unit_index, start_char, end_char
			FROM document_chunk_spans
			WHERE extraction_id = ? AND chunk_key = ? ORDER BY span_ordinal`), extractionID, chunk.Key)
		if err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("read normalized document chunk spans: %w", err)
		}
		for spanRows.Next() {
			var span document.ChunkSpan
			if err := spanRows.Scan(&span.UnitIndex, &span.CharStart, &span.CharEnd); err != nil {
				_ = spanRows.Close()
				return document.NormalizedDocument{}, fmt.Errorf("scan normalized document chunk span: %w", err)
			}
			chunk.Spans = append(chunk.Spans, span)
		}
		if err := spanRows.Err(); err != nil {
			_ = spanRows.Close()
			return document.NormalizedDocument{}, fmt.Errorf("iterate normalized document chunk spans: %w", err)
		}
		if err := spanRows.Close(); err != nil {
			return document.NormalizedDocument{}, fmt.Errorf("close normalized document chunk spans: %w", err)
		}
	}
	if err := document.ValidateNormalizedDocument(normalized); err != nil {
		return document.NormalizedDocument{}, fmt.Errorf("validate stored normalized document: %w", err)
	}
	return normalized, nil
}
