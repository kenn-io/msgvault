package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PackIndexEntry mirrors one kit pack.Entry for a packed attachment blob.
// See docs/internal/packed-attachments-design.md.
type PackIndexEntry struct {
	BlobHash  string
	PackID    string
	Offset    int64
	StoredLen int64
	RawLen    int64
	Flags     uint8
	CRC32C    uint32
}

// PackRecord holds a sealed pack's immutable totals, captured at seal or
// crash-reconciliation adoption.
type PackRecord struct {
	PackID      string
	EntryCount  int64
	StoredBytes int64
	CreatedAt   time.Time
}

// RecordPackedBlobs inserts a sealed pack's record and its blob index rows,
// and canonicalizes any noncanonical local storage_path/thumbnail_path
// recording a packed hash to `hash[:2]/hash`, all in one transaction (the
// design's one-transaction rule: the pack is adopted and the DB paths point
// at the canonical blob location atomically). URL-backed paths are never
// rewritten. Idempotent: re-recording an existing pack or blob is a no-op,
// so crash reconciliation can re-run adoption safely. Every entry must
// belong to rec's pack and carry a 64-char blob hash; any violation fails
// the whole call.
func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	for _, e := range entries {
		if e.PackID != rec.PackID {
			return fmt.Errorf("pack index entry %s has pack id %q, want %q",
				e.BlobHash, e.PackID, rec.PackID)
		}
		if len(e.BlobHash) != 64 {
			return fmt.Errorf("pack index entry has malformed blob hash %q", e.BlobHash)
		}
	}
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, ?, ?, ?)`),
			rec.PackID, rec.EntryCount, rec.StoredBytes,
			rec.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("insert attachment_packs row for %s: %w", rec.PackID, err)
		}
		for _, e := range entries {
			if _, err := tx.Exec(s.dialect.InsertOrIgnore(`
				INSERT OR IGNORE INTO attachment_pack_index
				    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
				VALUES (?, ?, ?, ?, ?, ?, ?)`),
				e.BlobHash, e.PackID, e.Offset, e.StoredLen, e.RawLen,
				int64(e.Flags), int64(e.CRC32C)); err != nil {
				return fmt.Errorf("insert pack index row for %s: %w", e.BlobHash, err)
			}
		}
		for _, e := range entries {
			canonical := e.BlobHash[:2] + "/" + e.BlobHash
			if _, err := tx.Exec(`
				UPDATE attachments SET storage_path = ?
				WHERE content_hash = ? AND storage_path != ?
				  AND storage_path IS NOT NULL AND storage_path != ''
				  AND storage_path NOT LIKE 'http://%'
				  AND storage_path NOT LIKE 'https://%'`,
				canonical, e.BlobHash, canonical); err != nil {
				return fmt.Errorf("canonicalize storage_path for %s: %w", e.BlobHash, err)
			}
			if _, err := tx.Exec(`
				UPDATE attachments SET thumbnail_path = ?
				WHERE thumbnail_hash = ? AND thumbnail_path != ?
				  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
				  AND thumbnail_path NOT LIKE 'http://%'
				  AND thumbnail_path NOT LIKE 'https://%'`,
				canonical, e.BlobHash, canonical); err != nil {
				return fmt.Errorf("canonicalize thumbnail_path for %s: %w", e.BlobHash, err)
			}
		}
		return nil
	})
}

// GetAttachmentPackEntry returns the pack location of a blob, or (nil, nil)
// when the blob is not packed (loose or unknown).
func (s *Store) GetAttachmentPackEntry(blobHash string) (*PackIndexEntry, error) {
	var e PackIndexEntry
	var flags, crc int64
	err := s.db.QueryRow(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash = ?`, blobHash).
		Scan(&e.BlobHash, &e.PackID, &e.Offset, &e.StoredLen, &e.RawLen, &flags, &crc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // (nil, nil) signals "not packed"; blobstore.PackIndex callers nil-check the pointer
	}
	if err != nil {
		return nil, fmt.Errorf("get pack index entry for %s: %w", blobHash, err)
	}
	e.Flags = uint8(flags) //nolint:gosec // flags column stores a single byte
	e.CRC32C = uint32(crc) //nolint:gosec // crc32c column stores a uint32
	return &e, nil
}

// UnpackedBlob is one distinct local blob that has no pack index row.
// Path is the DB-recorded path relative to the attachments dir,
// slash-separated; Size is -1 when unknown (thumbnail-only blobs).
type UnpackedBlob struct {
	Hash string
	Path string
	Size int64
}

// ListUnpackedBlobs returns every distinct local (non-URL) content and
// thumbnail blob that has no attachment_pack_index row, with its
// DB-recorded relative path. Content blobs come first, then blobs seen
// only as thumbnails (Size -1); a hash appearing as both is listed once,
// as a content blob.
func (s *Store) ListUnpackedBlobs() ([]UnpackedBlob, error) {
	var blobs []UnpackedBlob
	seen := make(map[string]struct{})

	collect := func(query string, scanSize bool) error {
		rows, err := s.db.Query(query)
		if err != nil {
			return fmt.Errorf("list unpacked blobs: %w", err)
		}
		defer rows.Close() //nolint:errcheck // read-only cursor
		for rows.Next() {
			var b UnpackedBlob
			if scanSize {
				err = rows.Scan(&b.Hash, &b.Path, &b.Size)
			} else {
				b.Size = -1
				err = rows.Scan(&b.Hash, &b.Path)
			}
			if err != nil {
				return fmt.Errorf("scan unpacked blob: %w", err)
			}
			if _, dup := seen[b.Hash]; dup {
				continue
			}
			seen[b.Hash] = struct{}{}
			blobs = append(blobs, b)
		}
		return rows.Err()
	}

	if err := collect(`
		SELECT content_hash, MIN(storage_path), COALESCE(MAX(size), -1)
		FROM attachments
		WHERE content_hash IS NOT NULL AND content_hash != ''
		  AND storage_path IS NOT NULL AND storage_path != ''
		  AND storage_path NOT LIKE 'http://%'
		  AND storage_path NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = attachments.content_hash)
		GROUP BY content_hash
		ORDER BY MIN(id)`, true); err != nil {
		return nil, err
	}
	if err := collect(`
		SELECT thumbnail_hash, MIN(thumbnail_path)
		FROM attachments
		WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
		  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
		  AND thumbnail_path NOT LIKE 'http://%'
		  AND thumbnail_path NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = attachments.thumbnail_hash)
		GROUP BY thumbnail_hash
		ORDER BY MIN(id)`, false); err != nil {
		return nil, err
	}
	return blobs, nil
}

// ListIndexedBlobHashes returns the set of all packed blob hashes.
func (s *Store) ListIndexedBlobHashes() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT blob_hash FROM attachment_pack_index`)
	if err != nil {
		return nil, fmt.Errorf("list indexed blob hashes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	hashes := make(map[string]struct{})
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan indexed blob hash: %w", err)
		}
		hashes[h] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list indexed blob hashes: %w", err)
	}
	return hashes, nil
}

// ListPackRecords returns all attachment pack records ordered by pack_id.
func (s *Store) ListPackRecords() ([]PackRecord, error) {
	rows, err := s.db.Query(`
		SELECT pack_id, entry_count, stored_bytes, created_at
		FROM attachment_packs ORDER BY pack_id`)
	if err != nil {
		return nil, fmt.Errorf("list pack records: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var recs []PackRecord
	for rows.Next() {
		var r PackRecord
		var createdAt string
		if err := rows.Scan(&r.PackID, &r.EntryCount, &r.StoredBytes, &createdAt); err != nil {
			return nil, fmt.Errorf("scan pack record: %w", err)
		}
		r.CreatedAt, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at for pack %s: %w", r.PackID, err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pack records: %w", err)
	}
	return recs, nil
}

// HasPackRecord reports whether the pack has an attachment_packs row.
func (s *Store) HasPackRecord(packID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`
		SELECT 1 FROM attachment_packs WHERE pack_id = ?`, packID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check pack record %s: %w", packID, err)
	}
	return true, nil
}

// CountPackIndexEntries returns the number of live index rows in a pack.
func (s *Store) CountPackIndexEntries(packID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM attachment_pack_index WHERE pack_id = ?`, packID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pack index entries for %s: %w", packID, err)
	}
	return n, nil
}

// DeletePackRecord removes a pack's index rows and its attachment_packs
// row in one transaction (used by unpack).
func (s *Store) DeletePackRecord(packID string) error {
	return s.withTx(func(tx *loggedTx) error {
		if _, err := tx.Exec(`
			DELETE FROM attachment_pack_index WHERE pack_id = ?`, packID); err != nil {
			return fmt.Errorf("delete pack index rows for %s: %w", packID, err)
		}
		if _, err := tx.Exec(`
			DELETE FROM attachment_packs WHERE pack_id = ?`, packID); err != nil {
			return fmt.Errorf("delete pack record %s: %w", packID, err)
		}
		return nil
	})
}
