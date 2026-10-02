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
// Kit's two ListIndexed calls (repair and loose sweeping) use the same window;
// omitted entries are deferred, never treated as absent from the catalog.
type PackVerificationPass struct {
	entries []PackIndexEntry
	after   string
	more    bool
}

// BeginPackVerification selects a bounded window after the durable cursor.
// It is only for Pack, never Unpack, which must enumerate every indexed blob.
func (s *Store) BeginPackVerification(ctx context.Context, maxEntries int, maxBytes int64) (context.Context, *PackVerificationPass, error) {
	if maxEntries <= 0 || maxBytes <= 0 {
		return ctx, nil, errors.New("pack verification limits must be positive")
	}
	var after string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), packVerificationCursorKey).Scan(&after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ctx, nil, fmt.Errorf("read pack verification cursor: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c FROM attachment_pack_index WHERE blob_hash > ? ORDER BY blob_hash LIMIT ?`), after, maxEntries+1)
	if err != nil {
		return ctx, nil, fmt.Errorf("list pack verification window: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	pass := &PackVerificationPass{}
	var bytes int64
	for rows.Next() {
		entry, err := scanPackIndexEntry(rows)
		if err != nil {
			return ctx, nil, fmt.Errorf("scan pack verification window: %w", err)
		}
		if len(pass.entries) >= maxEntries || (len(pass.entries) > 0 && entry.RawLen > maxBytes-bytes) {
			pass.more = true
			break
		}
		pass.entries = append(pass.entries, entry)
		pass.after = entry.BlobHash
		bytes += entry.RawLen
	}
	if err := rows.Err(); err != nil {
		return ctx, nil, fmt.Errorf("iterate pack verification window: %w", err)
	}
	return context.WithValue(ctx, packVerificationKey{}, pass), pass, nil
}

// FinishPackVerification advances only after Pack successfully verified its
// entire window. A cancelled pass replays the window. Completing a cycle
// resets the cursor so the next maintenance request starts a fresh check.
func (s *Store) FinishPackVerification(ctx context.Context, pass *PackVerificationPass) (bool, error) {
	after := pass.after
	if !pass.more {
		after = ""
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error { return setArchiveMetadataValueTx(ctx, tx, packVerificationCursorKey, after) })
	if err != nil {
		return false, fmt.Errorf("checkpoint pack verification: %w", err)
	}
	return pass.more, nil
}
