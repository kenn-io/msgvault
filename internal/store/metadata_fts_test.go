package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

func metadataTrigramStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenForTest(filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = st.DB().Exec(string(schema))
	require.NoError(t, err)
	_, err = st.DB().Exec(`
 INSERT INTO sources(id,source_type,identifier) VALUES(1,'gmail','archive@example.org');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,1,'synthetic-thread','email_thread');
 INSERT INTO participants(id,email_address,display_name,phone_number) VALUES(1,'sender@example.org','ÉCOLE Contact','+15550001111');
 INSERT INTO messages(id,conversation_id,source_id,message_type,source_message_id,subject,snippet,sender_id) VALUES(1,1,1,'email','synthetic-1','prefix Needle suffix','preview',1);
 INSERT INTO message_recipients(id,message_id,participant_id,recipient_type,display_name) VALUES(1,1,1,'to','Occurrence Alias');
 `)
	require.NoError(t, err)
	return st
}

func metadataTrigramCount(t *testing.T, st *Store, table, term string) int {
	t.Helper()
	var count int
	arg := `"` + strings.ReplaceAll(strings.ToLower(term), `"`, `""`) + `"`
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+table+" MATCH ?", arg).Scan(&count))
	return count
}

func TestMetadataTrigramLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := metadataTrigramStore(t)
	require.NoError(st.InitSchema())
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "eedle"))
	assert.Equal(1, metadataTrigramCount(t, st, "participants_metadata_fts", "école"))
	assert.Equal(1, metadataTrigramCount(t, st, "participants_metadata_fts", "00011"))
	assert.Equal(1, metadataTrigramCount(t, st, "recipients_metadata_fts", "rrence"))
	require.NoError(st.InitSchema(), "reopen does not duplicate index entries")
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "eedle"))
	_, err := st.DB().Exec(`UPDATE messages SET subject=NULL,snippet='other-marker' WHERE id=1;
 UPDATE participants SET display_name='Renamed Contact' WHERE id=1;
 UPDATE message_recipients SET display_name='Replaced Alias' WHERE id=1;`)
	require.NoError(err)
	assert.Zero(metadataTrigramCount(t, st, "messages_metadata_fts", "eedle"))
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "her-mark"))
	assert.Zero(metadataTrigramCount(t, st, "participants_metadata_fts", "école"))
	assert.Equal(1, metadataTrigramCount(t, st, "participants_metadata_fts", "named"))
	assert.Zero(metadataTrigramCount(t, st, "recipients_metadata_fts", "rrence"))
	assert.Equal(1, metadataTrigramCount(t, st, "recipients_metadata_fts", "placed"))
	_, err = st.DB().Exec(`INSERT INTO messages(id,conversation_id,source_id,message_type,source_message_id,subject) VALUES(2,1,1,'email','synthetic-2','inserted needle');
 INSERT INTO participants(id,display_name) VALUES(2,'Inserted Person');
 INSERT INTO message_recipients(id,message_id,participant_id,recipient_type,display_name) VALUES(2,2,2,'to','Inserted Alias');`)
	require.NoError(err)
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "needle"))
	assert.Equal(1, metadataTrigramCount(t, st, "participants_metadata_fts", "inserted"))
	assert.Equal(1, metadataTrigramCount(t, st, "recipients_metadata_fts", "inserted"))
	_, err = st.DB().Exec(`DELETE FROM messages WHERE id=2; DELETE FROM participants WHERE id=2;`)
	require.NoError(err)
	assert.Zero(metadataTrigramCount(t, st, "messages_metadata_fts", "needle"))
	assert.Zero(metadataTrigramCount(t, st, "participants_metadata_fts", "inserted"))
	assert.Zero(metadataTrigramCount(t, st, "recipients_metadata_fts", "inserted"), "foreign key cascade deletes alias index")
}

func TestMetadataTrigramRepairsMissingIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := metadataTrigramStore(t)
	require.NoError(st.InitSchema())
	_, err := st.DB().Exec(`DROP TABLE recipients_metadata_fts`)
	require.NoError(err)
	require.NoError(st.InitSchema())
	assert.Equal(1, metadataTrigramCount(t, st, "recipients_metadata_fts", "rrence"))
}

func TestMetadataTrigramCapabilityTransition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := metadataTrigramStore(t)
	require.NoError(st.InitSchema())
	require.NoError(st.ensureMetadataFTS(t.Context(), false))
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key='metadata_fts_version'`).Scan(&count))
	assert.Zero(count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE '%_metadata_fts_%'`).Scan(&count))
	assert.Zero(count)
	_, err := st.DB().Exec(`UPDATE messages SET subject='offline changed' WHERE id=1;
 UPDATE participants SET display_name='Offline Person' WHERE id=1;
 UPDATE message_recipients SET display_name='Offline Alias' WHERE id=1;`)
	require.NoError(err)
	require.NoError(st.ensureMetadataFTS(t.Context(), true))
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "offline"))
	assert.Equal(1, metadataTrigramCount(t, st, "participants_metadata_fts", "offline"))
	assert.Equal(1, metadataTrigramCount(t, st, "recipients_metadata_fts", "offline"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(st.ensureMetadataFTS(ctx, true), context.Canceled)
}

// FTS5 deletes rowid ranges from _data on nearly every trigger-maintained
// insert. A plan that scans _data makes ingest time grow with index size.
func TestFTSDataRangeDeletesKeepRowidPlan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "msgvault.db")
	plans := func(st *Store, phase string) {
		for _, table := range []string{"messages_fts_data", "messages_metadata_fts_data"} {
			var id, parent, unused int
			var detail string
			require.NoError(st.DB().QueryRow(
				"EXPLAIN QUERY PLAN SELECT block FROM "+table+" WHERE id >= ? AND id <= ?", 1, 2,
			).Scan(&id, &parent, &unused, &detail))
			assert.Contains(detail, "USING INTEGER PRIMARY KEY", table+" after "+phase)
		}
	}
	st, err := OpenForTest(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	plans(st, "schema initialization")
	require.NoError(st.Close())
	st, err = OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	plans(st, "close-time statistics refresh")
	require.NoError(st.optimizeSQLite(t.Context()))
	plans(st, "scheduled statistics refresh")
}

// FTS5 reads its configuration the first time a connection uses an index.
// Inside a deferred transaction that read would pin a snapshot, and a commit
// from another connection before the write would make the write fail with
// SQLITE_BUSY_SNAPSHOT. New connections load the indexes before any
// transaction starts.
func TestMetadataTriggerWriteAfterConcurrentCommit(t *testing.T) {
	require := require.New(t)
	st := metadataTrigramStore(t)
	require.NoError(st.InitSchema())
	db, err := sql.Open(sqliteutil.DriverName(), st.dbPath+"?_busy_timeout=5000")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	conn, err := db.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(conn.Close()) })

	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	update, err := tx.PrepareContext(t.Context(), `UPDATE messages SET subject = ? WHERE id = 1`)
	require.NoError(err)
	defer func() { require.NoError(update.Close()) }()
	_, err = st.DB().Exec(`UPDATE participants SET display_name = 'Concurrent Writer' WHERE id = 1`)
	require.NoError(err)
	_, err = update.ExecContext(t.Context(), "written after a concurrent commit")
	require.NoError(err)
	require.NoError(tx.Commit())
}
