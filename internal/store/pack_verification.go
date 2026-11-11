package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/kit/packstore"
)

const packVerificationCursorKey = "attachment_pack_verification_cursor"

type packVerificationKey struct{}

// PackVerificationPass limits physical verification during one automatic Pack
// call. All resolver, reference, and unpack inventories remain authoritative.
// Kit's two ListIndexed calls (repair and loose sweeping) use the same candidate
// hashes; omitted entries are deferred, never treated as absent from the
// catalog. Rows are read when Kit lists them, while it holds the maintenance
// lease, so a repack that finished earlier cannot leave a stale pack location
// for repair to delete.
type PackVerificationPass struct {
	skipLooseEnumeration bool
	start                string
	maxEntries           int
	maxBytes             int64
	// selected is set by the first ListIndexed call, which fixes the range.
	selected bool
	end      string
	hashes   []string
	more     bool
}

// SkipLooseEnumeration empties Pack's candidate list for a pass that only advances
// verification and has no new loose blob to find. Reference inventory, orphan
// reconciliation, and loose sweeping are untouched.
func (p *PackVerificationPass) SkipLooseEnumeration() {
	p.skipLooseEnumeration = true
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

// listPackVerificationWindow selects at most maxEntries rows or maxBytes of
// raw content after the cursor, always admitting one oversized row. Maintenance
// catalogs restrict selection to actionable candidates; catalogs without a
// filesystem root retain the full index window. Later calls use the same window.
func (s *Store) listPackVerificationWindow(
	ctx context.Context, pass *PackVerificationPass, root string,
) ([]PackIndexEntry, error) {
	if root != "" {
		return s.listActionablePackWindow(ctx, pass, root)
	}
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

// listActionablePackWindow fixes the candidate hashes on the first listing,
// then reads their current mappings on both listings under the maintenance lease.
func (s *Store) listActionablePackWindow(ctx context.Context, pass *PackVerificationPass, root string) ([]PackIndexEntry, error) {
	if !pass.selected {
		hashes, err := s.packVerificationCandidates(ctx, root)
		if err != nil {
			return nil, err
		}
		var bytes int64
		for _, hash := range hashes {
			if hash <= pass.start {
				continue
			}
			entry, err := s.GetAttachmentPackEntry(ctx, hash)
			if err != nil {
				return nil, err
			}
			if entry == nil {
				continue
			}
			if len(pass.hashes) >= pass.maxEntries || (len(pass.hashes) > 0 && entry.RawLen > pass.maxBytes-bytes) {
				pass.more = true
				break
			}
			pass.hashes = append(pass.hashes, hash)
			pass.end = hash
			bytes += entry.RawLen
		}
		pass.selected = true
	}
	entries := make([]PackIndexEntry, 0, len(pass.hashes))
	for _, hash := range pass.hashes {
		entry, err := s.GetAttachmentPackEntry(ctx, hash)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries, nil
}

// packVerificationCandidates finds canonical loose sources and mappings whose
// physical pack is missing, including mappings without a pack record.
func (s *Store) packVerificationCandidates(ctx context.Context, root string) ([]string, error) {
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	if err != nil {
		return nil, err
	}
	candidates := make(map[string]struct{})
	dirs, err := os.ReadDir(layout.Root())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("list loose attachment directories: %w", err)
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !dir.IsDir() || len(dir.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(layout.Root(), dir.Name()))
		if err != nil {
			return nil, fmt.Errorf("list loose attachment directory: %w", err)
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !file.Type().IsRegular() {
				continue
			}
			hash, err := packstore.ParseHash(strings.TrimSuffix(file.Name(), ".zst"))
			if err != nil || hash.String()[:2] != dir.Name() {
				continue
			}
			path := filepath.Join(layout.Root(), dir.Name(), file.Name())
			if path == layout.LoosePath(hash) || path == layout.CompressedLoosePath(hash) {
				candidates[hash.String()] = struct{}{}
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT pack_id FROM attachment_pack_index`)
	if err != nil {
		return nil, fmt.Errorf("list indexed packs: %w", err)
	}
	var missing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck // read-only cursor
			return nil, err
		}
		if _, err := os.Stat(layout.PackPath(id)); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, id)
		} else if err != nil {
			rows.Close() //nolint:errcheck // read-only cursor
			return nil, fmt.Errorf("stat indexed pack: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck // read-only cursor
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range missing {
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT blob_hash FROM attachment_pack_index WHERE pack_id = ?`), id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				rows.Close() //nolint:errcheck // read-only cursor
				return nil, err
			}
			candidates[hash] = struct{}{}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	hashes := make([]string, 0, len(candidates))
	for hash := range candidates {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	return hashes, nil
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
