package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"
)

// DocumentExtractionRange is unpublished text from one successful PDF selection.
// Evidence contains Docbank's canonical normalized evidence, never a provider body.
type DocumentExtractionRange struct {
	FirstPage        int
	LastPage         int
	TotalPages       int
	Evidence         []byte
	EvidenceChecksum string
	Markdown         string
	CharCount        int
}

var ErrDocumentRangeBytes = errors.New("document range checkpoints exceed max_response_bytes")

// LoadDocumentExtractionRanges bounds stored bytes; the worker validates
// canonical evidence and its checksum before use.
func (s *Store) LoadDocumentExtractionRanges(ctx context.Context, claim DocumentExtractionClaim, maxBytes int64) ([]DocumentExtractionRange, error) {
	if maxBytes <= 0 {
		return nil, errors.New("invalid document range byte bound")
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT first_page,last_page,total_pages,evidence,evidence_checksum,markdown,char_count FROM document_extraction_ranges WHERE extraction_id = ? ORDER BY first_page`), claim.ExtractionID)
	if err != nil {
		return nil, fmt.Errorf("load document ranges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ranges []DocumentExtractionRange
	var size int64
	for rows.Next() {
		var r DocumentExtractionRange
		if err := rows.Scan(&r.FirstPage, &r.LastPage, &r.TotalPages, &r.Evidence, &r.EvidenceChecksum, &r.Markdown, &r.CharCount); err != nil {
			return nil, err
		}
		size += int64(len(r.Evidence) + len(r.Markdown))
		if size > maxBytes {
			return nil, ErrDocumentRangeBytes
		}
		ranges = append(ranges, r)
	}
	return ranges, rows.Err()
}

func (s *Store) SaveDocumentExtractionRange(ctx context.Context, claim DocumentExtractionClaim, r DocumentExtractionRange, maxBytes int64) error {
	digest := sha256.Sum256(r.Evidence)
	if r.FirstPage < 1 || r.LastPage < r.FirstPage || r.TotalPages < r.LastPage || len(r.Evidence) == 0 || hex.EncodeToString(digest[:]) != r.EvidenceChecksum || !utf8.ValidString(r.Markdown) || r.CharCount < utf8.RuneCountInString(r.Markdown) || maxBytes <= 0 {
		return errors.New("invalid document range checkpoint")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		// Lock the exact live claim while writing, so a replacement cannot race the fence.
		result, err := q.Exec(`UPDATE document_extraction_claims SET updated_at = `+s.dialect.Now()+` WHERE profile_id = ? AND canonical_blob_hash = ? AND extraction_input_key = ? AND extraction_id = ? AND lease_owner = ? AND lease_fence = ? AND lease_until > `+s.dialect.Now(), claim.ProfileID, claim.CanonicalBlobHash, claim.ExtractionInputKey, claim.ExtractionID, claim.LeaseOwner, claim.LeaseFence)
		if err != nil {
			return fmt.Errorf("fence document range checkpoint: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrDocumentExtractionFenceLost
		}
		var size int64
		if err := q.QueryRow(`SELECT COALESCE(SUM(payload_bytes),0) FROM document_extraction_ranges WHERE extraction_id = ? AND first_page != ?`, claim.ExtractionID, r.FirstPage).Scan(&size); err != nil {
			return err
		}
		payloadBytes := int64(len(r.Evidence) + len(r.Markdown))
		if size+payloadBytes > maxBytes {
			return ErrDocumentRangeBytes
		}
		_, err = q.Exec(`INSERT INTO document_extraction_ranges (extraction_id,first_page,last_page,total_pages,evidence,evidence_checksum,markdown,char_count,payload_bytes) VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (extraction_id,first_page) DO NOTHING`, claim.ExtractionID, r.FirstPage, r.LastPage, r.TotalPages, string(r.Evidence), r.EvidenceChecksum, r.Markdown, r.CharCount, payloadBytes)
		return err
	})
}
