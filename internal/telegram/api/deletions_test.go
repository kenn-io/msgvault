package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCommonDeletionIsAppliedBeforeCheckpointAndReplayed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	for _, row := range []struct{ peer, kind, key string }{{"456", "direct_chat", "456:7"}, {"-1000000000123", "group_chat", "-1000000000123:7"}} {
		conv, err := st.EnsureConversationWithType(source.ID, row.peer, row.kind, "Synthetic")
		require.NoError(err)
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram"}, BodyText: sql.NullString{String: "synthetic", Valid: true}})
		require.NoError(err)
	}
	previous := telegram.CommonUpdateState{Pts: 10, Date: 100}
	next := telegram.CommonUpdateState{Pts: 11, Date: 101}
	response := &tg.UpdatesDifference{OtherUpdates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{7}, Pts: 11, PtsCount: 1}}}
	page := CommonDifferencePage{Response: response, Previous: previous, Next: &next}
	saveErr := errors.New("synthetic checkpoint failure")
	apply := func(ctx context.Context, _ tg.UpdatesDifferenceClass) error {
		return ApplyCommonDeletions(ctx, st, source.ID, response.OtherUpdates)
	}
	err = CommitCommonDifference(ctx, page, &previous, apply, func(context.Context, telegram.CommonUpdateState) error { return saveErr })
	require.ErrorIs(err, saveErr)
	assert.Equal(10, previous.Pts)
	require.NoError(CommitCommonDifference(ctx, page, &previous, apply, func(context.Context, telegram.CommonUpdateState) error { return nil }))
	assert.Equal(11, previous.Pts)
	var userDeleted, channelDeleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "456:7").Scan(&userDeleted))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&channelDeleted))
	assert.True(userDeleted)
	assert.False(channelDeleted)
}

func TestChannelDeletionRequiresMatchingPeer(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:7", MessageType: "telegram"}})
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "channel"}
	err = ApplyChannelDeletions(ctx, st, source.ID, dialog, []tg.UpdateClass{&tg.UpdateDeleteChannelMessages{ChannelID: 124, Messages: []int{7}}})
	require.ErrorContains(err, "peer")
	require.NoError(ApplyChannelDeletions(ctx, st, source.ID, dialog, []tg.UpdateClass{&tg.UpdateDeleteChannelMessages{ChannelID: 123, Messages: []int{7}}}))
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&deleted))
	assert.True(t, deleted)
}

func TestChannelDeletionRejectsOtherSourceTypes(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "synthetic")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:7", MessageType: "discord"}})
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "channel"}
	require.Error(ApplyChannelDeletions(ctx, st, source.ID, dialog, []tg.UpdateClass{&tg.UpdateDeleteChannelMessages{ChannelID: 123, Messages: []int{7}}}))
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&deleted))
	assert.False(t, deleted)
}
