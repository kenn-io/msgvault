package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// The sqlite3 driver is registered by internal/store in production; the
	// blank import keeps internal/backup usable standalone (tests, restore).
	_ "github.com/mattn/go-sqlite3"
)

// FreezeCoordinator brackets the freeze window: Begin drains and holds the
// daemon's operation gate, End releases it (docs/architecture/backup-format.md, Freeze Protocol). The pinned read
// transaction — which keeps the main DB file frozen afterwards — is owned by
// FrozenSession, not the coordinator.
type FreezeCoordinator interface {
	Begin(ctx context.Context) error
	End(ctx context.Context) error
}

// NoopFreezeCoordinator is for tests and capture paths with no daemon.
type NoopFreezeCoordinator struct{}

// Begin implements FreezeCoordinator.
func (NoopFreezeCoordinator) Begin(context.Context) error { return nil }

// End implements FreezeCoordinator.
func (NoopFreezeCoordinator) End(context.Context) error { return nil }

const (
	checkpointRetries = 50
	checkpointBackoff = 100 * time.Millisecond

	// freezeEndTimeout bounds the FreezeCoordinator.End call made when
	// closing out the freeze window. It runs against a fresh context
	// (context.Background(), not the caller's request context) because the
	// gate must still be released — and released promptly — even when the
	// caller's context is already canceled or openPinnedSession itself
	// failed; a short, independent bound keeps that release from hanging.
	freezeEndTimeout = 10 * time.Second
)

// FrozenSession holds the pinned read transaction that freezes the main DB
// file in content and size while writers proceed into the WAL.
type FrozenSession struct {
	db        *sql.DB
	tx        *sql.Tx
	PageSize  uint32
	PageCount uint64
}

// OpenFrozenSession executes the freeze protocol: gate -> checkpoint
// TRUNCATE -> pinned read transaction -> capture geometry -> gate release.
//
// The gate is released here, before attachment capture runs, so a gated
// operation that deletes attachment files (remove-account) can race a
// long-running capture and delete a file the pinned transaction still
// references. That backup fails loudly (read or hash error) and is
// retryable; holding the gate through the whole capture window would block
// every daemon write for minutes instead. Accepted limitation — see
// docs/architecture/backup-format.md, Current Limitations.
func OpenFrozenSession(ctx context.Context, dbPath string, fc FreezeCoordinator) (*FrozenSession, error) {
	if err := fc.Begin(ctx); err != nil {
		return nil, fmt.Errorf("backup: freeze begin: %w", err)
	}
	s, err := openPinnedSession(ctx, dbPath)
	endCtx, cancel := context.WithTimeout(context.Background(), freezeEndTimeout)
	endErr := fc.End(endCtx)
	cancel()
	if err != nil {
		if s != nil {
			_ = s.Close()
		}
		if endErr != nil {
			return nil, errors.Join(err, fmt.Errorf("backup: freeze end: %w", endErr))
		}
		return nil, err
	}
	if endErr != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: freeze end: %w", endErr)
	}
	return s, nil
}

func openPinnedSession(ctx context.Context, dbPath string) (*FrozenSession, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("backup: opening DB %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	s := &FrozenSession{db: db}

	var busy, logFrames, checkpointed int
	for attempt := 0; ; attempt++ {
		row := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		if err := row.Scan(&busy, &logFrames, &checkpointed); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("backup: wal_checkpoint: %w", err)
		}
		if busy == 0 {
			break
		}
		if attempt >= checkpointRetries {
			_ = s.Close()
			return nil, fmt.Errorf("backup: wal_checkpoint stayed busy after %d attempts (long-running reader?)", attempt)
		}
		select {
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		case <-time.After(checkpointBackoff):
		}
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: opening pinned read transaction: %w", err)
	}
	s.tx = tx
	// Touch the schema to materialize the read mark at WAL offset zero.
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: pinning read snapshot: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&s.PageSize); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: reading page_size: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&s.PageCount); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: reading page_count: %w", err)
	}
	return s, nil
}

const contentBearing = `content_hash IS NOT NULL AND content_hash != ''
	AND storage_path IS NOT NULL AND storage_path != ''
	AND storage_path NOT LIKE 'http%'`

const thumbBearing = `thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
	AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
	AND thumbnail_path NOT LIKE 'http%'`

// attachmentBlobsQuery counts the distinct content-bearing hashes reachable
// from the archive with thumbnails included: exactly the population
// AttachmentRefs enumerates and CaptureAttachments stores. UNION deduplicates
// a thumbnail hash that also appears as a content hash, so this count always
// equals len(AttachmentRefs) and the manifest's attachments.blobs.
const attachmentBlobsQuery = `SELECT COUNT(*) FROM (
	SELECT content_hash AS h FROM attachments WHERE ` + contentBearing + `
	UNION
	SELECT thumbnail_hash AS h FROM attachments WHERE ` + thumbBearing + `
)`

// Stats computes manifest stats inside the frozen snapshot so restore
// proofs compare exactly (docs/architecture/backup-format.md, Freeze Protocol).
func (s *FrozenSession) Stats(ctx context.Context) (ManifestStats, error) {
	return computeManifestStats(ctx, s.tx)
}

// rowQuerier is the query surface computeManifestStats needs, satisfied by
// both *sql.Tx (freeze-time capture) and *sql.DB (restore-time proof).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// computeManifestStats runs the manifest stats queries against q. Capture
// and restore share this one implementation, so the restore proof compares
// numbers derived by exactly the same queries the manifest recorded.
func computeManifestStats(ctx context.Context, q rowQuerier) (ManifestStats, error) {
	var st ManifestStats
	counts := []struct {
		dst   *int64
		query string
	}{
		{&st.Messages, "SELECT COUNT(*) FROM messages"},
		{&st.Conversations, "SELECT COUNT(*) FROM conversations"},
		{&st.Sources, "SELECT COUNT(*) FROM sources"},
		{&st.Accounts, "SELECT COUNT(*) FROM account_identities"},
		{&st.Labels, "SELECT COUNT(*) FROM labels"},
		{&st.AttachmentRows, "SELECT COUNT(*) FROM attachments"},
		{&st.AttachmentBlobs, attachmentBlobsQuery},
	}
	for _, c := range counts {
		if err := q.QueryRowContext(ctx, c.query).Scan(c.dst); err != nil {
			return st, fmt.Errorf("backup: stats query %q: %w", c.query, err)
		}
	}
	err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MIN(sent_at),''), COALESCE(MAX(sent_at),'') FROM messages",
	).Scan(&st.DateRange[0], &st.DateRange[1])
	if err != nil {
		return st, fmt.Errorf("backup: date range query: %w", err)
	}
	return st, nil
}

// AttachmentRefs returns the frozen content-bearing locator set in
// first-seen order, with thumbnails appended. size is nullable, so a hash
// whose rows all lack size metadata yields the same -1 sentinel thumbnails
// use; capture resolves the real size from the file either way. Each ref
// carries one recorded storage path (MIN across the hash's rows — any copy
// works, capture re-derives the hash from the bytes), because importers may
// namespace paths rather than use the plain "<aa>/<hash>" layout.
func (s *FrozenSession) AttachmentRefs(ctx context.Context) ([]ContentRef, error) {
	rows, err := s.tx.QueryContext(ctx,
		"SELECT content_hash, COALESCE(MAX(size), -1), MIN(storage_path) FROM attachments WHERE "+contentBearing+
			" GROUP BY content_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("backup: attachment locator query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []ContentRef
	seen := map[string]bool{}
	for rows.Next() {
		var ref ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("backup: scanning attachment locator: %w", err)
		}
		refs = append(refs, ref)
		seen[ref.Hash] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: attachment locator rows: %w", err)
	}

	thumbRows, err := s.tx.QueryContext(ctx,
		"SELECT thumbnail_hash, MIN(thumbnail_path) FROM attachments WHERE "+thumbBearing+
			" GROUP BY thumbnail_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("backup: thumbnail locator query: %w", err)
	}
	defer func() { _ = thumbRows.Close() }()
	for thumbRows.Next() {
		var ref ContentRef
		if err := thumbRows.Scan(&ref.Hash, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("backup: scanning thumbnail locator: %w", err)
		}
		if !seen[ref.Hash] {
			ref.Size = -1
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	if err := thumbRows.Err(); err != nil {
		return nil, fmt.Errorf("backup: thumbnail locator rows: %w", err)
	}
	return refs, nil
}

// HasNonCanonicalAttachmentPaths reports whether any content-bearing row in
// the frozen snapshot records a storage or thumbnail path other than the
// canonical loose "<hash[:2]>/<hash>" derivation. It inspects every row, not
// AttachmentRefs' one-path-per-hash selection, because a hash can be
// recorded at a canonical path in one row and a namespaced path in another.
// Snapshots containing such paths require a path-aware restore and are
// marked with a higher manifest reader version at create time.
func (s *FrozenSession) HasNonCanonicalAttachmentPaths(ctx context.Context) (bool, error) {
	var found bool
	err := s.tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM attachments WHERE `+contentBearing+`
		  AND storage_path != substr(content_hash, 1, 2) || '/' || content_hash
		UNION ALL
		SELECT 1 FROM attachments WHERE `+thumbBearing+`
		  AND thumbnail_path != substr(thumbnail_hash, 1, 2) || '/' || thumbnail_hash
	)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("backup: attachment path canonicality query: %w", err)
	}
	return found, nil
}

// Close releases the pinned transaction and connection. Idempotent.
func (s *FrozenSession) Close() error {
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}
