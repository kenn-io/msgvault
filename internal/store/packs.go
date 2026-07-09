package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kit/pack"
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
// so crash reconciliation can re-run adoption safely. The record must carry a
// canonical pack ID, and every entry must belong to that pack and carry a
// 64-char blob hash; any violation fails the whole call.
func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	return s.recordPackedBlobs(rec, entries, false)
}

// AdoptPackedBlobs records a reconciled orphan pack and transactionally
// repoints the supplied blob index entries to it. The caller must submit only
// entries that were absent from the index or whose previously indexed packed
// copy failed verification. Repointing instead of deleting stale rows before
// adoption avoids a crash window with no readable packed index.
func (s *Store) AdoptPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	return s.recordPackedBlobs(rec, entries, true)
}

func (s *Store) recordPackedBlobs(rec PackRecord, entries []PackIndexEntry, replaceExisting bool) error {
	if !pack.IsValidPackID(rec.PackID) {
		return fmt.Errorf("attachment pack record has malformed pack id %q", rec.PackID)
	}
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
			if replaceExisting {
				if _, err := tx.Exec(`
					DELETE FROM attachment_pack_index WHERE blob_hash = ?`, e.BlobHash); err != nil {
					return fmt.Errorf("replace pack index row for %s: %w", e.BlobHash, err)
				}
			}
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

// ListAttachmentPackEntries returns the live blob index rows owned by one
// pack, ordered by their position in the pack. Footer entries without a live
// index row are dead and must not be served or restored by unpack.
func (s *Store) ListAttachmentPackEntries(packID string) ([]PackIndexEntry, error) {
	rows, err := s.db.Query(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index
		WHERE pack_id = ?
		ORDER BY pack_offset, blob_hash`, packID)
	if err != nil {
		return nil, fmt.Errorf("list pack index entries for %s: %w", packID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var entries []PackIndexEntry
	for rows.Next() {
		var e PackIndexEntry
		var flags, crc int64
		if err := rows.Scan(&e.BlobHash, &e.PackID, &e.Offset, &e.StoredLen,
			&e.RawLen, &flags, &crc); err != nil {
			return nil, fmt.Errorf("scan pack index entry for %s: %w", packID, err)
		}
		e.Flags = uint8(flags) //nolint:gosec // flags column stores a single byte
		e.CRC32C = uint32(crc) //nolint:gosec // crc32c column stores a uint32
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pack index entries for %s: %w", packID, err)
	}
	return entries, nil
}

// UnpackedBlob is one distinct local blob that has no pack index row.
// Paths contains every distinct DB-recorded local candidate path relative to
// the attachments dir, slash-separated. Size is -1 when unknown
// (thumbnail-only blobs).
type UnpackedBlob struct {
	Hash  string
	Paths []string
	Size  int64
}

// ListUnpackedBlobs returns every distinct local (non-URL) content and
// thumbnail blob that has no attachment_pack_index row, preserving all of its
// DB-recorded relative candidate paths. Content blobs come first, then blobs
// seen only as thumbnails (Size -1); a hash appearing as both is listed once
// with content and thumbnail paths combined.
func (s *Store) ListUnpackedBlobs() ([]UnpackedBlob, error) {
	var blobs []UnpackedBlob
	byHash := make(map[string]int)
	seenPaths := make(map[string]map[string]struct{})

	collect := func(query string, scanSize bool) error {
		rows, err := s.db.Query(query)
		if err != nil {
			return fmt.Errorf("list unpacked blobs: %w", err)
		}
		defer rows.Close() //nolint:errcheck // read-only cursor
		for rows.Next() {
			var hash, path string
			var size int64
			if scanSize {
				err = rows.Scan(&hash, &path, &size)
			} else {
				size = -1
				err = rows.Scan(&hash, &path)
			}
			if err != nil {
				return fmt.Errorf("scan unpacked blob: %w", err)
			}
			if _, ok := seenPaths[hash]; !ok {
				seenPaths[hash] = make(map[string]struct{})
			}
			if _, dup := seenPaths[hash][path]; dup {
				continue
			}
			seenPaths[hash][path] = struct{}{}
			if idx, ok := byHash[hash]; ok {
				blobs[idx].Paths = append(blobs[idx].Paths, path)
				if scanSize && size > blobs[idx].Size {
					blobs[idx].Size = size
				}
				continue
			}
			byHash[hash] = len(blobs)
			blobs = append(blobs, UnpackedBlob{Hash: hash, Paths: []string{path}, Size: size})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate unpacked blobs: %w", err)
		}
		return nil
	}

	if err := collect(`
		SELECT content_hash, storage_path, COALESCE(MAX(size), -1)
		FROM attachments
		WHERE content_hash IS NOT NULL AND content_hash != ''
		  AND storage_path IS NOT NULL AND storage_path != ''
		  AND storage_path NOT LIKE 'http://%'
		  AND storage_path NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = attachments.content_hash)
		GROUP BY content_hash, storage_path
		ORDER BY MIN(id), storage_path`, true); err != nil {
		return nil, err
	}
	if err := collect(`
		SELECT thumbnail_hash, thumbnail_path
		FROM attachments
		WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
		  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
		  AND thumbnail_path NOT LIKE 'http://%'
		  AND thumbnail_path NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = attachments.thumbnail_hash)
		GROUP BY thumbnail_hash, thumbnail_path
		ORDER BY MIN(id), thumbnail_path`, false); err != nil {
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

// ClearAttachmentPackMetadata deletes every attachment_pack_index and
// attachment_packs row in one transaction. Missing tables are a no-op so
// restoring a snapshot from before packed storage does not have to initialize
// or migrate the rest of the database merely to perform this cleanup.
//
// It is used after `backup restore`, which materializes loose canonical
// attachment files only — never production pack files — so any pack metadata
// carried in the restored database points at packs that do not exist and must
// be dropped before the vault is used.
func (s *Store) ClearAttachmentPackMetadata() error {
	indexExists, err := s.tableExists("attachment_pack_index")
	if err != nil {
		return fmt.Errorf("check attachment_pack_index table: %w", err)
	}
	packsExists, err := s.tableExists("attachment_packs")
	if err != nil {
		return fmt.Errorf("check attachment_packs table: %w", err)
	}
	if !indexExists && !packsExists {
		return nil
	}
	return s.withTx(func(tx *loggedTx) error {
		if indexExists {
			if _, err := tx.Exec(`DELETE FROM attachment_pack_index`); err != nil {
				return fmt.Errorf("clear attachment_pack_index: %w", err)
			}
		}
		if packsExists {
			if _, err := tx.Exec(`DELETE FROM attachment_packs`); err != nil {
				return fmt.Errorf("clear attachment_packs: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) tableExists(name string) (bool, error) {
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`
	if s.dialect.DriverName() == "pgx" {
		query = `SELECT COUNT(*) FROM information_schema.tables
		         WHERE table_schema = current_schema() AND table_name = ?`
	}
	var count int
	if err := s.db.QueryRow(query, name).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
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

// DeletePackIndexEntry removes one unreadable packed-blob mapping while
// retaining the pack record and all other live entries. The packer calls this
// only after a loose copy has been hash-verified and materialized canonically;
// the old pack entry then becomes dead bytes for GC/repack accounting.
func (s *Store) DeletePackIndexEntry(blobHash string) error {
	if _, err := s.db.Exec(`
		DELETE FROM attachment_pack_index WHERE blob_hash = ?`, blobHash); err != nil {
		return fmt.Errorf("delete pack index entry for %s: %w", blobHash, err)
	}
	return nil
}
