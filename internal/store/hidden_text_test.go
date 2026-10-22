package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestUpsertMessageBodyHiddenTextPresent(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("hidden-html")
	for _, tc := range []struct {
		name   string
		html   sql.NullString
		hidden bool
	}{
		{"hidden", sql.NullString{String: `<div hidden>Hidden text</div>`, Valid: true}, true},
		{"visible", sql.NullString{String: `<p>Visible text</p>`, Valid: true}, false},
		{"absent", sql.NullString{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "Archived text", Valid: true}, tc.html))
			var hidden bool
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
				`SELECT hidden_text_present FROM messages WHERE id = ?`), id).Scan(&hidden))
			assert.Equal(tc.hidden, hidden)
		})
	}
}

func TestInitSchemaBackfillsHiddenHTMLText(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	const migration = "messages_hidden_text_backfill_v1"
	ids := make([]int64, 0, 4)
	for _, html := range []string{`<div hidden>Hidden text</div>`, `<p>Visible</p>`, ""} {
		id := f.CreateMessage(html)
		ids = append(ids, id)
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (?, ?, ?)`), id, "Archived text", html)
		require.NoError(err)
	}
	ids = append(ids, f.CreateMessage("bodyless"))
	if !f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().Exec(`
			DROP TRIGGER trg_cache_message_facts_update;
			CREATE TRIGGER trg_cache_message_facts_update
			AFTER UPDATE OF sender_id, is_from_me, source_is_from_me,
				has_attachments, attachment_count, account_address, account_path
				ON messages FOR EACH ROW
			WHEN OLD.sender_id IS NOT NEW.sender_id OR OLD.is_from_me IS NOT NEW.is_from_me
				OR OLD.source_is_from_me IS NOT NEW.source_is_from_me
				OR OLD.has_attachments IS NOT NEW.has_attachments
				OR OLD.attachment_count IS NOT NEW.attachment_count
				OR OLD.account_address IS NOT NEW.account_address
				OR OLD.account_path IS NOT NEW.account_path BEGIN
				INSERT INTO cache_related_change_journal (dataset, message_id)
				VALUES ('message_facts', NEW.id);
			END;
			UPDATE applied_migrations SET version = 2 WHERE name = 'cache_message_source_attribution';
		`)
		require.NoError(err)
	}
	_, err := f.Store.DB().Exec(`ALTER TABLE messages DROP COLUMN hidden_text_present`)
	require.NoError(err, "simulate an archive before the flag column")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migration)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`UPDATE archive_metadata SET value = '10' WHERE key = 'schema_version'`)
	require.NoError(err)
	var before string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), ids[0]).Scan(&before))

	require.NoError(f.Store.InitSchemaContext(t.Context()))
	for i, id := range ids {
		var hidden bool
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
			`SELECT hidden_text_present FROM messages WHERE id = ?`), id).Scan(&hidden))
		assert.Equal(i == 0, hidden)
	}
	var after string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT CAST(content_changed_at AS TEXT) FROM messages WHERE id = ?`), ids[0]).Scan(&after))
	assert.Equal(before, after, "backfill must preserve the content watermark")
	applied, err := f.Store.IsMigrationApplied(migration)
	require.NoError(err)
	assert.True(applied)
	if !f.Store.IsPostgreSQL() {
		var baseline int64
		require.NoError(f.Store.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
		_, err := f.Store.DB().Exec(`UPDATE messages SET hidden_text_present = 0 WHERE id = ?`, ids[0])
		require.NoError(err)
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
			WHERE seq > ? AND dataset = 'message_facts' AND message_id = ?`, baseline, ids[0]).Scan(&count))
		assert.Equal(1, count)
	}

	// An older writer can add unflagged HTML; the ledger must still prevent a rescan.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_bodies SET body_html = ? WHERE message_id = ?`),
		`<div hidden>Later HTML</div>`, ids[1])
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	var hidden bool
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT hidden_text_present FROM messages WHERE id = ?`), ids[1]).Scan(&hidden))
	assert.False(hidden, "completed backfill runs once")
}

func TestUpdateMessageDerivedTextHiddenTextPresent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("derived-hidden-html")
	require.NoError(f.Store.UpdateMessageDerivedText(id,
		sql.NullString{String: "Hidden text", Valid: true},
		sql.NullString{String: `<div hidden>Hidden text</div>`, Valid: true},
		sql.NullString{String: "Hidden text", Valid: true}, store.FTSDoc{Body: "Hidden text"}))
	msg, err := f.Store.GetMessage(id)
	require.NoError(err)
	assert.True(msg.HiddenTextPresent)
}
