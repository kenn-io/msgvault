package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func archivedEmailFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	id, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "archived", MessageType: "email", Subject: sql.NullString{String: "Stored subject", Valid: true}, Snippet: sql.NullString{String: "Heizkörper", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}))
	require.NoError(t, f.Store.UpsertMessageRaw(id, []byte("Content-Type: text/plain; charset=UTF-8\r\n\r\nHeizkörper")))
	require.NoError(t, f.Store.UpsertFTS(id, "Stored subject", "Heizkörper", "", "", ""))
	return f, id
}

func TestUpdateArchivedEmailText(t *testing.T) {
	body := sql.NullString{String: "Heizkörper", Valid: true}
	html := sql.NullString{String: "<p>Heizkörper</p>", Valid: true}
	t.Run("current text leaves the row unchanged", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, id := archivedEmailFixture(t)
		modified := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET last_modified = ? WHERE id = ?"), modified, id)
		require.NoError(err)
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		changed, err := f.Store.UpdateArchivedEmailTextContext(t.Context(), item, body, html, body)
		require.NoError(err)
		assert.False(changed)
		var after time.Time
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT last_modified FROM messages WHERE id = ?"), id).Scan(&after))
		assert.Equal(modified, after.UTC())
	})
	t.Run("stale search is refreshed", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, id := archivedEmailFixture(t)
		require.NoError(f.Store.UpsertFTS(id, "Stored subject", "stale search", "", "", ""))
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		changed, err := f.Store.UpdateArchivedEmailTextContext(t.Context(), item, body, html, body)
		require.NoError(err)
		assert.True(changed)
		_, found, err := f.Store.SearchMessages("Heizkörper", 0, 10)
		require.NoError(err)
		assert.Equal(int64(1), found)
	})
	t.Run("mismatched HTML is rewritten", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, id := archivedEmailFixture(t)
		require.NoError(f.Store.UpsertMessageBody(id, body, sql.NullString{String: "wrong HTML", Valid: true}))
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		changed, err := f.Store.UpdateArchivedEmailTextContext(t.Context(), item, body, html, body)
		require.NoError(err)
		assert.True(changed)
		var got string
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&got))
		assert.Equal("<p>Heizkörper</p>", got)
	})
	t.Run("failed snippet rolls back the body", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f, id := archivedEmailFixture(t)
		require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "stale", Valid: true}, html))
		query := fmt.Sprintf(`CREATE TRIGGER fail_email_snippet BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'snippet failure'); END`, id)
		if f.Store.IsPostgreSQL() {
			_, err := f.Store.DB().Exec(`CREATE FUNCTION fail_email_snippet() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'snippet failure'; END $$`)
			require.NoError(err)
			query = `CREATE TRIGGER fail_email_snippet BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_email_snippet()`
		}
		_, err := f.Store.DB().Exec(query)
		require.NoError(err)
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		_, err = f.Store.UpdateArchivedEmailTextContext(t.Context(), item, body, html, body)
		require.Error(err)
		got, err := f.Store.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("stale", got)
	})
	t.Run("large multibyte email preserves subject search", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		skipUnlessPostgres(t)
		f, id := archivedEmailFixture(t)
		// This existing fixture exceeds the byte cap. A character-only repair
		// cap overflowed its vector and erased otherwise searchable subjects.
		large := sql.NullString{String: multibyteOversizedBody(), Valid: true}
		require.NoError(f.Store.UpsertMessageRaw(id, []byte("Content-Type: text/plain; charset=UTF-8\r\n\r\n"+large.String)))
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		changed, err := f.Store.UpdateArchivedEmailTextContext(t.Context(), item, large, sql.NullString{}, body)
		require.NoError(err)
		assert.True(changed)
		_, found, err := f.Store.SearchMessages("Stored", 0, 10)
		require.NoError(err)
		assert.Equal(int64(1), found)
	})
	t.Run("PostgreSQL overflow remains terminal", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		skipUnlessPostgres(t)
		f, id := archivedEmailFixture(t)
		_, err := f.Store.DB().Exec(`CREATE FUNCTION fail_email_fts() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.search_fts <> ''::tsvector THEN RAISE EXCEPTION USING ERRCODE = '54000', MESSAGE = 'synthetic tsvector overflow'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_email_fts BEFORE UPDATE OF search_fts ON messages FOR EACH ROW EXECUTE FUNCTION fail_email_fts()`)
		require.NoError(err)
		item, err := f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		updated := sql.NullString{String: "changed", Valid: true}
		_, err = f.Store.UpdateArchivedEmailTextContext(t.Context(), item, updated, html, body)
		require.NoError(err)
		got, err := f.Store.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("changed", got)
		var terminal bool
		require.NoError(f.Store.DB().QueryRow("SELECT search_fts = ''::tsvector AND indexing_version = $1 FROM messages WHERE id = $2", store.CurrentFTSIndexingVersion, id).Scan(&terminal))
		assert.True(terminal)
		item, err = f.Store.ScanArchivedEmailContext(t.Context(), f.Source.ID, id-1, id)
		require.NoError(err)
		changed, err := f.Store.UpdateArchivedEmailTextContext(t.Context(), item, updated, html, body)
		require.NoError(err)
		assert.False(changed)
	})
}
