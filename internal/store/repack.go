package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kit/pack"
)

// PackUsage combines a sealed pack's immutable footer totals with the subset
// of its index rows that still have an attachment reference.
type PackUsage struct {
	PackRecord

	LiveEntries     int64
	LiveStoredBytes int64
	LiveRawBytes    int64
}

// RepackMove describes one compare-and-swap from an expected old pack to an
// entry in a newly sealed pack.
type RepackMove struct {
	OldPackID string
	NewEntry  PackIndexEntry
}

// ListPackUsage returns every recorded pack in deterministic creation order.
// Attachment references, not index rows alone, define the live aggregates.
func (s *Store) ListPackUsage() ([]PackUsage, error) {
	rows, err := s.db.Query(`
		WITH referenced AS (
		    SELECT i.blob_hash, i.pack_id, i.stored_len, i.raw_len
		    FROM attachment_pack_index i
		    WHERE EXISTS (
		        SELECT 1 FROM attachments a
		        WHERE a.content_hash = i.blob_hash
		           OR a.thumbnail_hash = i.blob_hash
		    )
		)
		SELECT p.pack_id, p.entry_count, p.stored_bytes, p.created_at,
		       COUNT(r.blob_hash), COALESCE(SUM(r.stored_len), 0),
		       COALESCE(SUM(r.raw_len), 0)
		FROM attachment_packs p
		LEFT JOIN referenced r ON r.pack_id = p.pack_id
		GROUP BY p.pack_id, p.entry_count, p.stored_bytes, p.created_at
		ORDER BY p.created_at, p.pack_id`)
	if err != nil {
		return nil, fmt.Errorf("list attachment pack usage: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor

	var usage []PackUsage
	for rows.Next() {
		var u PackUsage
		var createdAt string
		if err := rows.Scan(&u.PackID, &u.EntryCount, &u.StoredBytes, &createdAt,
			&u.LiveEntries, &u.LiveStoredBytes, &u.LiveRawBytes); err != nil {
			return nil, fmt.Errorf("scan attachment pack usage: %w", err)
		}
		u.CreatedAt, err = time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at for pack %s: %w", u.PackID, err)
		}
		if err := validatePackUsage(u); err != nil {
			return nil, err
		}
		usage = append(usage, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachment pack usage: %w", err)
	}
	return usage, nil
}

func validatePackUsage(u PackUsage) error {
	if !pack.IsValidPackID(u.PackID) {
		return fmt.Errorf("attachment pack usage has malformed pack id %q", u.PackID)
	}
	if u.EntryCount < 0 || u.StoredBytes < 0 || u.LiveEntries < 0 ||
		u.LiveStoredBytes < 0 || u.LiveRawBytes < 0 ||
		u.LiveEntries > u.EntryCount || u.LiveStoredBytes > u.StoredBytes {
		return fmt.Errorf(
			"attachment pack %s has impossible accounting: entries=%d live_entries=%d stored_bytes=%d live_stored_bytes=%d live_raw_bytes=%d",
			u.PackID, u.EntryCount, u.LiveEntries, u.StoredBytes,
			u.LiveStoredBytes, u.LiveRawBytes)
	}
	return nil
}

// ListReferencedPackEntries returns only index rows that still have an
// attachment reference, ordered by their physical position.
func (s *Store) ListReferencedPackEntries(packID string) ([]PackIndexEntry, error) {
	if !pack.IsValidPackID(packID) {
		return nil, fmt.Errorf("list referenced pack entries: malformed pack id %q", packID)
	}
	rows, err := s.db.Query(`
		SELECT i.blob_hash, i.pack_id, i.pack_offset, i.stored_len,
		       i.raw_len, i.flags, i.crc32c
		FROM attachment_pack_index i
		WHERE i.pack_id = ?
		  AND EXISTS (
		      SELECT 1 FROM attachments a
		      WHERE a.content_hash = i.blob_hash
		         OR a.thumbnail_hash = i.blob_hash
		  )
		ORDER BY i.pack_offset, i.blob_hash`, packID)
	if err != nil {
		return nil, fmt.Errorf("list referenced pack entries for %s: %w", packID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	entries, err := scanPackIndexEntries(rows, packID)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func scanPackIndexEntries(rows *loggedRows, packID string) ([]PackIndexEntry, error) {
	var entries []PackIndexEntry
	for rows.Next() {
		var entry PackIndexEntry
		var flags, crc int64
		if err := rows.Scan(&entry.BlobHash, &entry.PackID, &entry.Offset,
			&entry.StoredLen, &entry.RawLen, &flags, &crc); err != nil {
			return nil, fmt.Errorf("scan referenced pack entry for %s: %w", packID, err)
		}
		if entry.PackID != packID || len(entry.BlobHash) != 64 || entry.Offset < 0 ||
			entry.StoredLen < 0 || entry.RawLen < 0 || flags < 0 || flags > 255 ||
			crc < 0 || crc > int64(^uint32(0)) {
			return nil, fmt.Errorf("pack %s has malformed referenced index entry for %q", packID, entry.BlobHash)
		}
		entry.Flags = uint8(flags)
		entry.CRC32C = uint32(crc)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate referenced pack entries for %s: %w", packID, err)
	}
	return entries, nil
}

// CommitRepack atomically records newly sealed packs and moves every currently
// referenced mapping in the complete selected source-pack set. Exact-set
// validation prevents an omitted or concurrently changed mapping from being
// stranded when old pack files are retired.
func (s *Store) CommitRepack(
	ctx context.Context,
	sourcePackIDs []string,
	records []PackRecord,
	moves []RepackMove,
) error {
	selected, moveByHash, err := validateRepackInput(sourcePackIDs, records, moves)
	if err != nil {
		return err
	}

	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		expected := make(map[string]string)
		for sourcePackID := range selected {
			var exists int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM attachment_packs WHERE pack_id = ?`, sourcePackID,
			).Scan(&exists); err != nil {
				return fmt.Errorf("check selected source pack %s: %w", sourcePackID, err)
			}
			if exists != 1 {
				return fmt.Errorf("selected source pack %s does not exist", sourcePackID)
			}

			rows, err := tx.QueryContext(ctx, `
				SELECT i.blob_hash
				FROM attachment_pack_index i
				WHERE i.pack_id = ?
				  AND EXISTS (
				      SELECT 1 FROM attachments a
				      WHERE a.content_hash = i.blob_hash
				         OR a.thumbnail_hash = i.blob_hash
				  )`, sourcePackID)
			if err != nil {
				return fmt.Errorf("list current referenced mappings for %s: %w", sourcePackID, err)
			}
			for rows.Next() {
				var hash string
				if err := rows.Scan(&hash); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan current referenced mapping for %s: %w", sourcePackID, err)
				}
				expected[hash] = sourcePackID
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterate current referenced mappings for %s: %w", sourcePackID, err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close current referenced mappings for %s: %w", sourcePackID, err)
			}
		}

		if len(expected) != len(moveByHash) {
			return fmt.Errorf("repack move set is not an exact match: selected packs have %d referenced mappings, got %d moves", len(expected), len(moveByHash))
		}
		for hash, oldPackID := range expected {
			move, ok := moveByHash[hash]
			if !ok || move.OldPackID != oldPackID {
				return fmt.Errorf("repack move set is not an exact match for blob %s in pack %s", hash, oldPackID)
			}
		}

		for _, rec := range records {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, ?, ?, ?)`, rec.PackID, rec.EntryCount, rec.StoredBytes,
				rec.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("insert repacked attachment pack %s: %w", rec.PackID, err)
			}
		}

		for _, move := range moves {
			e := move.NewEntry
			res, err := tx.ExecContext(ctx, `
				UPDATE attachment_pack_index
				SET pack_id = ?, pack_offset = ?, stored_len = ?, raw_len = ?, flags = ?, crc32c = ?
				WHERE blob_hash = ? AND pack_id = ?`,
				e.PackID, e.Offset, e.StoredLen, e.RawLen, int64(e.Flags),
				int64(e.CRC32C), e.BlobHash, move.OldPackID)
			if err != nil {
				return fmt.Errorf("CAS repacked blob %s from %s: %w", e.BlobHash, move.OldPackID, err)
			}
			changed, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("count CAS result for repacked blob %s: %w", e.BlobHash, err)
			}
			if changed != 1 {
				return fmt.Errorf("CAS repacked blob %s from %s changed %d rows, want exactly 1", e.BlobHash, move.OldPackID, changed)
			}
		}

		for sourcePackID := range selected {
			var remaining int64
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM attachment_pack_index i
				WHERE i.pack_id = ?
				  AND EXISTS (
				      SELECT 1 FROM attachments a
				      WHERE a.content_hash = i.blob_hash
				         OR a.thumbnail_hash = i.blob_hash
				  )`, sourcePackID).Scan(&remaining); err != nil {
				return fmt.Errorf("verify source pack %s is empty: %w", sourcePackID, err)
			}
			if remaining != 0 {
				return fmt.Errorf("source pack %s retains %d referenced mappings after repack", sourcePackID, remaining)
			}
		}
		return nil
	})
}

func validateRepackInput(
	sourcePackIDs []string,
	records []PackRecord,
	moves []RepackMove,
) (map[string]struct{}, map[string]RepackMove, error) {
	selected := make(map[string]struct{}, len(sourcePackIDs))
	for _, id := range sourcePackIDs {
		if !pack.IsValidPackID(id) {
			return nil, nil, fmt.Errorf("repack source has malformed pack id %q", id)
		}
		if _, duplicate := selected[id]; duplicate {
			return nil, nil, fmt.Errorf("repack source pack %s is duplicated", id)
		}
		selected[id] = struct{}{}
	}

	newRecords := make(map[string]PackRecord, len(records))
	for _, rec := range records {
		if !pack.IsValidPackID(rec.PackID) {
			return nil, nil, fmt.Errorf("repack record has malformed pack id %q", rec.PackID)
		}
		if _, source := selected[rec.PackID]; source {
			return nil, nil, fmt.Errorf("repack output pack %s is also a source pack", rec.PackID)
		}
		if rec.EntryCount <= 0 || rec.StoredBytes < 0 || rec.CreatedAt.IsZero() {
			return nil, nil, fmt.Errorf("repack record %s has invalid immutable totals", rec.PackID)
		}
		if _, duplicate := newRecords[rec.PackID]; duplicate {
			return nil, nil, fmt.Errorf("repack output pack %s is duplicated", rec.PackID)
		}
		newRecords[rec.PackID] = rec
	}

	type totals struct {
		entries int64
		stored  int64
	}
	byNewPack := make(map[string]totals, len(records))
	moveByHash := make(map[string]RepackMove, len(moves))
	for _, move := range moves {
		if _, ok := selected[move.OldPackID]; !ok {
			return nil, nil, fmt.Errorf("repack move for %s names unselected source pack %q", move.NewEntry.BlobHash, move.OldPackID)
		}
		e := move.NewEntry
		if _, ok := newRecords[e.PackID]; !ok {
			return nil, nil, fmt.Errorf("repack move for %s names unknown output pack %q", e.BlobHash, e.PackID)
		}
		if len(e.BlobHash) != 64 || e.Offset < 0 || e.StoredLen < 0 || e.RawLen < 0 {
			return nil, nil, fmt.Errorf("repack move has malformed entry for blob %q", e.BlobHash)
		}
		if _, duplicate := moveByHash[e.BlobHash]; duplicate {
			return nil, nil, fmt.Errorf("repack move for blob %s is duplicated", e.BlobHash)
		}
		moveByHash[e.BlobHash] = move
		t := byNewPack[e.PackID]
		t.entries++
		t.stored += e.StoredLen
		byNewPack[e.PackID] = t
	}
	for packID, rec := range newRecords {
		t := byNewPack[packID]
		if t.entries != rec.EntryCount || t.stored != rec.StoredBytes {
			return nil, nil, fmt.Errorf("repack record %s totals do not match its moves", packID)
		}
	}
	if len(selected) == 0 && (len(records) != 0 || len(moves) != 0) {
		return nil, nil, errors.New("repack outputs require at least one selected source pack")
	}
	return selected, moveByHash, nil
}

// DeleteEmptyPackRecord removes a pack record only when no referenced mapping
// names it. Stale unreferenced index rows are deleted explicitly in the same
// transaction because the schema deliberately has no foreign-key cascade.
func (s *Store) DeleteEmptyPackRecord(packID string) (bool, error) {
	if !pack.IsValidPackID(packID) {
		return false, fmt.Errorf("delete empty pack record: malformed pack id %q", packID)
	}
	var deleted bool
	err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		var live int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM attachment_pack_index i
			WHERE i.pack_id = ?
			  AND EXISTS (
			      SELECT 1 FROM attachments a
			      WHERE a.content_hash = i.blob_hash
			         OR a.thumbnail_hash = i.blob_hash
			  )`, packID).Scan(&live); err != nil {
			return fmt.Errorf("count referenced mappings for pack %s: %w", packID, err)
		}
		if live != 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM attachment_pack_index WHERE pack_id = ?`, packID,
		); err != nil {
			return fmt.Errorf("delete stale mappings for pack %s: %w", packID, err)
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM attachment_packs WHERE pack_id = ?`, packID,
		)
		if err != nil {
			return fmt.Errorf("delete empty pack record %s: %w", packID, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("count deleted empty pack record %s: %w", packID, err)
		}
		if rows > 1 {
			return fmt.Errorf("delete empty pack record %s affected %d rows", packID, rows)
		}
		deleted = rows == 1
		return nil
	})
	return deleted, err
}
