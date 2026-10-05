package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const packVerificationCursorKey = "attachment_pack_verification_cursor"

type packVerificationKey struct{}

// PackVerificationPass limits physical verification during one automatic Pack
// call. All resolver, reference, and unpack inventories remain authoritative.
// Kit's two ListIndexed calls (repair and loose sweeping) use the same hash
// range; omitted entries are deferred, never treated as absent from the
// catalog. Rows are read when Kit lists them, while it holds the maintenance
// lease, so a repack that finished earlier cannot leave a stale pack location
// for repair to delete.
type PackVerificationPass struct {
	start      string
	maxEntries int
	maxBytes   int64
	// selected is set by the first ListIndexed call, which fixes the range.
	selected bool
	end      string
	more     bool
}

// BeginPackVerification starts a bounded verification window after the
// durable cursor. It is only for Pack, never Unpack, which must enumerate
// every indexed blob.
func (s *Store) BeginPackVerification(
	ctx context.Context, maxEntries int, maxBytes int64,
) (context.Context, *PackVerificationPass, error) {
	if maxEntries <= 0 || maxBytes <= 0 {
		return ctx, nil, errors.New("pack verification limits must be positive")
	}
	var start string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`),
		packVerificationCursorKey).Scan(&start)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ctx, nil, fmt.Errorf("read pack verification cursor: %w", err)
	}
	pass := &PackVerificationPass{start: start, maxEntries: maxEntries, maxBytes: maxBytes}
	return context.WithValue(ctx, packVerificationKey{}, pass), pass, nil
}

// listPackVerificationWindow returns the current index rows in the pass's
// hash range. The first call selects at most maxEntries rows or maxBytes of
// raw content after the cursor, always admitting one oversized row. Later
// calls return the current rows in that same range, so rows Pack adds past
// its end wait for a later window.
func (s *Store) listPackVerificationWindow(
	ctx context.Context, pass *PackVerificationPass,
) ([]PackIndexEntry, error) {
	if pass.selected && pass.end == "" {
		return nil, nil
	}
	query := `SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash > ?`
	args := []any{pass.start}
	if pass.selected {
		query += ` AND blob_hash <= ? ORDER BY blob_hash`
		args = append(args, pass.end)
	} else {
		query += ` ORDER BY blob_hash LIMIT ?`
		args = append(args, pass.maxEntries+1)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list pack verification window: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var entries []PackIndexEntry
	var bytes int64
	more := false
	for rows.Next() {
		entry, err := scanPackIndexEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pack verification window: %w", err)
		}
		if !pass.selected && (len(entries) >= pass.maxEntries ||
			(len(entries) > 0 && entry.RawLen > pass.maxBytes-bytes)) {
			more = true
			break
		}
		entries = append(entries, entry)
		bytes += entry.RawLen
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pack verification window: %w", err)
	}
	if !pass.selected {
		pass.selected = true
		pass.more = more
		if len(entries) > 0 {
			pass.end = entries[len(entries)-1].BlobHash
		}
	}
	return entries, nil
}

// FinishPackVerification advances only after Pack successfully verified its
// entire window. A cancelled pass replays the window. Completing a cycle
// resets the cursor so the next maintenance request starts a fresh check.
func (s *Store) FinishPackVerification(ctx context.Context, pass *PackVerificationPass) (bool, error) {
	if !pass.selected {
		return false, nil
	}
	after := pass.end
	if !pass.more {
		after = ""
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		return setArchiveMetadataValueTx(ctx, tx, packVerificationCursorKey, after)
	})
	if err != nil {
		return false, fmt.Errorf("checkpoint pack verification: %w", err)
	}
	return pass.more, nil
}
