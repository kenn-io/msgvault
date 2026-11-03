package meta

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportReplayKeepsAnalyticsCacheCurrent(t *testing.T) {
	const updatedBody = "cacheafter"
	const updatedActor = "Updated Example Actor"
	const fromSnapshotSQL = `SELECT display_name FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "cachebefore")}
	replay := func() {
		before := f.calls
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.NoError(err)
		assert.False(sum.Partial)
		assert.Equal(2, sum.MessagesProcessed)
		assert.Greater(f.calls, before, "the unchanged replay must actually read the provider")
	}
	clock := func() int64 {
		var seq int64
		require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&seq))
		return seq
	}
	factsSince := func(seq int64) int {
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, seq).Scan(&count))
		return count
	}
	replay()
	child := message(t, st, src.ID, "100_201")
	baseline := clock()
	replay()
	assert.Zero(factsSince(baseline), "unchanged snapshots must not invalidate the analytics publication")
	f.comments["100_200"][0].Message = updatedBody
	f.comments["100_200"][0].From.Name = updatedActor
	replay()
	body, err := st.GetMessageBodyText(child.ID)
	require.NoError(err)
	assert.Equal(updatedBody, body)
	var fromName string
	require.NoError(st.DB().QueryRow(fromSnapshotSQL, child.ID).Scan(&fromName))
	assert.Equal(updatedActor, fromName)
	assert.Positive(factsSince(baseline), "real provider changes must still invalidate cached facts")
	for _, corruption := range []string{
		`UPDATE message_bodies SET body_text = 'corrupted' WHERE message_id = ?`,
		`DELETE FROM messages_fts WHERE message_id = ?`,
		`UPDATE message_recipients SET display_name = 'corrupted' WHERE message_id = ?`,
	} {
		_, err := st.DB().Exec(corruption, child.ID)
		require.NoError(err)
		replay()
		body, err = st.GetMessageBodyText(child.ID)
		require.NoError(err)
		assert.Equal(updatedBody, body)
		require.NoError(st.DB().QueryRow(fromSnapshotSQL, child.ID).Scan(&fromName))
		assert.Equal(updatedActor, fromName)
		var hits int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE message_id = ? AND messages_fts MATCH 'cacheafter'`, child.ID).Scan(&hits))
		assert.Equal(1, hits, "unchanged raw evidence cannot hide damaged body/search/recipient derivatives")
	}
	require.NoError(st.UpsertMessageRawWithFormat(child.ID, []byte(`{"id":"100_201","message":"wrong-evidence"}`), rawFormat))
	replay()
	stored, err := st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{"100_201"})
	require.NoError(err)
	expectedRaw, err := json.Marshal(f.comments["100_200"][0])
	require.NoError(err)
	assert.JSONEq(string(expectedRaw), string(stored["100_201"].Raw))
	baseline = clock()
	replay()
	assert.Zero(factsSince(baseline), "repaired data must settle back to a quiet replay")
	f.comments["100_200"][0].From = nil
	replay()
	require.NoError(st.DB().QueryRow(fromSnapshotSQL, child.ID).Scan(&fromName))
	assert.Equal(updatedActor, fromName, "an omitted provider author must retain its known snapshot without requiring email")
	var indexedName string
	require.NoError(st.DB().QueryRow(`SELECT from_addr FROM messages_fts WHERE message_id = ?`, child.ID).Scan(&indexedName))
	assert.Equal(updatedActor, indexedName)
	baseline = clock()
	replay()
	assert.Zero(factsSince(baseline))
}

func TestImportReplayPreservesGenericStoreReplyEdit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "provider child"), *comment("100_202", "100_200", "generic reply target")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	child, target := message(t, st, src.ID, "100_201"), message(t, st, src.ID, "100_202")
	before, err := st.GetMessageMetadata(child.ID)
	require.NoError(err)
	require.NoError(st.SetMessageReplyContext(t.Context(), child.ID, target.ID))
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	after := message(t, st, src.ID, "100_201")
	assert.Equal(target.ID, after.ReplyToMessageID.Int64)
	metadata, err := st.GetMessageMetadata(child.ID)
	require.NoError(err)
	assert.JSONEq(before.String, metadata.String, "generic edge edit must not reparent immutable provider metadata")
}
