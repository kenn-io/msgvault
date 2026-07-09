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

// RecordPackedBlobs inserts a sealed pack's record and its blob index rows in
// one transaction. Idempotent: re-recording an existing pack or blob is a
// no-op, so crash reconciliation can re-run adoption safely.
func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
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
