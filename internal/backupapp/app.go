// Package backupapp implements go.kenn.io/kit/backup's App interface for
// msgvault: the schema queries, layout names, and manifest stats that make the
// generic snapshot engine back up a msgvault archive.
package backupapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"

	"go.kenn.io/kit/backup"
)

// contentBearing and thumbBearing select attachment rows whose bytes live
// in the local attachments tree. Only genuine URL schemes are excluded — a
// local namespaced path is free to start with "http" (an importer may use
// an "http-cache/" namespace), so the patterns must match the "://"
// separator, not the bare prefix.
const contentBearing = `content_hash IS NOT NULL AND content_hash != ''
	AND storage_path IS NOT NULL AND storage_path != ''
	AND storage_path NOT LIKE 'http://%'
	AND storage_path NOT LIKE 'https://%'`

const thumbBearing = `thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
	AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
	AND thumbnail_path NOT LIKE 'http://%'
	AND thumbnail_path NOT LIKE 'https://%'`

// attachmentBlobsQuery counts the distinct content-bearing hashes reachable
// from the archive with thumbnails included: exactly the population
// ContentInfo enumerates and CaptureAttachments stores. UNION deduplicates
// a thumbnail hash that also appears as a content hash, so this count always
// equals len(ContentInfo.Refs) and the manifest's attachments.blobs.
const attachmentBlobsQuery = `SELECT COUNT(*) FROM (
	SELECT content_hash AS h FROM attachments WHERE ` + contentBearing + `
	UNION
	SELECT thumbnail_hash AS h FROM attachments WHERE ` + thumbBearing + `
)`

// Stats is msgvault's manifest stats payload (moved from backup.ManifestStats;
// identical field order and json tags).
type Stats struct {
	Messages        int64     `json:"messages"`
	Conversations   int64     `json:"conversations"`
	Sources         int64     `json:"sources"`
	Accounts        int64     `json:"accounts"`
	AttachmentRows  int64     `json:"attachment_rows"`
	AttachmentBlobs int64     `json:"attachment_blobs"`
	Labels          int64     `json:"labels"`
	DateRange       [2]string `json:"date_range"`
}

// rowQuerier is the query surface computeManifestStats needs, satisfied by
// both *sql.Tx (freeze-time capture) and *sql.DB (restore-time proof).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// computeManifestStats runs the manifest stats queries against q. Capture
// and restore share this one implementation, so the restore proof compares
// numbers derived by exactly the same queries the manifest recorded.
func computeManifestStats(ctx context.Context, q rowQuerier) (Stats, error) {
	var st Stats
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
			return st, fmt.Errorf("backupapp: stats query %q: %w", c.query, err)
		}
	}
	err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MIN(sent_at),''), COALESCE(MAX(sent_at),'') FROM messages",
	).Scan(&st.DateRange[0], &st.DateRange[1])
	if err != nil {
		return st, fmt.Errorf("backupapp: date range query: %w", err)
	}
	return st, nil
}

// manifestExcluded names the live-archive paths a snapshot never captures.
var manifestExcluded = []string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"}

// App implements backup.App for msgvault.
type App struct{ version string }

var _ backup.App = (*App)(nil)

// New returns an App recording version as the manifest's app version.
func New(version string) *App { return &App{version: version} }

// FrozenView implements backup.App.
func (a *App) FrozenView(s *backup.FrozenSession) backup.FrozenView {
	return &frozenView{tx: s.Tx()}
}

// DBFileName implements backup.App.
func (a *App) DBFileName() string { return "msgvault.db" }

// ContentDirName implements backup.App.
func (a *App) ContentDirName() string { return "attachments" }

// PackFileExtension implements backup.App. Existing msgvault backup
// repositories are written with this extension, so it is frozen.
func (a *App) PackFileExtension() string { return ".mvpack" }

// ExcludedPaths implements backup.App.
func (a *App) ExcludedPaths() []string { return manifestExcluded }

// Version implements backup.App.
func (a *App) Version() string { return a.version }

// frozenView implements backup.FrozenView against a FrozenSession's pinned
// read transaction.
type frozenView struct{ tx *sql.Tx }

// ContentInfo returns the frozen content-bearing locator set in first-seen
// order, with thumbnails appended. size is nullable, so a hash whose rows
// all lack size metadata yields the same -1 sentinel thumbnails use; capture
// resolves the real size from the file either way. Each ref carries one
// recorded storage path (MIN across the hash's rows — any copy works,
// capture re-derives the hash from the bytes), because importers may
// namespace paths rather than use the plain "<aa>/<hash>" layout.
func (v *frozenView) ContentInfo(ctx context.Context) (*backup.ContentInfo, error) {
	rows, err := v.tx.QueryContext(ctx,
		"SELECT content_hash, COALESCE(MAX(size), -1), MIN(storage_path) FROM attachments WHERE "+contentBearing+
			" GROUP BY content_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("backupapp: attachment locator query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []backup.ContentRef
	seen := map[string]bool{}
	for rows.Next() {
		var ref backup.ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("backupapp: scanning attachment locator: %w", err)
		}
		refs = append(refs, ref)
		seen[ref.Hash] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backupapp: attachment locator rows: %w", err)
	}

	thumbRows, err := v.tx.QueryContext(ctx,
		"SELECT thumbnail_hash, MIN(thumbnail_path) FROM attachments WHERE "+thumbBearing+
			" GROUP BY thumbnail_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("backupapp: thumbnail locator query: %w", err)
	}
	defer func() { _ = thumbRows.Close() }()
	for thumbRows.Next() {
		var ref backup.ContentRef
		if err := thumbRows.Scan(&ref.Hash, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("backupapp: scanning thumbnail locator: %w", err)
		}
		if !seen[ref.Hash] {
			ref.Size = -1
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	if err := thumbRows.Err(); err != nil {
		return nil, fmt.Errorf("backupapp: thumbnail locator rows: %w", err)
	}

	var rowCount int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM attachments").Scan(&rowCount); err != nil {
		return nil, fmt.Errorf("backupapp: attachment row count query: %w", err)
	}

	nonCanonical, err := v.hasNonCanonicalAttachmentPaths(ctx)
	if err != nil {
		return nil, err
	}

	return &backup.ContentInfo{Refs: refs, Rows: rowCount, NonCanonicalPaths: nonCanonical}, nil
}

// hasNonCanonicalAttachmentPaths reports whether any content-bearing row in
// the frozen snapshot records a storage or thumbnail path other than the
// canonical loose "<hash[:2]>/<hash>" derivation. It inspects every row, not
// ContentInfo's one-path-per-hash selection, because a hash can be recorded
// at a canonical path in one row and a namespaced path in another. Snapshots
// containing such paths require a path-aware restore and are marked with a
// higher manifest reader version at create time.
func (v *frozenView) hasNonCanonicalAttachmentPaths(ctx context.Context) (bool, error) {
	var found bool
	err := v.tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM attachments WHERE `+contentBearing+`
		  AND storage_path != substr(content_hash, 1, 2) || '/' || content_hash
		UNION ALL
		SELECT 1 FROM attachments WHERE `+thumbBearing+`
		  AND thumbnail_path != substr(thumbnail_hash, 1, 2) || '/' || thumbnail_hash
	)`).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("backupapp: attachment path canonicality query: %w", err)
	}
	return found, nil
}

// Stats implements backup.FrozenView.
func (v *frozenView) Stats(ctx context.Context) (json.RawMessage, error) {
	st, err := computeManifestStats(ctx, v.tx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

// RestoredStats implements backup.App.
func (a *App) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	st, err := computeManifestStats(ctx, db)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

// RestoredContentPaths maps each content and thumbnail hash in the restored
// database to every relative storage path it is recorded at. Paths come from
// DB rows, so each is validated as local before restore writes it.
func (a *App) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	// UNION deduplicates repeated (hash, path) rows across attachments.
	rows, err := db.QueryContext(ctx,
		"SELECT content_hash, storage_path FROM attachments WHERE "+contentBearing+
			" UNION SELECT thumbnail_hash, thumbnail_path FROM attachments WHERE "+thumbBearing)
	if err != nil {
		return nil, fmt.Errorf("backupapp: attachment path query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash, p string
		if err := rows.Scan(&hash, &p); err != nil {
			return nil, fmt.Errorf("backupapp: scanning attachment path: %w", err)
		}
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			return nil, fmt.Errorf(
				"backupapp: attachment %s storage path %q escapes the attachments directory", hash, p)
		}
		paths[hash] = append(paths[hash], rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backupapp: attachment path rows: %w", err)
	}
	return paths, nil
}

// CheckManifest returns app-level manifest consistency problems (verify).
func (a *App) CheckManifest(m *backup.Manifest) []string {
	st, err := ParseStats(m.Stats)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	if st.AttachmentBlobs != m.Attachments.Blobs {
		return []string{fmt.Sprintf(
			"stats.attachment_blobs %d != attachments.blobs %d",
			st.AttachmentBlobs, m.Attachments.Blobs)}
	}
	return nil
}

// ParseStats decodes a manifest stats payload.
func ParseStats(raw json.RawMessage) (Stats, error) {
	var st Stats
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("backupapp: parsing manifest stats: %w", err)
	}
	return st, nil
}
