package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(st.InitSchemaContext(ctx), context.Canceled)
	assert.Equal(1, metadataTrigramCount(t, st, "messages_metadata_fts", "eedle"))
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
