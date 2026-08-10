package beeper

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestRepairArchiveRewritesStaleDerivedRows covers repairing an archive
// written before HTML was converted and before shares were classified: the
// pass must reach both from the stored payload alone, without contacting
// Beeper Desktop.
func TestRepairArchiveRewritesStaleDerivedRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	ch := shareAndHTMLChat()
	f.addChat(ch)
	f.setAsset("mxc://x/share1", []byte("share-preview-bytes"))
	f.setAsset("mxc://x/photo1", []byte("photo-bytes"))

	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.NoError(err)

	// Simulate rows written by an older build: raw HTML in the body and no
	// share classification on the attachments.
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_bodies SET body_text = ?
		WHERE message_id = (SELECT id FROM messages WHERE source_message_id = 'html1')`),
		`<p>hello <a href="https://example.com" rel="noopener noreferrer">there</a></p>`)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE attachments SET attachment_metadata = NULL`)
	require.NoError(err)

	sum, err := imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Zero(sum.Undecodable)
	assert.Equal(int64(1), sum.BodiesRewritten, "only the stale body needs rewriting")

	var body, snippet string
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, COALESCE(m.snippet, '')
		FROM messages m JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_message_id = 'html1'`).Scan(&body, &snippet))
	assert.Equal("hello there", body, "HTML must be converted to plain text")
	assert.Equal("hello there", snippet)

	// The forwarded link keeps its share URL; the photo stays unclassified.
	var shareMeta, photoMeta string
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(CAST(a.attachment_metadata AS TEXT), '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = 'share1'`).Scan(&shareMeta))
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(CAST(a.attachment_metadata AS TEXT), '')
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = 'photo1'`).Scan(&photoMeta))
	assert.JSONEq(`{"shared_url":"https://www.instagram.com/p/ABC/"}`, shareMeta)
	assert.Empty(photoMeta, "media the sender composed is not a share")

	// Re-running must be a no-op: nothing left differing from the archive.
	again, err := imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)
	assert.Zero(again.BodiesRewritten)
	assert.Zero(again.AttachmentsTagged)
	assert.Equal(sum.MessagesScanned, again.MessagesScanned)
}

func TestRepairArchiveRollsBackDerivedTextTogether(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injects a failure after the body write")
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(shareAndHTMLChat())
	f.setAsset("mxc://x/share1", []byte("share-preview-bytes"))
	f.setAsset("mxc://x/photo1", []byte("photo-bytes"))

	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.NoError(err)

	const stale = `<p>hello <a href="https://example.com">there</a></p>`
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_bodies SET body_text = ?
		WHERE message_id = (SELECT id FROM messages WHERE source_message_id = 'html1')`), stale)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_repair_snippet
		BEFORE UPDATE OF snippet ON messages
		WHEN OLD.source_message_id = 'html1'
		BEGIN SELECT RAISE(ABORT, 'injected snippet failure'); END`)
	require.NoError(err)

	sum, err := imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors)

	var body string
	require.NoError(st.DB().QueryRow(`SELECT b.body_text FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_message_id = 'html1'`).Scan(&body))
	assert.Equal(stale, body, "a later derived-field failure must roll back the body write")

	_, err = st.DB().Exec(`DROP TRIGGER fail_repair_snippet`)
	require.NoError(err)
	sum, err = imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.BodiesRewritten)
	require.NoError(st.DB().QueryRow(`SELECT b.body_text FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_message_id = 'html1'`).Scan(&body))
	assert.Equal("hello there", body)
}

func TestRepairArchiveRefreshesSnippetAndFTSWhenBodyIsCurrent(t *testing.T) {
	testutil.SkipIfPostgres(t, "directly corrupts the SQLite FTS5 table")
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(shareAndHTMLChat())
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	require.True(st.FTS5Available(), "FTS5 must be available for the repair regression")

	var messageID int64
	require.NoError(st.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = 'html1'`).Scan(&messageID))
	_, err = st.DB().Exec(`UPDATE messages SET snippet = 'stale snippet' WHERE id = ?`, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, messageID)
	require.NoError(err)

	sum, err := imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Zero(sum.BodiesRewritten, "the body was already current")

	var body, snippet string
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, COALESCE(m.snippet, '')
		FROM messages m JOIN message_bodies b ON b.message_id = m.id
		WHERE m.id = ?`, messageID).Scan(&body, &snippet))
	assert.Equal("hello there", body)
	assert.Equal("hello there", snippet, "repair must refresh a stale snippet independently of body drift")

	var ftsHits int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND messages_fts MATCH 'hello'`, messageID).Scan(&ftsHits))
	assert.Equal(1, ftsHits, "repair must restore a missing FTS row independently of body drift")
}

func TestSyncReportsIncompleteRepair(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite trigger injects a row-level metadata failure")
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(shareAndHTMLChat())
	f.setAsset("mxc://x/share1", []byte("share-preview-bytes"))
	f.setAsset("mxc://x/photo1", []byte("photo-bytes"))

	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name LIKE 'rederive:%'`)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE attachments SET attachment_metadata = NULL`)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_repair_metadata
		BEFORE UPDATE OF attachment_metadata ON attachments
		BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors, "row-level repair failures must reach the sync summary")
}

// TestRepairArchiveLeavesStoredMediaAlone covers the pass rewriting only
// derived columns: a repair must never disturb downloaded blobs.
func TestRepairArchiveLeavesStoredMediaAlone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(shareAndHTMLChat())
	f.setAsset("mxc://x/share1", []byte("share-preview-bytes"))
	f.setAsset("mxc://x/photo1", []byte("photo-bytes"))

	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.NoError(err)

	before := storedBlobs(t, st)
	require.NotEmpty(before)

	_, err = imp.RepairSource(context.Background(), beeperSourceID(t, st), nil)
	require.NoError(err)

	assert.Equal(before, storedBlobs(t, st), "repair must not touch stored blobs")
}

// storedBlobs maps each attachment to the blob it points at, so a test can
// assert that a pass left stored media untouched.
func storedBlobs(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	require := require.New(t)
	rows, err := st.DB().Query(
		`SELECT source_attachment_id, COALESCE(content_hash, '') || '|' || storage_path FROM attachments`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		require.NoError(rows.Scan(&k, &v))
		out[k] = v
	}
	require.NoError(rows.Err())
	return out
}

// shareAndHTMLChat builds a chat holding one HTML message, one forwarded link
// preview, and one photo the sender composed.
func shareAndHTMLChat() *fakeChat {
	// Older than the reconcile window so head re-walks terminate immediately.
	base := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(time.Second)
	return &fakeChat{
		ID: "!repair:beeper.local", AccountID: "signal", Network: "Signal",
		Title: "Repair", Type: "single", LastActivity: base.Add(2 * time.Minute),
		Participants: []map[string]any{{"id": "@me:beeper.local", "isSelf": true}},
		Msgs: []fakeMsg{
			{
				ID: "html1", SortKey: 1, Timestamp: base,
				Text:     `<p>hello <a href="https://example.com" rel="noopener noreferrer">there</a></p>`,
				SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
			},
			{
				ID: "share1", SortKey: 2, Timestamp: base.Add(time.Minute), Type: typeImage,
				Text:     `<a href="https://www.instagram.com/p/ABC/" rel="noopener noreferrer">https://www.instagram.com/p/ABC/</a>`,
				SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
				Attachments: []map[string]any{{"id": "mxc://x/share1", "type": "img", "mimeType": "image/jpeg"}},
			},
			{
				ID: "photo1", SortKey: 3, Timestamp: base.Add(2 * time.Minute), Type: typeImage,
				SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
				Attachments: []map[string]any{{"id": "mxc://x/photo1", "type": "img", "mimeType": "image/jpeg"}},
			},
		},
	}
}

// TestSyncHealsRowsFromAnOlderBuild covers an upgraded archive converging on
// its own: the next sync re-derives rows written before the derivation changed,
// without the user knowing a repair exists. The ledger then keeps later syncs
// from repeating the work.
func TestSyncHealsRowsFromAnOlderBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(shareAndHTMLChat())
	f.setAsset("mxc://x/share1", []byte("share-preview-bytes"))
	f.setAsset("mxc://x/photo1", []byte("photo-bytes"))

	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.NoError(err)

	// Put the archive back into the state an older build would have left, and
	// clear the ledger so the source looks un-repaired.
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_bodies SET body_text = ?
		WHERE message_id = (SELECT id FROM messages WHERE source_message_id = 'html1')`),
		`<p>hello <a href="https://example.com" rel="noopener noreferrer">there</a></p>`)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name LIKE 'rederive:%'`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.Equal(int64(1), sum.BodiesRepaired, "the sync must heal the stale row")

	var body string
	require.NoError(st.DB().QueryRow(`SELECT b.body_text FROM messages m
		JOIN message_bodies b ON b.message_id = m.id
		WHERE m.source_message_id = 'html1'`).Scan(&body))
	assert.Equal("hello there", body)

	// The ledger now records the pass, so the next sync must not redo it.
	next, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.Zero(next.BodiesRepaired, "a healed archive must not re-derive on every sync")
}

func TestSyncRunsCurrentRepairAfterV2WasApplied(t *testing.T) {
	testutil.SkipIfPostgres(t, "directly corrupts the SQLite FTS5 table")
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	ch := shareAndHTMLChat()
	ch.Msgs[0].Text = `<mx-reply><blockquote>quoted parent text</blockquote></mx-reply><p>fresh reply</p>`
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	defer done()

	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	require.True(st.FTS5Available(), "FTS5 must be available for the upgrade regression")

	var messageID int64
	require.NoError(st.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = 'html1'`).Scan(&messageID))
	const oldDerived = "quoted parent text\n\nfresh reply"
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), oldDerived, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET snippet = ? WHERE id = ?`), oldDerived, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM messages_fts WHERE rowid = ?`, messageID)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name LIKE 'rederive:beeper:signal:%'`)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO applied_migrations (name) VALUES ('rederive:beeper:signal:v2')`)
	require.NoError(err)

	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)

	var body, snippet string
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, COALESCE(m.snippet, '')
		FROM messages m JOIN message_bodies b ON b.message_id = m.id
		WHERE m.id = ?`, messageID).Scan(&body, &snippet))
	assert.Equal("fresh reply", body, "the current repair version must run after v2")
	assert.Equal("fresh reply", snippet)
	var ftsHits int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND messages_fts MATCH 'fresh'`, messageID).Scan(&ftsHits))
	assert.Equal(1, ftsHits, "the current repair version must restore FTS after v2")
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND messages_fts MATCH 'quoted'`, messageID).Scan(&ftsHits))
	assert.Zero(ftsHits, "the parent fallback must not remain searchable as reply text")
}

// beeperSourceID returns the archive's single Beeper source row.
func beeperSourceID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(
		`SELECT id FROM sources WHERE source_type = 'beeper'`).Scan(&id))
	return id
}
