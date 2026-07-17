package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

// PackIndexAdoption couples one canonical orphan-pack entry with every
// original attachment hash spelling that made the entry live. Each original
// hash must normalize exactly to Entry.BlobHash.
type PackIndexAdoption struct {
	Entry          PackIndexEntry
	OriginalHashes []string
}

func scanPackIndexEntry(row scanner) (PackIndexEntry, error) {
	var entry PackIndexEntry
	var flags, crc int64
	if err := row.Scan(&entry.BlobHash, &entry.PackID, &entry.Offset,
		&entry.StoredLen, &entry.RawLen, &flags, &crc); err != nil {
		return PackIndexEntry{}, err
	}
	return decodePackIndexEntry(entry, flags, crc)
}

func decodePackIndexEntry(entry PackIndexEntry, flags, crc int64) (PackIndexEntry, error) {
	if err := validatePackIndexEntry(entry, flags, crc); err != nil {
		return PackIndexEntry{}, err
	}
	entry.Flags = uint8(flags) //nolint:gosec // validatePackIndexEntry proved uint8 range
	entry.CRC32C = uint32(crc) //nolint:gosec // validatePackIndexEntry proved uint32 range
	return entry, nil
}

func validatePackIndexEntry(entry PackIndexEntry, flags, crc int64) error {
	if err := validateCanonicalBlobHash(entry.BlobHash); err != nil {
		return err
	}
	if !pack.IsValidPackID(entry.PackID) {
		return fmt.Errorf("malformed pack id %q", entry.PackID)
	}
	if entry.Offset < 0 {
		return fmt.Errorf("pack index entry %s has negative offset %d",
			entry.BlobHash, entry.Offset)
	}
	if entry.StoredLen < 0 {
		return fmt.Errorf("pack index entry %s has negative stored length %d",
			entry.BlobHash, entry.StoredLen)
	}
	if entry.RawLen < 0 || entry.RawLen > int64(pack.MaxRawLen) {
		return fmt.Errorf(
			"pack index entry %s has raw length %d outside 0..%d",
			entry.BlobHash, entry.RawLen, uint64(pack.MaxRawLen))
	}
	if flags < 0 || flags > int64(^uint8(0)) {
		return fmt.Errorf("pack index entry %s has flags %d outside uint8 range",
			entry.BlobHash, flags)
	}
	if crc < 0 || crc > int64(^uint32(0)) {
		return fmt.Errorf("pack index entry %s has crc32c %d outside uint32 range",
			entry.BlobHash, crc)
	}
	return nil
}

func scanPackIndexEntries(
	rows *loggedRows,
	description string,
	expectedPackID string,
) ([]PackIndexEntry, error) {
	var entries []PackIndexEntry
	for rows.Next() {
		entry, err := scanPackIndexEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", description, err)
		}
		if expectedPackID != "" && entry.PackID != expectedPackID {
			return nil, fmt.Errorf("scan %s: entry %s belongs to pack %s",
				description, entry.BlobHash, entry.PackID)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", description, err)
	}
	return entries, nil
}

func normalizeBlobHash(blobHash string) (string, error) {
	normalized := strings.ToLower(blobHash)
	if len(normalized) != 64 {
		return "", fmt.Errorf("malformed blob hash %q: must be exactly 64 hex characters", blobHash)
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", fmt.Errorf("malformed blob hash %q: contains non-hexadecimal characters", blobHash)
	}
	return normalized, nil
}

func validateCanonicalBlobHash(blobHash string) error {
	normalized, err := normalizeBlobHash(blobHash)
	if err != nil {
		return err
	}
	if normalized != blobHash {
		return fmt.Errorf("malformed blob hash %q: must use canonical lowercase hex", blobHash)
	}
	return nil
}

// AttachmentBlobLocation is the production read resolution for one content
// hash. Referenced is authoritative: callers must not serve either Pack or a
// loose fallback when it is false. Pack is nil for a referenced loose blob.
type AttachmentBlobLocation struct {
	Referenced bool
	Pack       *PackIndexEntry
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
// canonical lowercase SHA-256 blob hash; any violation fails the whole call.
func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	return s.recordPackedBlobs(rec, entries, false, nil)
}

// RecordPackedBlobsWithAliases inserts a newly sealed pack while
// transactionally canonicalizing every local attachment hash spelling that
// produced each entry. Unlike orphan adoption, existing index rows are not
// replaced: ordinary packing must not overwrite a concurrently published
// mapping.
func (s *Store) RecordPackedBlobsWithAliases(rec PackRecord, packed []PackIndexAdoption) error {
	entries := make([]PackIndexEntry, len(packed))
	originalHashes := make([][]string, len(packed))
	for i, blob := range packed {
		entries[i] = blob.Entry
		originalHashes[i] = blob.OriginalHashes
	}
	return s.recordPackedBlobs(rec, entries, false, originalHashes)
}

// AdoptPackedBlobs records a reconciled orphan pack and transactionally
// repoints the supplied blob index entries to it. The caller must submit only
// entries that were absent from the index or whose previously indexed packed
// copy failed verification. Repointing instead of deleting stale rows before
// adoption avoids a crash window with no readable packed index.
func (s *Store) AdoptPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	return s.recordPackedBlobs(rec, entries, true, nil)
}

// AdoptPackedBlobsWithAliases records a reconciled orphan pack and repoints
// each canonical index entry while transactionally canonicalizing the local
// attachment rows that reference it through the supplied original spellings.
// URL-backed and empty paths remain unchanged; their case-equivalent hash
// spelling may be exchanged with a local alias so the local hash and path stay
// canonical. Validation of any entry or alias fails the entire call before
// the transaction begins.
func (s *Store) AdoptPackedBlobsWithAliases(rec PackRecord, adoptions []PackIndexAdoption) error {
	entries := make([]PackIndexEntry, len(adoptions))
	originalHashes := make([][]string, len(adoptions))
	for i, adoption := range adoptions {
		entries[i] = adoption.Entry
		originalHashes[i] = adoption.OriginalHashes
	}
	return s.recordPackedBlobs(rec, entries, true, originalHashes)
}

func (s *Store) recordPackedBlobs(
	rec PackRecord,
	entries []PackIndexEntry,
	replaceExisting bool,
	originalHashes [][]string,
) error {
	if !pack.IsValidPackID(rec.PackID) {
		return fmt.Errorf("attachment pack record has malformed pack id %q", rec.PackID)
	}
	if rec.EntryCount < 0 || rec.StoredBytes < 0 {
		return fmt.Errorf("attachment pack record %s has invalid totals: entries=%d stored_bytes=%d",
			rec.PackID, rec.EntryCount, rec.StoredBytes)
	}
	if int64(len(entries)) > rec.EntryCount {
		return fmt.Errorf("attachment pack record %s has %d submitted entries, exceeding total %d",
			rec.PackID, len(entries), rec.EntryCount)
	}
	var submittedStoredBytes int64
	if originalHashes != nil && len(originalHashes) != len(entries) {
		return fmt.Errorf("attachment pack record %s has %d entries but %d alias sets",
			rec.PackID, len(entries), len(originalHashes))
	}
	aliasesByEntry := make([][]string, len(entries))
	for i, e := range entries {
		if e.PackID != rec.PackID {
			return fmt.Errorf("pack index entry %s has pack id %q, want %q",
				e.BlobHash, e.PackID, rec.PackID)
		}
		if err := validatePackIndexEntry(e, int64(e.Flags), int64(e.CRC32C)); err != nil {
			return fmt.Errorf("pack index entry: %w", err)
		}
		if e.StoredLen > rec.StoredBytes-submittedStoredBytes {
			return fmt.Errorf(
				"attachment pack record %s has submitted stored bytes exceeding total %d",
				rec.PackID, rec.StoredBytes)
		}
		submittedStoredBytes += e.StoredLen
		aliases := []string{e.BlobHash}
		if originalHashes != nil {
			aliases = originalHashes[i]
			if len(aliases) == 0 {
				return fmt.Errorf("pack index entry %s has no original hash aliases", e.BlobHash)
			}
		}
		for _, alias := range aliases {
			normalized, err := normalizeBlobHash(alias)
			if err != nil {
				return fmt.Errorf("pack index entry %s alias: %w", e.BlobHash, err)
			}
			if normalized != e.BlobHash {
				return fmt.Errorf("pack index entry %s alias %q normalizes to %s",
					e.BlobHash, alias, normalized)
			}
		}
		aliasesByEntry[i] = aliases
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
		for i, e := range entries {
			for _, alias := range aliasesByEntry[i] {
				if err := canonicalizeAttachmentBlobPathsTx(tx, e.BlobHash, alias); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// CanonicalizeAttachmentBlobPaths transactionally rewrites every nonempty
// local content and thumbnail path for blobHash to its content-addressed path.
// URL-backed and empty paths are left unchanged.
func (s *Store) CanonicalizeAttachmentBlobPaths(blobHash string) error {
	return s.CanonicalizeAttachmentBlobAliases(blobHash, []string{blobHash})
}

// CanonicalizeAttachmentBlobAliases transactionally rewrites every nonempty
// local path whose hash is one of originalHashes to its content-addressed
// path and the canonical lowercase hash. Every alias must normalize to
// blobHash. URL-backed and empty paths remain unchanged; when one owns the
// canonical per-message unique key, its hash spelling is exchanged with the
// local row's alias so loose reads remain consistent with the local path.
func (s *Store) CanonicalizeAttachmentBlobAliases(blobHash string, originalHashes []string) error {
	normalized, err := normalizeBlobHash(blobHash)
	if err != nil {
		return err
	}
	if len(originalHashes) == 0 {
		return fmt.Errorf("canonicalize attachment blob %s: no original hash aliases", normalized)
	}
	for _, original := range originalHashes {
		alias, err := normalizeBlobHash(original)
		if err != nil {
			return fmt.Errorf("canonicalize attachment blob %s alias: %w", normalized, err)
		}
		if alias != normalized {
			return fmt.Errorf("canonicalize attachment blob %s alias %q normalizes to %s",
				normalized, original, alias)
		}
	}
	return s.withTx(func(tx *loggedTx) error {
		for _, original := range originalHashes {
			if err := canonicalizeAttachmentBlobPathsTx(tx, normalized, original); err != nil {
				return err
			}
		}
		return nil
	})
}

func canonicalizeAttachmentBlobPathsTx(tx *loggedTx, blobHash, lookupHash string) error {
	canonical := blobHash[:2] + "/" + blobHash
	// The unique attachment key is case-sensitive, so legacy local rows for one
	// message can contain case-equivalent hashes. Collapse those logical local
	// duplicates before lowercasing; otherwise the update below collides with
	// idx_attachments_msg_content_hash and wedges every later maintenance run.
	// Retaining MIN(id) matches the one-shot legacy duplicate migration. URL and
	// empty-path rows are outside this repair because this API preserves them.
	if _, err := tx.Exec(`
		DELETE FROM attachments
		WHERE LOWER(content_hash) = ?
		  AND storage_path IS NOT NULL AND storage_path != ''
		  AND LOWER(storage_path) NOT LIKE 'http://%'
		  AND LOWER(storage_path) NOT LIKE 'https://%'
		  AND id NOT IN (
			SELECT MIN(id) FROM attachments
			WHERE LOWER(content_hash) = ?
			  AND storage_path IS NOT NULL AND storage_path != ''
			  AND LOWER(storage_path) NOT LIKE 'http://%'
			  AND LOWER(storage_path) NOT LIKE 'https://%'
			GROUP BY message_id
		  )`, blobHash, blobHash); err != nil {
		return fmt.Errorf("deduplicate case-equivalent attachment rows for %s: %w", blobHash, err)
	}
	if err := swapPreservedCanonicalHashOwnersTx(tx, blobHash, lookupHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE attachments SET storage_path = ?, content_hash = ?
		WHERE (content_hash = ? OR content_hash = ?)
		  AND (storage_path != ? OR content_hash != ?)
		  AND storage_path IS NOT NULL AND storage_path != ''
		  AND LOWER(storage_path) NOT LIKE 'http://%'
		  AND LOWER(storage_path) NOT LIKE 'https://%'`,
		canonical, blobHash, blobHash, lookupHash, canonical, blobHash); err != nil {
		return fmt.Errorf("canonicalize storage_path for %s: %w", blobHash, err)
	}
	if _, err := tx.Exec(`
		UPDATE attachments SET thumbnail_path = ?, thumbnail_hash = ?
		WHERE (thumbnail_hash = ? OR thumbnail_hash = ?)
		  AND (thumbnail_path != ? OR thumbnail_hash != ?)
		  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
		  AND LOWER(thumbnail_path) NOT LIKE 'http://%'
		  AND LOWER(thumbnail_path) NOT LIKE 'https://%'`,
		canonical, blobHash, blobHash, lookupHash, canonical, blobHash); err != nil {
		return fmt.Errorf("canonicalize thumbnail_path for %s: %w", blobHash, err)
	}
	return nil
}

type attachmentHashOwnerSwap struct {
	localID   int64
	localHash string
	ownerID   int64
}

// swapPreservedCanonicalHashOwnersTx frees the lowercase per-message key for
// each local row without changing a URL/empty row's path. A temporary NULL,
// excluded from the partial unique index, prevents immediate unique-index
// checks from rejecting the two-row hash exchange.
func swapPreservedCanonicalHashOwnersTx(tx *loggedTx, blobHash, lookupHash string) error {
	rows, err := tx.Query(`
		SELECT local.id, local.content_hash, canonical_owner.id
		FROM attachments AS local
		JOIN attachments AS canonical_owner
		  ON canonical_owner.message_id = local.message_id
		 AND canonical_owner.id != local.id
		 AND canonical_owner.content_hash = ?
		WHERE (local.content_hash = ? OR local.content_hash = ?)
		  AND local.content_hash != ?
		  AND local.storage_path IS NOT NULL AND local.storage_path != ''
		  AND LOWER(local.storage_path) NOT LIKE 'http://%'
		  AND LOWER(local.storage_path) NOT LIKE 'https://%'
		  AND (canonical_owner.storage_path IS NULL
		       OR canonical_owner.storage_path = ''
		       OR LOWER(canonical_owner.storage_path) LIKE 'http://%'
		       OR LOWER(canonical_owner.storage_path) LIKE 'https://%')`,
		blobHash, blobHash, lookupHash, blobHash)
	if err != nil {
		return fmt.Errorf("find preserved canonical hash owners for %s: %w", blobHash, err)
	}
	var swaps []attachmentHashOwnerSwap
	for rows.Next() {
		var swap attachmentHashOwnerSwap
		if err := rows.Scan(&swap.localID, &swap.localHash, &swap.ownerID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan preserved canonical hash owner for %s: %w", blobHash, err)
		}
		swaps = append(swaps, swap)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate preserved canonical hash owners for %s: %w", blobHash, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close preserved canonical hash owners for %s: %w", blobHash, err)
	}

	for _, swap := range swaps {
		if _, err := tx.Exec(`
			UPDATE attachments SET content_hash = NULL
			WHERE id = ? AND content_hash = ?`, swap.ownerID, blobHash); err != nil {
			return fmt.Errorf("temporarily release canonical attachment hash %s: %w", blobHash, err)
		}
		if _, err := tx.Exec(`
			UPDATE attachments SET content_hash = ?
			WHERE id = ? AND content_hash = ?`, blobHash, swap.localID, swap.localHash); err != nil {
			return fmt.Errorf("assign canonical attachment hash %s to local row: %w", blobHash, err)
		}
		if _, err := tx.Exec(`
			UPDATE attachments SET content_hash = ?
			WHERE id = ? AND content_hash IS NULL`, swap.localHash, swap.ownerID); err != nil {
			return fmt.Errorf("preserve nonlocal attachment hash alias for %s: %w", blobHash, err)
		}
	}
	return nil
}

// GetAttachmentPackEntry returns the pack location of a blob, or (nil, nil)
// when the blob is not packed (loose or unknown).
func (s *Store) GetAttachmentPackEntry(blobHash string) (*PackIndexEntry, error) {
	entry, err := scanPackIndexEntry(s.db.QueryRow(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash = ?`, blobHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // (nil, nil) signals "not packed"; packed-storage resolvers nil-check the pointer
	}
	if err != nil {
		return nil, fmt.Errorf("get pack index entry for %s: %w", blobHash, err)
	}
	return &entry, nil
}

const attachmentReferencedHashesSQL = `
	SELECT LOWER(content_hash) FROM attachments
	WHERE content_hash IS NOT NULL AND content_hash != ''
	UNION
	SELECT LOWER(thumbnail_hash) FROM attachments
	WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''`

const resolveAttachmentBlobSQL = `
	WITH requested(blob_hash) AS (VALUES (CAST(? AS TEXT)))
	SELECT CASE WHEN (
	           EXISTS (SELECT 1 FROM attachments a
	                   WHERE LOWER(a.content_hash) = ?)
	           OR EXISTS (SELECT 1 FROM attachments a
	                      WHERE LOWER(a.thumbnail_hash) = ?)
	       ) THEN 1 ELSE 0 END,
	       p.blob_hash, p.pack_id, p.pack_offset,
	       p.stored_len, p.raw_len, p.flags, p.crc32c
	FROM requested
	LEFT JOIN attachment_pack_index p ON p.blob_hash = requested.blob_hash`

// ResolveAttachmentBlob determines attachment liveness and the optional pack
// location in one query. Attachment rows, rather than storage metadata, are
// the liveness authority, so stale unreferenced index rows are never exposed
// to the production read path.
func (s *Store) ResolveAttachmentBlob(blobHash string) (AttachmentBlobLocation, error) {
	canonicalHash, err := normalizeBlobHash(blobHash)
	if err != nil {
		return AttachmentBlobLocation{}, err
	}
	if canonicalHash != blobHash {
		var legacyIndexRows int
		err = s.db.QueryRow(s.dialect.Rebind(`
			SELECT COUNT(*) FROM attachment_pack_index WHERE blob_hash = ?`), blobHash).
			Scan(&legacyIndexRows)
		if err != nil {
			return AttachmentBlobLocation{}, fmt.Errorf(
				"check noncanonical pack index key %s: %w", blobHash, err)
		}
		if legacyIndexRows != 0 {
			return AttachmentBlobLocation{}, fmt.Errorf(
				"resolve attachment blob %s: malformed pack index key must use canonical lowercase hex",
				blobHash)
		}
	}
	var referenced int
	var hash, packID sql.NullString
	var offset, storedLen, rawLen, flags, crc sql.NullInt64
	err = s.db.QueryRow(s.dialect.Rebind(resolveAttachmentBlobSQL),
		canonicalHash, canonicalHash, canonicalHash).
		Scan(&referenced, &hash, &packID, &offset, &storedLen, &rawLen, &flags, &crc)
	if err != nil {
		return AttachmentBlobLocation{}, fmt.Errorf("resolve attachment blob %s: %w", blobHash, err)
	}
	loc := AttachmentBlobLocation{Referenced: referenced != 0}
	if !loc.Referenced || !hash.Valid {
		return loc, nil
	}
	if !packID.Valid || !offset.Valid || !storedLen.Valid || !rawLen.Valid ||
		!flags.Valid || !crc.Valid {
		return AttachmentBlobLocation{}, fmt.Errorf(
			"resolve attachment blob %s: incomplete pack index metadata", blobHash)
	}
	entry, err := decodePackIndexEntry(PackIndexEntry{
		BlobHash:  hash.String,
		PackID:    packID.String,
		Offset:    offset.Int64,
		StoredLen: storedLen.Int64,
		RawLen:    rawLen.Int64,
	}, flags.Int64, crc.Int64)
	if err != nil {
		return AttachmentBlobLocation{}, fmt.Errorf("resolve attachment blob %s: %w", blobHash, err)
	}
	loc.Pack = &entry
	return loc, nil
}

// ListReferencedBlobHashes returns every non-empty content or thumbnail hash
// named by an attachment row. A hash shared across columns appears once.
func (s *Store) ListReferencedBlobHashes() (map[string]struct{}, error) {
	rows, err := s.db.Query(`
		SELECT content_hash FROM attachments
		WHERE content_hash IS NOT NULL AND content_hash != ''
		UNION
		SELECT thumbnail_hash FROM attachments
		WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''`)
	if err != nil {
		return nil, fmt.Errorf("list referenced attachment blob hashes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	hashes := make(map[string]struct{})
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scan referenced attachment blob hash: %w", err)
		}
		hashes[hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate referenced attachment blob hashes: %w", err)
	}
	return hashes, nil
}

// PruneUnreferencedPackIndex removes stale storage mappings whose hash no
// longer appears in either attachment hash column. References are compared
// case-insensitively because legacy rows may contain uppercase SHA-256 text;
// pruning must not destroy their otherwise-readable canonical mapping before
// alias-aware maintenance can normalize the row.
func (s *Store) PruneUnreferencedPackIndex(ctx context.Context) (int64, error) {
	var pruned int64
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		res, err := tx.ExecContext(ctx, pruneUnreferencedPackIndexSQL)
		if err != nil {
			return fmt.Errorf("prune unreferenced pack index: %w", err)
		}
		pruned, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("count pruned pack index rows: %w", err)
		}
		return nil
	})
	return pruned, err
}

const pruneUnreferencedPackIndexSQL = `
	DELETE FROM attachment_pack_index
	WHERE blob_hash NOT IN (` + attachmentReferencedHashesSQL + `
	)`

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
	return scanPackIndexEntries(rows, "pack index entries for "+packID, packID)
}

// UnpackedBlob is one distinct local blob that has no pack index row. Hash is
// canonical lowercase for every valid SHA-256 value; OriginalHashes retains
// every case spelling that must be canonicalized atomically when the blob is
// packed. Malformed hashes remain unmodified and distinct so maintenance can
// report and preserve them without deriving content-addressed paths. Paths
// contains every distinct DB-recorded local candidate path relative to the
// attachments dir, slash-separated. Size is -1 when unknown (thumbnail-only).
type UnpackedBlob struct {
	Hash           string
	OriginalHashes []string
	Paths          []string
	Size           int64
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
	seenAliases := make(map[string]map[string]struct{})

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
			canonicalHash, normalizeErr := normalizeBlobHash(hash)
			key := "valid:" + canonicalHash
			if normalizeErr != nil {
				canonicalHash = hash
				key = "malformed:" + hash
			}
			idx, exists := byHash[key]
			if !exists {
				idx = len(blobs)
				byHash[key] = idx
				seenPaths[key] = make(map[string]struct{})
				seenAliases[key] = make(map[string]struct{})
				blobs = append(blobs, UnpackedBlob{Hash: canonicalHash, Size: size})
			}
			if _, seen := seenAliases[key][hash]; !seen {
				seenAliases[key][hash] = struct{}{}
				blobs[idx].OriginalHashes = append(blobs[idx].OriginalHashes, hash)
			}
			if scanSize && size > blobs[idx].Size {
				blobs[idx].Size = size
			}
			if _, dup := seenPaths[key][path]; dup {
				continue
			}
			seenPaths[key][path] = struct{}{}
			blobs[idx].Paths = append(blobs[idx].Paths, path)
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
		  AND LOWER(storage_path) NOT LIKE 'http://%'
		  AND LOWER(storage_path) NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = LOWER(attachments.content_hash))
		GROUP BY content_hash, storage_path
		ORDER BY MIN(id), storage_path`, true); err != nil {
		return nil, err
	}
	if err := collect(`
		SELECT thumbnail_hash, thumbnail_path
		FROM attachments
		WHERE thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
		  AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
		  AND LOWER(thumbnail_path) NOT LIKE 'http://%'
		  AND LOWER(thumbnail_path) NOT LIKE 'https://%'
		  AND NOT EXISTS (SELECT 1 FROM attachment_pack_index p
		                  WHERE p.blob_hash = LOWER(attachments.thumbnail_hash))
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

// ListIndexedBlobEntries returns every packed blob mapping keyed by blob hash.
// It includes stale mappings so filesystem sweep can account for every index
// row before reference pruning.
func (s *Store) ListIndexedBlobEntries() (map[string]PackIndexEntry, error) {
	rows, err := s.db.Query(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index`)
	if err != nil {
		return nil, fmt.Errorf("list indexed blob entries: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	byHash := make(map[string]PackIndexEntry)
	for rows.Next() {
		entry, err := scanPackIndexEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan indexed blob entry: %w", err)
		}
		byHash[entry.BlobHash] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexed blob entries: %w", err)
	}
	return byHash, nil
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
