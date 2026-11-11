package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

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

// listPackVerificationWindow fixes candidate hashes on the first listing and
// rereads their current mappings on the second, under the maintenance lease.
func (s *Store) listPackVerificationWindow(ctx context.Context, pass *PackVerificationPass, root string) ([]PackIndexEntry, error) {
	if pass.selected {
		if len(pass.hashes) == 0 {
			return nil, nil
		}
		return s.listPackVerificationCandidates(ctx, pass.hashes)
	}
	hashes, err := s.packVerificationCandidates(ctx, root)
	if err != nil {
		return nil, err
	}
	var entries []PackIndexEntry
	var bytes int64
	for start := sort.Search(len(hashes), func(i int) bool { return hashes[i] > pass.start }); start < len(hashes) && !pass.more; start += 256 {
		batch, err := s.listPackVerificationCandidates(ctx, hashes[start:min(start+256, len(hashes))])
		if err != nil {
			return nil, err
		}
		for _, entry := range batch {
			if len(entries) >= pass.maxEntries || (len(entries) > 0 && entry.RawLen > pass.maxBytes-bytes) {
				pass.more = true
				break
			}
			entries = append(entries, entry)
			pass.hashes = append(pass.hashes, entry.BlobHash)
			pass.end = entry.BlobHash
			bytes += entry.RawLen
		}
	}
	pass.selected = true
	return entries, nil
}

// listPackVerificationCandidates avoids one query per unindexed loose file.
func (s *Store) listPackVerificationCandidates(ctx context.Context, hashes []string) ([]PackIndexEntry, error) {
	args := make([]any, len(hashes))
	for i, hash := range hashes {
		args[i] = hash
	}
	query := `SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash IN (` + placeholders(len(hashes)) + `) ORDER BY blob_hash`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pack verification candidates: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	return scanPackIndexEntries(rows, "pack verification candidates", "")
}

// packVerificationCandidates finds canonical loose sources and mappings whose
// physical pack is missing, including mappings without a pack record.
func (s *Store) packVerificationCandidates(ctx context.Context, root string) ([]string, error) {
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	if err != nil {
		return nil, err
	}
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{})
	if err != nil {
		return nil, err
	}
	defer backend.Close() //nolint:errcheck // inventory opens no pack readers
	inventory, err := backend.Inventory(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("inventory attachment files: %w", err)
	}
	candidates := make(map[string]struct{})
	present := make(map[string]struct{})
	for _, object := range inventory.Objects {
		if object.Ref.LooseHash != "" {
			candidates[object.Ref.LooseHash.String()] = struct{}{}
		} else {
			present[object.Ref.PackID] = struct{}{}
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
		if _, ok := present[id]; !ok {
			missing = append(missing, id)
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
		entries, err := s.ListAttachmentPackEntriesContext(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			candidates[entry.BlobHash] = struct{}{}
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
