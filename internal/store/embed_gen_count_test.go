package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetEmbedGenReturnsCommittedPrefixOnChunkError(t *testing.T) {
	previousChunkRows := embedGenStampChunkRows
	embedGenStampChunkRows = 1
	t.Cleanup(func() { embedGenStampChunkRows = previousChunkRows })

	st, first, second := seedEmbedGenCountMessages(t)
	installEmbedGenFailureTrigger(t, st, second)

	committed, err := st.SetEmbedGen(context.Background(), []int64{first, second}, 7)
	require.Error(t, err)
	assert.Equal(t, 1, committed)
	assertEmbedGenPrefix(t, st, first, second, 7)
}

func TestSetEmbedGenIfUnchangedReturnsCommittedPrefixOnRowError(t *testing.T) {
	st, first, second := seedEmbedGenCountMessages(t)
	installEmbedGenFailureTrigger(t, st, second)
	readToken := func(id int64) string {
		var token string
		require.NoError(t, st.DB().QueryRow(
			`SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?`, id).Scan(&token))
		return token
	}

	missed, committed, err := st.SetEmbedGenIfUnchanged(context.Background(), []EmbedGenStamp{
		{ID: first, LastModified: readToken(first)},
		{ID: second, LastModified: readToken(second)},
	}, 7)
	require.Error(t, err)
	assert.Empty(t, missed)
	assert.Equal(t, 1, committed)
	assertEmbedGenPrefix(t, st, first, second, 7)
}

func seedEmbedGenCountMessages(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := openTestStore(t)
	source, err := st.GetOrCreateSource("test", "stamp-prefix@example.test")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "stamp-prefix", "email_thread", "Subject")
	require.NoError(t, err)
	first := seedMessage(t, st, source.ID, conversationID, "first", "subject", "")
	second := seedMessage(t, st, source.ID, conversationID, "second", "subject", "")
	return st, first, second
}

func installEmbedGenFailureTrigger(t *testing.T, st *Store, failID int64) {
	t.Helper()
	_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_second_embed_stamp
		BEFORE UPDATE OF embed_gen ON messages WHEN OLD.id = %d
		BEGIN SELECT RAISE(ABORT, 'synthetic stamp failure'); END`, failID))
	require.NoError(t, err)
}

func assertEmbedGenPrefix(t *testing.T, st *Store, first, second, target int64) {
	t.Helper()
	var firstGeneration, secondGeneration sql.NullInt64
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, first).Scan(&firstGeneration))
	require.NoError(t, st.DB().QueryRow(`SELECT embed_gen FROM messages WHERE id = ?`, second).Scan(&secondGeneration))
	assert.Equal(t, target, firstGeneration.Int64)
	assert.True(t, firstGeneration.Valid)
	assert.False(t, secondGeneration.Valid)
}
