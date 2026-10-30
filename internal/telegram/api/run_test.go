package api

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAuthenticatedBackfillVerifiesOwnerBeforeArchiveWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 998}, Users: []tg.UserClass{&tg.User{ID: 998, Self: true}}})
	err = telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		return RunAuthenticatedBackfill(ctx, scoped, source.ID, tg.NewClient(mock), state, time.Unix(100, 0), false, 0, save)
	})
	require.ErrorContains(err, "account")
	assert.True(mock.AllWereMet())
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Zero(count)
}

func TestAuthenticatedBackfillSyncsSelectedChatWithInaccessibleShiftedHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	shiftedConversation, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic inaccessible group")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{
		SourceID: source.ID, ConversationID: shiftedConversation,
		SourceMessageID: "-1000000000123:-999999999", MessageType: "telegram",
	}})
	require.NoError(err)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(ok)
		folder, present := request.GetFolderID()
		require.True(present)
		assert.Equal(0, folder)
	}).ThenResult(&tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 8},
			&tg.Dialog{Peer: &tg.PeerChat{ChatID: 456}, TopMessage: 1},
		},
		Chats: []tg.ChatClass{
			&tg.ChannelForbidden{ID: 123, AccessHash: 42, Title: "Synthetic inaccessible group", Megagroup: true},
			&tg.Chat{ID: 456, Title: "Synthetic accessible group", Photo: &tg.ChatPhotoEmpty{}},
		},
	})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(ok)
		folder, present := request.GetFolderID()
		require.True(present)
		assert.Equal(1, folder)
	}).ThenResult(&tg.MessagesDialogs{})
	for _, result := range []*tg.MessagesMessages{
		{Messages: []tg.MessageClass{&tg.Message{ID: 1, PeerID: &tg.PeerChat{ChatID: 456}, Date: 100, Message: "accessible history"}}},
		{},
	} {
		mock.ExpectFunc(func(input bin.Encoder) {
			request, ok := input.(*tg.MessagesGetHistoryRequest)
			require.True(ok)
			assert.Equal(&tg.InputPeerChat{ChatID: 456}, request.Peer)
		}).ThenResult(result)
	}

	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 1, Date: 100}
	err = RunAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), state, time.Unix(200, 0), false, -456, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Equal(1, state.Dialogs["-456"].Frontier)
	assert.True(mock.AllWereMet())
	shifted, err := st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	assert.True(shifted, "the inaccessible peer's shifted history stays available for repair")
	blockedWriter, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	err = blockedWriter.Write(ctx, telegram.Chat{ID: -1000000000123, Type: "private_group", Name: "Synthetic inaccessible group"}, telegram.Message{
		ID: 9, SentAt: time.Unix(200, 0), Text: "must stay fenced", RawJSON: `{}`,
	})
	require.ErrorContains(err, "verified migration linkage")
	var archived int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-456:1").Scan(&archived))
	assert.Equal(1, archived)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:9").Scan(&archived))
	assert.Zero(archived)
}

func TestTakeoutRequiresFirstFullBackfill(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	require.NoError(telegram.WithLiveSyncState(ctx, st, source.ID, func(context.Context, *store.Store, *telegram.SyncState, func(context.Context, *telegram.SyncState) error) error {
		return nil
	}))
	for _, options := range []HistoryOptions{{Takeout: true}, {Full: true, Takeout: true}} {
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
		err := telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
			return RunAuthenticatedBackfillWithOptions(ctx, scoped, source.ID, tg.NewClient(mock), state, time.Unix(100, 0), false, 0, options, save)
		})
		require.ErrorContains(err, "takeout")
		assert.True(t, mock.AllWereMet())
	}
}

func TestTakeoutResumesPendingRangesAfterSuccessfulSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	ranges := []telegram.HistoryRange{{MinID: 1, MaxID: 100}}
	require.NoError(telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, _ *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
		state.Dialogs["-123"] = telegram.DialogState{History: &telegram.HistoryProgress{Ceiling: 7, Offset: 6, Ranges: ranges}}
		return save(ctx, state)
	}))

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(ok)
		folder, present := request.GetFolderID()
		require.True(present)
		assert.Equal(0, folder)
	}).ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(ok)
		folder, present := request.GetFolderID()
		require.True(present)
		assert.Equal(1, folder)
	}).ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(ok)
		assert.Equal(int64(42), request.TakeoutID)
		assert.IsType(&tg.MessagesGetSplitRangesRequest{}, request.Query)
	}).ThenResult(&tg.MessageRangeVector{Elems: []tg.MessageRange{{MinID: 1, MaxID: 100}}})
	for _, folder := range []int{0, 1} {
		wantFolder := folder
		mock.ExpectFunc(func(input bin.Encoder) {
			takeout, ok := input.(*tg.InvokeWithTakeoutRequest)
			require.True(ok)
			assert.Equal(int64(42), takeout.TakeoutID)
			ranged, ok := takeout.Query.(*tg.InvokeWithMessagesRangeRequest)
			require.True(ok)
			assert.Equal(tg.MessageRange{MinID: 1, MaxID: 100}, ranged.Range)
			request, ok := ranged.Query.(*tg.MessagesGetDialogsRequest)
			require.True(ok)
			folder, present := request.GetFolderID()
			require.True(present)
			assert.Equal(wantFolder, folder)
		}).ThenResult(func() *tg.MessagesDialogs {
			if wantFolder == 1 {
				return &tg.MessagesDialogs{}
			}
			return &tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}}
		}())
	}
	mock.ExpectFunc(func(input bin.Encoder) {
		takeout, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(ok)
		assert.Equal(int64(42), takeout.TakeoutID)
		ranged, ok := takeout.Query.(*tg.InvokeWithMessagesRangeRequest)
		require.True(ok)
		assert.Equal(tg.MessageRange{MinID: 1, MaxID: 100}, ranged.Range)
		request, ok := ranged.Query.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(6, request.OffsetID)
	}).ThenResult(&tg.MessagesMessages{})
	mock.ExpectFunc(func(input bin.Encoder) {
		takeout, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(ok)
		assert.Equal(int64(42), takeout.TakeoutID)
		assert.IsType(&tg.AccountFinishTakeoutSessionRequest{}, takeout.Query)
	}).ThenResult(&tg.BoolTrue{})

	var final *telegram.SyncState
	err = telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		final = state
		return RunAuthenticatedBackfillWithOptions(ctx, scoped, source.ID, tg.NewClient(mock), state, time.Unix(200, 0), false, -123, HistoryOptions{Takeout: true}, save)
	})
	require.NoError(err)
	require.NotNil(final)
	assert.Nil(final.Dialogs["-123"].History)
	assert.Equal(7, final.Dialogs["-123"].Frontier)
	assert.True(mock.AllWereMet())
}

func TestSelectedSupergroupFullBackfillArchivesFormerGroupHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	channel := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", AccessHash: 42, Title: "Synthetic group", TopMessage: 8, ObservedPTS: 10, HasObservedPTS: true, EnabledByDefault: true}
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(456)
	full.SetMigratedFromMaxID(2)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
	former, err := selectedFormerGroup(ctx, tg.NewClient(mock), []CatalogDialog{channel}, channel.ChatID)
	require.NoError(err)
	require.NotNil(former)
	assert.Equal(int64(-456), former.ChatID)
	assert.Equal(channel.ChatID, former.MigrationParentChatID)
	dialogs := attachFormerGroup([]CatalogDialog{channel}, former, nil)

	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChannel{ChannelID: 123, AccessHash: 42}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "current history"}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChannel{ChannelID: 123, AccessHash: 42}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChannel{ChannelID: 123, AccessHash: 42}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "current history"}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChannel{ChannelID: 123, AccessHash: 42}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChat{ChatID: 456}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 2, PeerID: &tg.PeerChat{ChatID: 456}, Date: 90, Message: "former history"}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChat{ChatID: 456}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChat{ChatID: 456}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 2, PeerID: &tg.PeerChat{ChatID: 456}, Date: 90, Message: "former history"}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(ok)
		assert.Equal(&tg.InputPeerChat{ChatID: 456}, request.Peer)
	}).ThenResult(&tg.MessagesMessages{})

	state := telegram.NewSyncState()
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, dialogs, time.Unix(200, 0), false, channel.ChatID, HistoryOptions{Full: true}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Equal(8, state.Dialogs["-1000000000123"].Frontier)
	assert.Equal(2, state.Dialogs["-456"].Frontier)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(2, count)
	assert.True(mock.AllWereMet())
}

func TestAuthenticatedFullBackfillArchivesEveryEnabledFormerGroup(t *testing.T) {
	for _, takeout := range []bool{false, true} {
		name := "ordinary"
		if takeout {
			name = "takeout"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
			require.NoError(err)

			firstChannel := &tg.Channel{ID: 123, Title: "Synthetic one", Megagroup: true, Photo: &tg.ChatPhotoEmpty{}}
			firstChannel.SetAccessHash(42)
			secondChannel := &tg.Channel{ID: 124, Title: "Synthetic two", Megagroup: true, Photo: &tg.ChatPhotoEmpty{}}
			secondChannel.SetAccessHash(43)
			firstDialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 1}
			firstDialog.SetPts(10)
			secondDialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 124}, TopMessage: 1}
			secondDialog.SetPts(11)
			catalog := &tg.MessagesDialogs{Dialogs: []tg.DialogClass{firstDialog, secondDialog}, Chats: []tg.ChatClass{firstChannel, secondChannel}}
			mock := tgmock.NewRequire(t)
			mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
			mock.Expect().ThenResult(catalog)
			mock.Expect().ThenResult(&tg.MessagesDialogs{})
			for _, item := range []struct {
				channelID int64
				access    int64
				formerID  int64
			}{{123, 42, 456}, {124, 43, 789}} {
				item := item
				full := &tg.ChannelFull{ID: item.channelID, ChatPhoto: &tg.PhotoEmpty{}}
				full.SetMigratedFromChatID(item.formerID)
				full.SetMigratedFromMaxID(1)
				mock.ExpectFunc(func(input bin.Encoder) {
					request, ok := input.(*tg.ChannelsGetFullChannelRequest)
					require.True(ok)
					assert.Equal(&tg.InputChannel{ChannelID: item.channelID, AccessHash: item.access}, request.Channel)
				}).ThenResult(&tg.MessagesChatFull{FullChat: full})
			}

			range_ := tg.MessageRange{MinID: 1, MaxID: 100}
			if takeout {
				mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
				mock.ExpectFunc(func(input bin.Encoder) {
					wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
					require.True(ok)
					assert.Equal(int64(42), wrapped.TakeoutID)
					assert.IsType(&tg.MessagesGetSplitRangesRequest{}, wrapped.Query)
				}).ThenResult(&tg.MessageRangeVector{Elems: []tg.MessageRange{range_}})
				for _, folder := range []int{0, 1} {
					wantFolder := folder
					mock.ExpectFunc(func(input bin.Encoder) {
						wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
						require.True(ok)
						ranged, ok := wrapped.Query.(*tg.InvokeWithMessagesRangeRequest)
						require.True(ok)
						assert.Equal(range_, ranged.Range)
						request, ok := ranged.Query.(*tg.MessagesGetDialogsRequest)
						require.True(ok)
						gotFolder, present := request.GetFolderID()
						require.True(present)
						assert.Equal(wantFolder, gotFolder)
					}).ThenResult(func() *tg.MessagesDialogs {
						if wantFolder == 1 {
							return &tg.MessagesDialogs{}
						}
						return catalog
					}())
				}
			}

			for _, item := range []struct {
				peer      tg.InputPeerClass
				native    tg.PeerClass
				message   string
				messageID int
			}{{&tg.InputPeerChannel{ChannelID: 123, AccessHash: 42}, &tg.PeerChannel{ChannelID: 123}, "current one", 1},
				{&tg.InputPeerChannel{ChannelID: 124, AccessHash: 43}, &tg.PeerChannel{ChannelID: 124}, "current two", 1},
				{&tg.InputPeerChat{ChatID: 456}, &tg.PeerChat{ChatID: 456}, "former one", 1},
				{&tg.InputPeerChat{ChatID: 789}, &tg.PeerChat{ChatID: 789}, "former two", 1}} {
				item := item
				for _, result := range []*tg.MessagesMessages{
					{Messages: []tg.MessageClass{&tg.Message{ID: item.messageID, PeerID: item.native, Date: 100, Message: item.message}}},
					{},
				} {
					mock.ExpectFunc(func(input bin.Encoder) {
						var request *tg.MessagesGetHistoryRequest
						if takeout {
							wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
							require.True(ok)
							assert.Equal(int64(42), wrapped.TakeoutID)
							ranged, ok := wrapped.Query.(*tg.InvokeWithMessagesRangeRequest)
							require.True(ok)
							assert.Equal(range_, ranged.Range)
							request, ok = ranged.Query.(*tg.MessagesGetHistoryRequest)
							require.True(ok)
						} else {
							var ok bool
							request, ok = input.(*tg.MessagesGetHistoryRequest)
							require.True(ok)
						}
						assert.Equal(item.peer, request.Peer)
					}).ThenResult(result)
				}
				mock.ExpectFunc(func(input bin.Encoder) {
					var request *tg.MessagesGetHistoryRequest
					if takeout {
						wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
						require.True(ok)
						assert.Equal(int64(42), wrapped.TakeoutID)
						request, ok = wrapped.Query.(*tg.MessagesGetHistoryRequest)
						require.True(ok)
					} else {
						var ok bool
						request, ok = input.(*tg.MessagesGetHistoryRequest)
						require.True(ok)
					}
					assert.Equal(item.peer, request.Peer)
				}).ThenResult(&tg.MessagesMessages{})
			}
			if takeout {
				mock.ExpectFunc(func(input bin.Encoder) {
					wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
					require.True(ok)
					assert.Equal(int64(42), wrapped.TakeoutID)
					assert.IsType(&tg.AccountFinishTakeoutSessionRequest{}, wrapped.Query)
				}).ThenResult(&tg.BoolTrue{})
			}
			mock.ExpectFunc(func(input bin.Encoder) {
				request, ok := input.(*tg.UpdatesGetDifferenceRequest)
				require.True(ok)
				assert.Equal(12, request.Pts)
				assert.Equal(100, request.Date)
			}).ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
			for _, item := range []struct {
				channelID int64
				access    int64
				pts       int
			}{{123, 42, 10}, {124, 43, 11}} {
				item := item
				mock.ExpectFunc(func(input bin.Encoder) {
					request, ok := input.(*tg.UpdatesGetChannelDifferenceRequest)
					require.True(ok)
					assert.Equal(&tg.InputChannel{ChannelID: item.channelID, AccessHash: item.access}, request.Channel)
					assert.Equal(item.pts, request.Pts)
				}).ThenResult(&tg.UpdatesChannelDifferenceEmpty{Pts: item.pts, Final: true})
			}

			state := telegram.NewSyncState()
			err = runAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), &tg.User{ID: 999, FirstName: "Synthetic"}, state, time.Unix(200, 0), false, 0, HistoryOptions{Full: true, Takeout: takeout}, func(context.Context, *telegram.SyncState) error { return nil })
			require.NoError(err)
			assert.Equal(1, state.Dialogs["-456"].Frontier)
			assert.Equal(1, state.Dialogs["-789"].Frontier)
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
			assert.Equal(4, count)
			assert.True(mock.AllWereMet())
		})
	}
}

func TestFullBackfillRewalksCompletedFrontierWithTimeBound(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	state := telegram.NewSyncState()
	state.Dialogs["-123"] = telegram.DialogState{Frontier: 7}
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", TopMessage: 7, EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 101, Message: "new"},
		&tg.Message{ID: 6, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "boundary"},
	}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	var sawFull bool
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(200, 0), false, 0, HistoryOptions{Full: true, After: time.Unix(100, 0).UTC()}, func(_ context.Context, next *telegram.SyncState) error {
		if progress := next.Dialogs["-123"].History; progress != nil {
			assert.Zero(t, progress.MinID)
			assert.Equal(t, time.Unix(100, 0).UTC(), progress.After)
			sawFull = sawFull || progress.Full
		}
		return nil
	})
	require.NoError(err)
	{
		assert := assert.New(t)
		assert.True(sawFull)
	}
	assert.True(t, mock.AllWereMet())
	assert.Equal(t, 7, state.Dialogs["-123"].Frontier)
	assert.Nil(t, state.Dialogs["-123"].History)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestChannelBaselineIsSavedBeforeHistory(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", TopMessage: 7, EnabledByDefault: true, HasObservedPTS: true, ObservedPTS: 20}
	state := telegram.NewSyncState()
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	var baselineSaved bool
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(200, 0), false, 0, HistoryOptions{}, func(_ context.Context, next *telegram.SyncState) error {
		progress := next.Dialogs["-1000000000123"]
		if progress.ChannelPTS != nil {
			assert.Equal(t, 20, *progress.ChannelPTS)
			baselineSaved = true
		}
		if progress.History != nil {
			assert.True(t, baselineSaved)
		}
		return nil
	})
	require.NoError(err)
	{
		assert := assert.New(t)
		assert.True(baselineSaved)
	}
	assert.True(t, mock.AllWereMet())
}

func TestChannelWithoutAppliedPositionRescansAndRecordsGap(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	now := time.Unix(1000000, 0).UTC()
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", TopMessage: 7, EnabledByDefault: true, HasObservedPTS: true, ObservedPTS: 20}
	state := telegram.NewSyncState()
	state.Dialogs["-1000000000123"] = telegram.DialogState{Frontier: 7}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: int(now.Unix()), Message: "recovered"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, now, false, 0, HistoryOptions{}, func(_ context.Context, next *telegram.SyncState) error {
		if next.Dialogs["-1000000000123"].ChannelPTS != nil {
			assert.Equal(t, 1, next.Gaps["-1000000000123"].Count)
		}
		return nil
	})
	require.NoError(err)
	{
		assert := assert.New(t)
		assert.Equal(20, *state.Dialogs["-1000000000123"].ChannelPTS)
	}
	assert.Equal(t, 1, state.Gaps["-1000000000123"].Count)
	assert.True(t, mock.AllWereMet())
}

func TestChannelHistoryLossRetainsCursorAndContinues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", TopMessage: 7, EnabledByDefault: true, HasObservedPTS: true, ObservedPTS: 20}
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenRPCErr(tgerr.New(400, "CHANNEL_PRIVATE"))
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(200, 0), false, 0, HistoryOptions{}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	progress := state.Dialogs["-1000000000123"]
	assert.Equal(10, *progress.ChannelPTS)
	assert.NotNil(progress.History)
	assert.NotNil(progress.Peer.InaccessibleSince)
	assert.True(mock.AllWereMet())
}

func TestFullBackfillResumesWithoutRestartingCompletedScan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	state := telegram.NewSyncState()
	state.Dialogs["-123"] = telegram.DialogState{Frontier: 7, History: &telegram.HistoryProgress{Ceiling: 7, Offset: 1, Full: true}}
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", TopMessage: 7, EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(200, 0), false, 0, HistoryOptions{Full: true}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Nil(state.Dialogs["-123"].History)
	assert.True(mock.AllWereMet())
}

func TestFullBackfillRescansRecentEdits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	now := time.Unix(1000000, 0).UTC()
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", TopMessage: 7, EnabledByDefault: true}
	original := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: int(now.Unix()), Message: "original"}
	require.NoError(ArchiveAPIMessageWithWriter(ctx, writer, dialog, original, nil, nil))
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: int(now.Unix()), Message: "edited"}
	edited.SetEditDate(int(now.Unix()) + 1)
	mock := tgmock.NewRequire(t)
	for range 2 {
		mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{edited}})
		mock.Expect().ThenResult(&tg.MessagesMessages{})
	}
	state := telegram.NewSyncState()
	state.Dialogs["-123"] = telegram.DialogState{Frontier: 7}
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, now, false, 0, HistoryOptions{Full: true}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
	assert.Equal("edited", body)
	var isEdited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), id).Scan(&isEdited))
	assert.True(isEdited, "full history applies edit evidence through the live edit path")
	assert.True(mock.AllWereMet())
}

func TestFullBackfillAppliesChangedTimeBoundAfterResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	state := telegram.NewSyncState()
	state.Dialogs["-123"] = telegram.DialogState{Frontier: 7, History: &telegram.HistoryProgress{Ceiling: 7, Offset: 1, Full: true, After: time.Unix(100, 0).UTC()}}
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", TopMessage: 7, EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 75, Message: "newly included"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(200, 0), false, 0, HistoryOptions{Full: true, After: time.Unix(50, 0).UTC()}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.True(mock.AllWereMet())
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestAuthenticatedBackfillArchivesDiscoveredHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true, FirstName: "Synthetic"}}})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(ok)
		assert.Equal(int64(42), wrapped.TakeoutID)
		assert.IsType(&tg.MessagesGetSplitRangesRequest{}, wrapped.Query)
	}).ThenResult(&tg.MessageRangeVector{Elems: []tg.MessageRange{{MinID: 1, MaxID: 100}}})
	for _, result := range []*tg.MessagesDialogs{
		{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}},
		{},
	} {
		mock.ExpectFunc(func(input bin.Encoder) {
			takeout, ok := input.(*tg.InvokeWithTakeoutRequest)
			require.True(ok)
			assert.Equal(int64(42), takeout.TakeoutID)
			ranged, ok := takeout.Query.(*tg.InvokeWithMessagesRangeRequest)
			require.True(ok)
			assert.Equal(tg.MessageRange{MinID: 1, MaxID: 100}, ranged.Range)
			assert.IsType(&tg.MessagesGetDialogsRequest{}, ranged.Query)
		}).ThenResult(result)
	}
	for _, result := range []*tg.MessagesMessages{
		{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}},
		{},
	} {
		mock.ExpectFunc(func(input bin.Encoder) {
			takeout, ok := input.(*tg.InvokeWithTakeoutRequest)
			require.True(ok)
			assert.Equal(int64(42), takeout.TakeoutID)
			ranged, ok := takeout.Query.(*tg.InvokeWithMessagesRangeRequest)
			require.True(ok)
			assert.Equal(tg.MessageRange{MinID: 1, MaxID: 100}, ranged.Range)
			assert.IsType(&tg.MessagesGetHistoryRequest{}, ranged.Query)
		}).ThenResult(result)
	}
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(ok)
		assert.Equal(int64(42), wrapped.TakeoutID)
		assert.IsType(&tg.AccountFinishTakeoutSessionRequest{}, wrapped.Query)
	}).ThenResult(&tg.BoolTrue{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	err = telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		return RunAuthenticatedBackfillWithOptions(ctx, scoped, source.ID, tg.NewClient(mock), state, time.Unix(100, 0), false, 0, HistoryOptions{Takeout: true}, save)
	})
	require.NoError(err)
	assert.True(mock.AllWereMet())
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(1, count)
}

func TestAuthenticatedSyncAppliesCommonChangesAfterHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 10, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "history"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "edited"}
	edited.SetEditDate(101)
	mock.Expect().ThenResult(&tg.UpdatesDifference{OtherUpdates: []tg.UpdateClass{&tg.UpdateEditMessage{Message: edited, Pts: 11, PtsCount: 1}}, State: tg.UpdatesState{Pts: 11, Date: 101}})
	state := telegram.NewSyncState()
	err = runAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), &tg.User{ID: 999, FirstName: "Synthetic"}, state, time.Unix(200, 0), false, 0, HistoryOptions{}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
	assert.Equal("edited", body)
	assert.Equal(11, state.Common.Pts)
	assert.True(mock.AllWereMet())
}

func TestAuthenticatedSyncProcessesMediaBeforeHistoryCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 10, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{ID: 101, DCID: 1, Size: 20})
	message.SetMedia(media)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	state := telegram.NewSyncState()
	options := HistoryOptions{MediaProcessor: func(_ context.Context, _ telegram.Chat, media *telegram.LiveMedia) error {
		media.SkipReason = attachmentpolicy.SkipSizeCap
		return nil
	}}
	var sawCheckpoint bool
	err = runAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), &tg.User{ID: 999}, state, time.Unix(200, 0), false, 0, options, func(_ context.Context, next *telegram.SyncState) error {
		if next.Dialogs["-123"].Frontier == 7 {
			var result string
			require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT a.attachment_state FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&result))
			assert.Equal(t, "skipped", result)
			sawCheckpoint = true
		}
		return nil
	})
	require.NoError(t, err)
	assert.True(t, sawCheckpoint)
	assert.True(t, mock.AllWereMet())
}

func TestAuthenticatedSyncStoresMediaUnderPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 10, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", ParticipantsCount: 2, Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{ID: 101, DCID: 1, Size: 9})
	message.SetMedia(media)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	options := HistoryOptions{Media: &MediaOptions{AttachmentsDir: t.TempDir(), TempDir: secureTempDir(t), Policy: attachmentpolicy.Policy{MaxBytes: 20, MaxParticipants: 20}, Download: func(_ context.Context, _ *MediaReference, dir string, _ int64) (*os.File, error) {
		file, err := os.CreateTemp(dir, "synthetic-*")
		require.NoError(err)
		_, err = file.WriteString("synthetic")
		require.NoError(err)
		_, err = file.Seek(0, 0)
		return file, err
	}}}
	err = runAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), &tg.User{ID: 999}, telegram.NewSyncState(), time.Unix(200, 0), false, 0, options, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var path, state string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT a.storage_path, a.attachment_state FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&path, &state))
	assert.NotEmpty(path)
	assert.Equal("stored", state)
	assert.True(mock.AllWereMet())
}

func TestAuthenticatedChatFilterAppliesSelectedCommonChangesWithoutAdvancingCursor(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	for _, row := range []struct {
		peer string
		key  string
		body string
	}{
		{peer: "-123", key: "-123:6", body: "selected before"},
		{peer: "-456", key: "-456:8", body: "unselected before"},
		{peer: "-123", key: "-123:9", body: "selected deletion target"},
		{peer: "-456", key: "-456:9", body: "unselected deletion target"},
	} {
		conversation, err := st.EnsureConversationWithType(source.ID, row.peer, "group_chat", "Synthetic")
		require.NoError(t, err)
		_, err = st.PersistMessage(&store.MessagePersistData{
			Message:  &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: row.key, MessageType: "telegram"},
			BodyText: sql.NullString{String: row.body, Valid: true},
		})
		require.NoError(t, err)
	}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 10, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7},
			&tg.Dialog{Peer: &tg.PeerChat{ChatID: 456}, TopMessage: 9},
		},
		Chats: []tg.ChatClass{
			&tg.Chat{ID: 123, Title: "Selected synthetic", Photo: &tg.ChatPhotoEmpty{}},
			&tg.Chat{ID: 456, Title: "Unselected synthetic", Photo: &tg.ChatPhotoEmpty{}},
		},
	})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "selected history"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	selectedEdit := &tg.Message{ID: 6, PeerID: &tg.PeerChat{ChatID: 123}, Date: 50, Message: "selected after"}
	selectedEdit.SetEditDate(150)
	unselectedEdit := &tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 456}, Date: 50, Message: "unselected after"}
	unselectedEdit.SetEditDate(150)
	mock.Expect().ThenResult(&tg.UpdatesDifference{
		OtherUpdates: []tg.UpdateClass{
			&tg.UpdateEditMessage{Message: selectedEdit, Pts: 11, PtsCount: 1},
			&tg.UpdateEditMessage{Message: unselectedEdit, Pts: 12, PtsCount: 1},
			&tg.UpdateDeleteMessages{Messages: []int{9}, Pts: 13, PtsCount: 1},
		},
		State: tg.UpdatesState{Pts: 13, Date: 151},
	})
	state := telegram.NewSyncState()
	var savedCommonPositions []int
	err = runAuthenticatedBackfill(ctx, st, source.ID, tg.NewClient(mock), &tg.User{ID: 999, FirstName: "Synthetic"}, state,
		time.Unix(200, 0), false, -123, HistoryOptions{}, func(_ context.Context, next *telegram.SyncState) error {
			if next.Common != nil {
				savedCommonPositions = append(savedCommonPositions, next.Common.Pts)
			}
			return nil
		})
	require.NoError(t, err)
	require.NotNil(t, state.Common)
	assert.Equal(t, 10, state.Common.Pts)
	assert.True(t, mock.AllWereMet())
	for _, pts := range savedCommonPositions {
		assert.Equal(t, 10, pts, "a chat-filtered run must not persist the account-wide common cursor")
	}
	for _, row := range []struct {
		key      string
		wantBody string
		deleted  bool
	}{
		{key: "-123:6", wantBody: "selected after"},
		{key: "-456:8", wantBody: "unselected before"},
		{key: "-123:9", wantBody: "selected deletion target", deleted: true},
		{key: "-456:9", wantBody: "unselected deletion target"},
	} {
		var messageID int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, row.key).Scan(&messageID))
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
		assert.Equal(t, row.wantBody, body, row.key)
		var deleted bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID).Scan(&deleted))
		assert.Equal(t, row.deleted, deleted, row.key)
	}
}

func TestAuthenticatedBackfillRepairsLegacyUserBeforeWriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	const peerID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	peer := &tg.User{ID: peerID, FirstName: "Synthetic"}
	peer.SetAccessHash(42)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: peerID}, TopMessage: 2}}, Users: []tg.UserClass{peer}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	for range 2 {
		mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: peerID}, Date: 100, Message: "synthetic two"}, &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: peerID}, Date: 100, Message: "synthetic one"}}, Users: []tg.UserClass{peer}})
		mock.Expect().ThenResult(&tg.MessagesMessages{})
	}
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	err = telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		return RunAuthenticatedBackfill(ctx, scoped, source.ID, tg.NewClient(mock), state, time.Unix(100, 0), false, 0, save)
	})
	require.NoError(err)
	assert.True(mock.AllWereMet())
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(ambiguous)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(2, count)
}

func TestBackfillDiscoveredDialogsCommitsHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	state := telegram.NewSyncState()
	var checkpoints []string
	err = BackfillDiscoveredDialogs(ctx, tg.NewClient(mock), writer, state, time.Unix(100, 0), false, 0, func(_ context.Context, next *telegram.SyncState) error {
		blob, err := next.Marshal()
		if err == nil {
			checkpoints = append(checkpoints, blob)
		}
		return err
	})
	require.NoError(err)
	assert.Equal(7, state.Dialogs["-123"].Frontier)
	assert.Nil(state.Dialogs["-123"].History)
	require.NotNil(state.Common)
	assert.Equal(12, state.Common.Pts)
	assert.GreaterOrEqual(len(checkpoints), 4)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(1, count)
}

func TestBackfillDiscoveredDialogsResumesFailedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(err)
	dialogs := &tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 8}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}}
	media := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "media"}
	mediaDoc := &tg.MessageMediaDocument{}
	mediaDoc.SetDocument(&tg.Document{ID: 101, AccessHash: 202, FileReference: []byte("synthetic-reference"), DCID: 3, Size: 42})
	media.SetMedia(mediaDoc)
	incomplete := &tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	incompleteDoc := &tg.MessageMediaDocument{}
	incompleteDoc.SetDocument(&tg.Document{ID: 102, Size: 42})
	incomplete.SetMedia(incompleteDoc)
	first := tgmock.NewRequire(t)
	first.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	first.Expect().ThenResult(dialogs)
	first.Expect().ThenResult(&tg.MessagesDialogs{})
	first.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{media, incomplete}})
	state := telegram.NewSyncState()
	var checkpoint string
	save := func(_ context.Context, next *telegram.SyncState) error {
		var err error
		checkpoint, err = next.Marshal()
		return err
	}
	err = BackfillDiscoveredDialogs(ctx, tg.NewClient(first), writer, state, time.Unix(100, 0), false, 0, save)
	require.ErrorContains(err, "document reference is unavailable")
	state, err = telegram.LoadSyncState(checkpoint)
	require.NoError(err)
	require.NotNil(state.Dialogs["-123"].History)
	assert.Zero(state.Dialogs["-123"].History.Offset)
	var stateName, mediaMetadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.attachment_state, CAST(a.attachment_metadata AS TEXT)
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&stateName, &mediaMetadata))
	assert.Equal("pending", stateName)
	assert.Contains(mediaMetadata, "c3ludGhldGljLXJlZmVyZW5jZQ==")
	second := tgmock.NewRequire(t)
	second.Expect().ThenResult(dialogs)
	second.Expect().ThenResult(&tg.MessagesDialogs{})
	second.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		media, &tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "retry"},
	}})
	second.Expect().ThenResult(&tg.MessagesMessages{})
	require.NoError(BackfillDiscoveredDialogs(ctx, tg.NewClient(second), writer, state, time.Unix(100, 0), false, 0, save))
	assert.Equal(8, state.Dialogs["-123"].Frontier)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(1, count)
}

func TestBackfillDiscoveredDialogsSkipsBroadcastByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(err)
	channel := &tg.Channel{ID: 123, Title: "Synthetic", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}
	channel.SetAccessHash(456)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{channel}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	state := telegram.NewSyncState()
	require.NoError(BackfillDiscoveredDialogs(ctx, tg.NewClient(mock), writer, state, time.Unix(100, 0), false, 0, func(context.Context, *telegram.SyncState) error { return nil }))
	assert.Zero(state.Dialogs["-1000000000123"].Frontier)
	assert.Nil(state.Dialogs["-1000000000123"].History)
	assert.True(mock.AllWereMet())
}

func TestUnfilteredIncludeChannelsSkipsInaccessibleBroadcast(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	forbidden := CatalogDialog{ChatID: -1000000000456, NativeID: 456, Kind: "channel", AccessHash: 789, TopMessage: 7, Inaccessible: true, UnsupportedReason: "Telegram dialog is inaccessible"}
	group := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", TopMessage: 1, EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	for _, result := range []*tg.MessagesMessages{
		{Messages: []tg.MessageClass{&tg.Message{ID: 1, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "accessible history"}}},
		{},
	} {
		mock.ExpectFunc(func(input bin.Encoder) {
			request, ok := input.(*tg.MessagesGetHistoryRequest)
			require.True(ok)
			assert.Equal(&tg.InputPeerChat{ChatID: 123}, request.Peer)
		}).ThenResult(result)
	}
	state := telegram.NewSyncState()
	err = backfillDialogsWithOptions(ctx, tg.NewClient(mock), writer, state, []CatalogDialog{forbidden, group}, time.Unix(200, 0), true, 0, HistoryOptions{}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Equal(1, state.Dialogs["-123"].Frontier)
	assert.True(mock.AllWereMet())

	err = backfillDialogsWithOptions(ctx, tg.NewClient(tgmock.NewRequire(t)), writer, telegram.NewSyncState(), []CatalogDialog{forbidden}, time.Unix(200, 0), true, forbidden.ChatID, HistoryOptions{}, func(context.Context, *telegram.SyncState) error { return nil })
	require.ErrorContains(err, "inaccessible")
}

func TestLiveBackfillReusesPreviouslyArchivedMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	mediaDir := filepath.Join(root, "files")
	require.NoError(os.MkdirAll(mediaDir, 0700))
	require.NoError(os.WriteFile(filepath.Join(mediaDir, "attachment.pdf"), []byte("archived bytes"), 0600))
	export := `{"personal_information":{"user_id":999,"first_name":"Owner"},"chats":{"list":[{"id":123,"type":"private_group","name":"Synthetic","messages":[{"id":7,"type":"message","date_unixtime":"100","text":"archived snapshot","file":"files/attachment.pdf","file_name":"attachment.pdf","mime_type":"application/pdf"}]}]}}`
	require.NoError(os.WriteFile(filepath.Join(root, "result.json"), []byte(export), 0600))
	attachmentsDir := t.TempDir()
	_, err := telegram.Import(ctx, st, telegram.ImportOptions{RootDir: root, AttachmentsDir: attachmentsDir})
	require.NoError(err)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	apiMessage := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "newer snapshot"}
	apiMedia := &tg.MessageMediaDocument{}
	apiMedia.SetDocument(&tg.Document{ID: 101, AccessHash: 202, FileReference: []byte("synthetic-reference"), DCID: 3, Size: 13, MimeType: "application/pdf", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "attachment.pdf"}}})
	apiMessage.SetMedia(apiMedia)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{apiMessage}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	err = telegram.WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, scoped *store.Store, state *telegram.SyncState, save func(context.Context, *telegram.SyncState) error) error {
		writer, err := telegram.NewLiveWriter(ctx, scoped, source.ID, 999, "Owner")
		if err != nil {
			return err
		}
		return BackfillDiscoveredDialogs(ctx, tg.NewClient(mock), writer, state, time.Unix(100, 0), false, 0, save)
	})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(1, count)
	var attachmentCount, storedBytes int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*), SUM(CASE WHEN storage_path != '' THEN 1 ELSE 0 END)
		FROM attachments WHERE message_id = ?`), id).Scan(&attachmentCount, &storedBytes))
	assert.Equal(1, attachmentCount, "live replay does not add a second occurrence")
	assert.Equal(1, storedBytes, "Desktop-imported media remains stored")
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("archived snapshot", body)
	completed, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(err)
	state, err := telegram.LoadSyncState(completed.CursorAfter.String)
	require.NoError(err)
	assert.Equal(7, state.Dialogs["-123"].Frontier)
}
