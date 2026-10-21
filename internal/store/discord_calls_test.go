package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDiscordCallArchiveRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "example-guild")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "200", "Example")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "100"})
	require.NoError(err)
	const raw = `{"id":"100","channel_id":"200","type":3,"timestamp":"2026-10-01T00:00:00Z"}`
	require.NoError(st.UpsertMessageRawWithFormat(id, []byte(raw), "discord_json"))
	got, err := st.GetDiscordCallRecordContext(t.Context(), id)
	require.NoError(err)
	assert.JSONEq(raw, string(got))
	for _, mutation := range []struct {
		query string
		args  []any
	}{
		{`UPDATE message_raw SET raw_format = ? WHERE message_id = ?`, []any{"mime", id}},
		{`UPDATE sources SET source_type = ? WHERE id = ?`, []any{"slack", source.ID}},
		{`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, []any{id}},
		{`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, []any{id}},
	} {
		_, err := st.DB().Exec(st.Rebind(mutation.query), mutation.args...)
		require.NoError(err)
		_, err = st.GetDiscordCallRecordContext(t.Context(), id)
		require.ErrorIs(err, callhistory.ErrInvalid)
		_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = NULL, deleted_from_source_at = NULL WHERE id = ?`), id)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET source_type = 'discord' WHERE id = ?`), source.ID)
		require.NoError(err)
		require.NoError(st.UpsertMessageRawWithFormat(id, []byte(raw), "discord_json"))
	}
	for _, missing := range []int64{0, -1, id + 1} {
		_, err := st.GetDiscordCallRecordContext(t.Context(), missing)
		require.ErrorIs(err, callhistory.ErrInvalid)
	}
	// Upsert compresses this payload, so the expanded cap must also be enforced.
	require.NoError(st.UpsertMessageRawWithFormat(id, []byte(strings.Repeat("x", callhistory.MaxRawBytes+1)), "discord_json"))
	_, err = st.GetDiscordCallRecordContext(t.Context(), id)
	require.ErrorIs(err, callhistory.ErrImportTooLarge)
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`), []byte(strings.Repeat("x", callhistory.MaxRawBytes+1)), id)
	require.NoError(err)
	_, err = st.GetDiscordCallRecordContext(t.Context(), id)
	require.ErrorIs(err, callhistory.ErrImportTooLarge)
	_, err = st.DB().Exec(st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`), []byte("broken"), id)
	require.NoError(err)
	_, err = st.GetDiscordCallRecordContext(t.Context(), id)
	require.ErrorIs(err, callhistory.ErrInvalid)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.GetDiscordCallRecordContext(ctx, id)
	require.ErrorIs(err, context.Canceled)
}
