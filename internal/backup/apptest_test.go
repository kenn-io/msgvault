package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testApp is the App collaborator the in-package engine tests capture and
// restore through. It mirrors the msgvault-shaped schema the engine's test
// fixtures seed, so the round-trip and verify tests exercise a realistic App.
//
// The production App is internal/backupapp; its query semantics are covered by
// backupapp_test. This double exists only because these tests live in package
// backup (they reach unexported engine internals), and package backup cannot
// import backupapp without an import cycle (backupapp imports backup).
type testApp struct{ version string }

var _ App = (*testApp)(nil)

// newTestApp returns the App the engine tests pass to Create/Restore/Verify.
func newTestApp() App { return &testApp{version: "test"} }

func (a *testApp) FrozenView(s *FrozenSession) FrozenView { return &testFrozenView{tx: s.Tx()} }

func (a *testApp) DBFileName() string     { return "msgvault.db" }
func (a *testApp) ContentDirName() string { return "attachments" }
func (a *testApp) Version() string        { return a.version }

func (a *testApp) ExcludedPaths() []string {
	return []string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"}
}

// testContentBearing / testThumbBearing select attachment rows whose bytes
// live in the local content tree; only genuine URL schemes are excluded.
const testContentBearing = `content_hash IS NOT NULL AND content_hash != ''
	AND storage_path IS NOT NULL AND storage_path != ''
	AND storage_path NOT LIKE 'http://%'
	AND storage_path NOT LIKE 'https://%'`

const testThumbBearing = `thumbnail_hash IS NOT NULL AND thumbnail_hash != ''
	AND thumbnail_path IS NOT NULL AND thumbnail_path != ''
	AND thumbnail_path NOT LIKE 'http://%'
	AND thumbnail_path NOT LIKE 'https://%'`

const testAttachmentBlobsQuery = `SELECT COUNT(*) FROM (
	SELECT content_hash AS h FROM attachments WHERE ` + testContentBearing + `
	UNION
	SELECT thumbnail_hash AS h FROM attachments WHERE ` + testThumbBearing + `
)`

// testStats mirrors backupapp.Stats' field order and json tags.
type testStats struct {
	Messages        int64     `json:"messages"`
	Conversations   int64     `json:"conversations"`
	Sources         int64     `json:"sources"`
	Accounts        int64     `json:"accounts"`
	AttachmentRows  int64     `json:"attachment_rows"`
	AttachmentBlobs int64     `json:"attachment_blobs"`
	Labels          int64     `json:"labels"`
	DateRange       [2]string `json:"date_range"`
}

type testRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func computeTestStats(ctx context.Context, q testRowQuerier) (testStats, error) {
	var st testStats
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
		{&st.AttachmentBlobs, testAttachmentBlobsQuery},
	}
	for _, c := range counts {
		if err := q.QueryRowContext(ctx, c.query).Scan(c.dst); err != nil {
			return st, fmt.Errorf("testapp: stats query %q: %w", c.query, err)
		}
	}
	err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MIN(sent_at),''), COALESCE(MAX(sent_at),'') FROM messages",
	).Scan(&st.DateRange[0], &st.DateRange[1])
	if err != nil {
		return st, fmt.Errorf("testapp: date range query: %w", err)
	}
	return st, nil
}

func (a *testApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	st, err := computeTestStats(ctx, db)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (a *testApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT content_hash, storage_path FROM attachments WHERE "+testContentBearing+
			" UNION SELECT thumbnail_hash, thumbnail_path FROM attachments WHERE "+testThumbBearing)
	if err != nil {
		return nil, fmt.Errorf("testapp: attachment path query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash, p string
		if err := rows.Scan(&hash, &p); err != nil {
			return nil, fmt.Errorf("testapp: scanning attachment path: %w", err)
		}
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("testapp: attachment %s storage path %q escapes the content directory", hash, p)
		}
		paths[hash] = append(paths[hash], rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: attachment path rows: %w", err)
	}
	return paths, nil
}

func (a *testApp) CheckManifest(m *Manifest) []string {
	st, err := parseTestStats(m.Stats)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	if st.AttachmentBlobs != m.Attachments.Blobs {
		return []string{fmt.Sprintf(
			"stats.attachment_blobs %d != attachments.blobs %d", st.AttachmentBlobs, m.Attachments.Blobs)}
	}
	return nil
}

// testFrozenView answers Create's schema questions against the pinned tx.
type testFrozenView struct{ tx *sql.Tx }

func (v *testFrozenView) Stats(ctx context.Context) (json.RawMessage, error) {
	st, err := computeTestStats(ctx, v.tx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (v *testFrozenView) ContentInfo(ctx context.Context) (*ContentInfo, error) {
	rows, err := v.tx.QueryContext(ctx,
		"SELECT content_hash, COALESCE(MAX(size), -1), MIN(storage_path) FROM attachments WHERE "+testContentBearing+
			" GROUP BY content_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("testapp: attachment locator query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []ContentRef
	seen := map[string]bool{}
	for rows.Next() {
		var ref ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("testapp: scanning attachment locator: %w", err)
		}
		refs = append(refs, ref)
		seen[ref.Hash] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: attachment locator rows: %w", err)
	}

	thumbRows, err := v.tx.QueryContext(ctx,
		"SELECT thumbnail_hash, MIN(thumbnail_path) FROM attachments WHERE "+testThumbBearing+
			" GROUP BY thumbnail_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("testapp: thumbnail locator query: %w", err)
	}
	defer func() { _ = thumbRows.Close() }()
	for thumbRows.Next() {
		var ref ContentRef
		if err := thumbRows.Scan(&ref.Hash, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("testapp: scanning thumbnail locator: %w", err)
		}
		if !seen[ref.Hash] {
			ref.Size = -1
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	if err := thumbRows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: thumbnail locator rows: %w", err)
	}

	var rowCount int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM attachments").Scan(&rowCount); err != nil {
		return nil, fmt.Errorf("testapp: attachment row count query: %w", err)
	}

	var nonCanonical bool
	err = v.tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM attachments WHERE `+testContentBearing+`
		  AND storage_path != substr(content_hash, 1, 2) || '/' || content_hash
		UNION ALL
		SELECT 1 FROM attachments WHERE `+testThumbBearing+`
		  AND thumbnail_path != substr(thumbnail_hash, 1, 2) || '/' || thumbnail_hash
	)`).Scan(&nonCanonical)
	if err != nil {
		return nil, fmt.Errorf("testapp: attachment path canonicality query: %w", err)
	}

	return &ContentInfo{Refs: refs, Rows: rowCount, NonCanonicalPaths: nonCanonical}, nil
}

func parseTestStats(raw json.RawMessage) (testStats, error) {
	var st testStats
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("testapp: parsing manifest stats: %w", err)
	}
	return st, nil
}

// mustParseStats decodes a manifest's stats payload for assertions.
func mustParseStats(t *testing.T, raw json.RawMessage) testStats {
	t.Helper()
	st, err := parseTestStats(raw)
	require.NoError(t, err)
	return st
}
