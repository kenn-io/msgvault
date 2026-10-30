package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBackfillPageAppliesCheckpointedAfterBound(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 3, PeerID: &tg.PeerChat{ChatID: 123}, Date: 101, Message: "new"},
		&tg.Message{ID: 2, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "boundary"},
		&tg.Message{ID: 1, PeerID: &tg.PeerChat{ChatID: 123}, Date: 99, Message: "old"},
	}})
	state := telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 3, After: time.Unix(100, 0).UTC()}}
	var written []int
	err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
		func(_ context.Context, message tg.MessageClass, _ []tg.UserClass, _ []tg.ChatClass) error {
			written = append(written, message.(*tg.Message).ID)
			return nil
		}, func(_ context.Context, next telegram.DialogState) error {
			assert.Equal(t, []int{3}, written)
			assert.Nil(t, next.History)
			assert.Equal(t, 3, next.Frontier)
			return nil
		})
	require.NoError(t, err)
	assert.Nil(t, state.History)
	assert.Equal(t, 3, state.Frontier)
}

func TestBackfillPageVisitsEveryTakeoutRangeBeforeCompleting(t *testing.T) {
	mock := tgmock.NewRequire(t)
	ranges := []telegram.HistoryRange{{MinID: 1, MaxID: 10}, {MinID: 11, MaxID: 30}}
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithMessagesRangeRequest)
		require.True(t, ok)
		assert.Equal(t, tg.MessageRange{MinID: 1, MaxID: 10}, wrapped.Range)
		req, ok := wrapped.Query.(*tg.MessagesGetHistoryRequest)
		require.True(t, ok)
		assert.Zero(t, req.OffsetID)
	}).ThenResult(&tg.MessagesMessages{})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithMessagesRangeRequest)
		require.True(t, ok)
		assert.Equal(t, tg.MessageRange{MinID: 11, MaxID: 30}, wrapped.Range)
		req, ok := wrapped.Query.(*tg.MessagesGetHistoryRequest)
		require.True(t, ok)
		assert.Zero(t, req.OffsetID)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 20}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithMessagesRangeRequest)
		require.True(t, ok)
		assert.Equal(t, tg.MessageRange{MinID: 11, MaxID: 30}, wrapped.Range)
		req, ok := wrapped.Query.(*tg.MessagesGetHistoryRequest)
		require.True(t, ok)
		assert.Equal(t, 20, req.OffsetID)
	}).ThenResult(&tg.MessagesMessages{})

	state := telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 30, Ranges: ranges}}
	saved := 0
	for range 3 {
		err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
			func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error { return nil },
			func(_ context.Context, next telegram.DialogState) error {
				saved++
				if saved == 1 {
					require.NotNil(t, next.History)
					assert.Equal(t, 1, next.History.RangeIndex)
					assert.Zero(t, next.History.Offset)
					assert.Zero(t, next.Frontier)
				} else if saved == 2 {
					require.NotNil(t, next.History)
					assert.Equal(t, 1, next.History.RangeIndex)
					assert.Equal(t, 20, next.History.Offset)
				} else {
					assert.Nil(t, next.History)
					assert.Equal(t, 30, next.Frontier)
				}
				return nil
			})
		require.NoError(t, err)
	}
	assert.Equal(t, 3, saved)
	assert.Nil(t, state.History)
	assert.Equal(t, 30, state.Frontier)
	assert.True(t, mock.AllWereMet())
}

func TestArchiveHistoryPageResolvesRepliesBeforeCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	child := &tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "child"}
	reply := &tg.MessageReplyHeader{}
	reply.SetReplyToMsgID(7)
	child.SetReplyTo(reply)
	parent := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 90, Message: "parent"}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{child, parent}})
	state := telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 10}}
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	saved := false
	err = ArchiveHistoryPage(ctx, tg.NewClient(mock), writer, dialog, &state, func(_ context.Context, next telegram.DialogState) error {
		var linked bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:8").Scan(&linked))
		assert.True(t, linked)
		assert.Equal(t, 7, next.History.Offset)
		saved = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, saved)
}

func TestArchiveHistoryPageResolvesOnlyRepliesFromThatPage(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "-123", "group_chat", "Earlier dialog")
	require.NoError(t, err)
	sentAt := time.Unix(90, 0).UTC()
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
		ConversationID: conversationID, SourceID: source.ID, SourceMessageID: "-123:1",
		MessageType: telegram.MessageType, SentAt: sql.NullTime{Time: sentAt, Valid: true},
	}})
	require.NoError(t, err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{ConversationID: conversationID, SourceID: source.ID, SourceMessageID: "-123:2",
			MessageType: telegram.MessageType, SentAt: sql.NullTime{Time: sentAt.Add(time.Second), Valid: true}},
		Metadata: &sql.NullString{String: `{"referenced_message_id":"-123:1","reply_peer_verified":true}`, Valid: true},
	})
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 3, PeerID: &tg.PeerChat{ChatID: 456}, Date: 100, Message: "unrelated"},
	}})
	state := telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 4}}
	dialog := CatalogDialog{ChatID: -456, NativeID: 456, Kind: "group", Title: "Current dialog"}
	err = ArchiveHistoryPage(ctx, tg.NewClient(mock), writer, dialog, &state,
		func(context.Context, telegram.DialogState) error { return nil })
	require.NoError(t, err)
	var linked bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:2").Scan(&linked))
	assert.False(t, linked, "a page must not sweep unresolved replies from every dialog")
	require.NoError(t, backfillDialogsWithOptions(ctx, tg.NewClient(tgmock.NewRequire(t)), writer,
		telegram.NewSyncState(), nil, time.Unix(100, 0).UTC(), false, 0, HistoryOptions{},
		func(context.Context, *telegram.SyncState) error { return nil }))
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:2").Scan(&linked))
	assert.True(t, linked, "the final run reconciliation links older unresolved replies")
}

func TestBackfillPageCheckpointFollowsAllWrites(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(t, ok)
		assert.Equal(t, 50, req.OffsetID)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 40}, &tg.MessageEmpty{ID: 39}}})
	state := telegram.DialogState{Frontier: 10, History: &telegram.HistoryProgress{Ceiling: 60, Offset: 50, MinID: 10}}
	writes := 0
	saved := false
	err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
		func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error { writes++; return nil },
		func(_ context.Context, next telegram.DialogState) error {
			saved = true
			assert.Equal(t, 2, writes)
			assert.Equal(t, 39, next.History.Offset)
			return nil
		})
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.True(saved)
	}
	assert.Equal(t, 10, state.Frontier)
	assert.Equal(t, 39, state.History.Offset)
}

func TestBackfillPageFailureDoesNotAdvanceCheckpoint(t *testing.T) {
	assert := assert.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 40}, &tg.MessageEmpty{ID: 39}}})
	state := telegram.DialogState{Frontier: 10, History: &telegram.HistoryProgress{Ceiling: 60, Offset: 50, MinID: 10}}
	writes := 0
	fail := errors.New("synthetic write failure")
	saved := false
	err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
		func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error {
			writes++
			if writes == 2 {
				return fail
			}
			return nil
		},
		func(context.Context, telegram.DialogState) error { saved = true; return nil })
	require.ErrorIs(t, err, fail)
	assert.False(saved)
	assert.Equal(50, state.History.Offset)
	assert.Equal(10, state.Frontier)
}

func TestBackfillPageCompletesOnlyAfterEmptyPageIsSaved(t *testing.T) {
	require := require.New(t)

	for _, saveFails := range []bool{true, false} {
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.MessagesMessages{})
		state := telegram.DialogState{Frontier: 10, History: &telegram.HistoryProgress{Ceiling: 60, Offset: 11, MinID: 10}}
		fail := errors.New("synthetic checkpoint failure")
		err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
			func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error { return nil },
			func(_ context.Context, next telegram.DialogState) error {
				assert.Nil(t, next.History)
				assert.Equal(t, 60, next.Frontier)
				if saveFails {
					return fail
				}
				return nil
			})
		if saveFails {
			require.ErrorIs(err, fail)
			{
				assert := assert.New(t)
				assert.Equal(10, state.Frontier)
			}
			require.NotNil(state.History)
		} else {
			require.NoError(err)
			assert.Equal(t, 60, state.Frontier)
			assert.Nil(t, state.History)
		}
	}
}

func TestBackfillPageRejectsInvalidSnapshot(t *testing.T) {
	state := telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 10, MinID: 10}}
	mock := tgmock.NewRequire(t)
	err := BackfillPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, &state,
		func(context.Context, tg.MessageClass, []tg.UserClass, []tg.ChatClass) error { return nil },
		func(context.Context, telegram.DialogState) error { return nil })
	require.ErrorContains(t, err, "checkpoint")
}
