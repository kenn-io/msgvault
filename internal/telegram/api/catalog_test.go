package api

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRefreshCatalogPreservesHistoryAndCommitsAfterSave(t *testing.T) {
	require := require.New(t)

	state := telegram.NewSyncState()
	state.Dialogs["-123"] = telegram.DialogState{Frontier: 5, History: &telegram.HistoryProgress{Ceiling: 10}, Peer: &telegram.DialogPeer{NativeID: 123, Kind: "group", Title: "Old"}}
	dialogs := []CatalogDialog{{ChatID: -123, NativeID: 123, Kind: "group", Title: "New", TopMessage: 20, EnabledByDefault: true}}
	now := time.Unix(100, 0).UTC()
	failure := errors.New("synthetic save failure")
	err := RefreshCatalog(context.Background(), state, dialogs, now, func(context.Context, *telegram.SyncState) error { return failure })
	require.ErrorIs(err, failure)
	{
		assert := assert.New(t)
		assert.Equal("Old", state.Dialogs["-123"].Peer.Title)
	}
	err = RefreshCatalog(context.Background(), state, dialogs, now, func(_ context.Context, next *telegram.SyncState) error {
		assert.Equal(t, 5, next.Dialogs["-123"].Frontier)
		assert.Equal(t, 10, next.Dialogs["-123"].History.Ceiling)
		assert.Equal(t, "New", next.Dialogs["-123"].Peer.Title)
		return nil
	})
	require.NoError(err)
	assert.Equal(t, "New", state.Dialogs["-123"].Peer.Title)
}

func TestRefreshCatalogTracksMissingAndObservedPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	state := telegram.NewSyncState()
	state.Dialogs["123"] = telegram.DialogState{Peer: &telegram.DialogPeer{NativeID: 123, Kind: "user"}}
	now := time.Unix(100, 0).UTC()
	dialog := CatalogDialog{ChatID: -1000000000456, NativeID: 456, Kind: "channel", AccessHash: 99, HasObservedPTS: true, ObservedPTS: 7}
	save := func(context.Context, *telegram.SyncState) error { return nil }
	require.NoError(RefreshCatalog(context.Background(), state, []CatalogDialog{dialog}, now, save))
	require.NotNil(state.Dialogs["123"].Peer.MissingSince)
	assert.Equal(now, *state.Dialogs["123"].Peer.MissingSince)
	require.NotNil(state.Dialogs["-1000000000456"].Peer.ObservedPTS)
	assert.Equal(7, *state.Dialogs["-1000000000456"].Peer.ObservedPTS)
	require.NoError(RefreshCatalog(context.Background(), state, []CatalogDialog{dialog, {ChatID: 123, NativeID: 123, Kind: "user"}}, now.Add(time.Hour), save))
	assert.Nil(state.Dialogs["123"].Peer.MissingSince)
	require.ErrorContains(RefreshCatalog(context.Background(), state, []CatalogDialog{dialog, dialog}, now, save), "duplicate")
}

func TestMapDialogsUsesPeerKindsAndChannelDefaults(t *testing.T) {
	user := &tg.User{ID: 123, FirstName: "Example"}
	user.SetAccessHash(456)
	group := &tg.Chat{ID: 123, Title: "Example group"}
	channel := &tg.Channel{ID: 123, Title: "Example channel", Broadcast: true}
	channel.SetAccessHash(789)
	users := []tg.UserClass{user}
	chats := []tg.ChatClass{group, channel}
	cases := []struct {
		name    string
		peer    tg.PeerClass
		wantID  int64
		kind    string
		enabled bool
	}{
		{"user", &tg.PeerUser{UserID: 123}, 123, "user", true},
		{"group", &tg.PeerChat{ChatID: 123}, -123, "group", true},
		{"channel", &tg.PeerChannel{ChannelID: 123}, -1000000000123, "channel", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			got, err := MapDialog(&tg.Dialog{Peer: tc.peer, TopMessage: 42}, users, chats)
			require.NoError(t, err)
			assert.Equal(tc.wantID, got.ChatID)
			assert.Equal(tc.kind, got.Kind)
			assert.Equal(tc.enabled, got.EnabledByDefault)
			assert.Equal(42, got.TopMessage)
		})
	}
}

func TestMapDialogRequiresUsableAccessHash(t *testing.T) {
	user := &tg.User{ID: 123}
	_, err := MapDialog(&tg.Dialog{Peer: &tg.PeerUser{UserID: 123}}, []tg.UserClass{user}, nil)
	require.Error(t, err)
}

func TestMapSupergroupRetainsPtsAndInputPeer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	channel := &tg.Channel{ID: 321, Title: "Example group", Megagroup: true}
	channel.SetAccessHash(654)
	dialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 321}, TopMessage: 10}
	dialog.SetPts(99)
	got, err := MapDialog(dialog, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.Equal("supergroup", got.Kind)
	assert.True(got.EnabledByDefault)
	assert.True(got.HasObservedPTS)
	assert.Equal(99, got.ObservedPTS)
	peer, err := got.InputPeer()
	require.NoError(err)
	assert.Equal(&tg.InputPeerChannel{ChannelID: 321, AccessHash: 654}, peer)
}

func TestMapDialogPreservesKnownMembershipForMediaPolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	group := &tg.Chat{ID: 123, ParticipantsCount: 12}
	mapped, err := MapDialog(&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}}, nil, []tg.ChatClass{group})
	require.NoError(err)
	assert.True(mapped.ParticipantCountKnown)
	assert.Equal(12, mapped.ParticipantCount)
	channel := &tg.Channel{ID: 321, Megagroup: true}
	channel.SetAccessHash(42)
	channel.SetParticipantsCount(25)
	mapped, err = MapDialog(&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 321}}, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.True(mapped.ParticipantCountKnown)
	assert.Equal(25, mapped.ParticipantCount)
	channel = &tg.Channel{ID: 321, Megagroup: true}
	channel.SetAccessHash(42)
	mapped, err = MapDialog(&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 321}}, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.False(mapped.ParticipantCountKnown)
}

func TestMapMonoforumUsesSafeHistoryMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	channel := &tg.Channel{ID: 1002147483649, Title: "Synthetic", Monoforum: true}
	channel.SetAccessHash(42)
	dialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: channel.ID}, TopMessage: 10}
	mapped, err := MapDialog(dialog, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.Equal("supergroup", mapped.Kind)
	assert.True(mapped.EnabledByDefault)
	channel.Megagroup = true
	channel.Creator = true
	mapped, err = MapDialog(dialog, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.Equal("supergroup", mapped.Kind)
	assert.False(mapped.EnabledByDefault)
	assert.Equal("administrator monoforum requires topic-aware history", mapped.UnsupportedReason)
	channel.Creator = false
	channel.SetAdminRights(tg.ChatAdminRights{ManageDirectMessages: true})
	mapped, err = MapDialog(dialog, nil, []tg.ChatClass{channel})
	require.NoError(err)
	assert.False(mapped.EnabledByDefault)
	assert.Equal("administrator monoforum requires topic-aware history", mapped.UnsupportedReason)
}

func TestMapSavedMessagesNeedsNoAccessHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	self := &tg.User{ID: 123, Self: true}
	got, err := MapDialog(&tg.Dialog{Peer: &tg.PeerUser{UserID: 123}}, []tg.UserClass{self}, nil)
	require.NoError(err)
	assert.True(got.EnabledByDefault)
	peer, err := got.InputPeer()
	require.NoError(err)
	assert.IsType(&tg.InputPeerSelf{}, peer)
}

func TestMapDialogRetainsForbiddenPeersAsInaccessible(t *testing.T) {
	channel := &tg.ChannelForbidden{ID: 456, AccessHash: 789, Title: "Unavailable channel"}
	channel.SetMegagroup(true)
	for _, tc := range []struct {
		name   string
		peer   tg.PeerClass
		entity tg.ChatClass
		kind   string
		input  tg.InputPeerClass
	}{
		{"group", &tg.PeerChat{ChatID: 123}, &tg.ChatForbidden{ID: 123, Title: "Unavailable group"}, "group", &tg.InputPeerChat{ChatID: 123}},
		{"supergroup", &tg.PeerChannel{ChannelID: 456}, channel, "supergroup", &tg.InputPeerChannel{ChannelID: 456, AccessHash: 789}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapped, err := MapDialog(&tg.Dialog{Peer: tc.peer, TopMessage: 17}, nil, []tg.ChatClass{tc.entity})
			require.NoError(t, err)
			assert.Equal(t, tc.kind, mapped.Kind)
			assert.Equal(t, 17, mapped.TopMessage)
			assert.True(t, mapped.Inaccessible)
			assert.False(t, mapped.EnabledByDefault)
			assert.Equal(t, "Telegram dialog is inaccessible", mapped.UnsupportedReason)
			input, err := mapped.InputPeer()
			require.NoError(t, err)
			assert.Equal(t, tc.input, input)

			observedAt := time.Unix(100, 0).UTC()
			state := telegram.NewSyncState()
			state.Dialogs[strconv.FormatInt(mapped.ChatID, 10)] = telegram.DialogState{Frontier: 11}
			require.NoError(t, RefreshCatalog(context.Background(), state, []CatalogDialog{mapped}, observedAt, func(context.Context, *telegram.SyncState) error { return nil }))
			stored := state.Dialogs[strconv.FormatInt(mapped.ChatID, 10)]
			assert.Equal(t, 11, stored.Frontier)
			require.NotNil(t, stored.Peer.InaccessibleSince)
			assert.Equal(t, observedAt, *stored.Peer.InaccessibleSince)
			require.NoError(t, RefreshCatalog(context.Background(), state, []CatalogDialog{mapped}, observedAt.Add(time.Hour), func(context.Context, *telegram.SyncState) error { return nil }))
			assert.Equal(t, observedAt, *state.Dialogs[strconv.FormatInt(mapped.ChatID, 10)].Peer.InaccessibleSince)
		})
	}
}

func TestListDialogsUsesForbiddenPeerAsPaginationAnchor(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(t, ok)
		assert.IsType(t, &tg.InputPeerEmpty{}, request.OffsetPeer)
	}).ThenResult(&tg.MessagesDialogsSlice{
		Dialogs:  []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}},
		Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}},
		Chats:    []tg.ChatClass{&tg.ChatForbidden{ID: 123, Title: "Unavailable group"}},
	})
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(t, ok)
		assert.Equal(t, 7, request.OffsetID)
		assert.Equal(t, 100, request.OffsetDate)
		assert.Equal(t, &tg.InputPeerChat{ChatID: 123}, request.OffsetPeer)
	}).ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 456}, TopMessage: 8}}, Chats: []tg.ChatClass{&tg.Chat{ID: 456, Title: "Available group", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})

	dialogs, err := ListDialogs(context.Background(), tg.NewClient(mock))
	require.NoError(t, err)
	require.Len(t, dialogs, 2)
	assert.True(t, dialogs[0].Inaccessible)
	assert.Equal(t, int64(-123), dialogs[0].ChatID)
	assert.Equal(t, int64(-456), dialogs[1].ChatID)
	assert.True(t, mock.AllWereMet())
}

func TestCatalogDialogRejectsInvalidNativeID(t *testing.T) {
	for _, kind := range []string{"user", "group", "supergroup", "channel"} {
		_, err := (CatalogDialog{Kind: kind, NativeID: -1}).InputPeer()
		assert.Error(t, err)
	}
}

func TestListDialogsReadsMainAndArchivedFolders(t *testing.T) {
	mock := tgmock.NewRequire(t)
	for _, folder := range []int{0, 1} {
		want := folder
		mock.ExpectFunc(func(input bin.Encoder) {
			request, ok := input.(*tg.MessagesGetDialogsRequest)
			require.True(t, ok)
			got, present := request.GetFolderID()
			assert.True(t, present)
			assert.Equal(t, want, got)
			assert.IsType(t, &tg.InputPeerEmpty{}, request.OffsetPeer)
		}).ThenResult(&tg.MessagesDialogs{})
	}
	dialogs, err := ListDialogs(context.Background(), tg.NewClient(mock))
	require.NoError(t, err)
	assert.Empty(t, dialogs)
	assert.True(t, mock.AllWereMet())
}

func TestListDialogsForRangesRetainsEveryRangeForEachDialog(t *testing.T) {
	ranges := []telegram.HistoryRange{{MinID: 1, MaxID: 10}, {MinID: 11, MaxID: 20}}
	chat := &tg.Chat{ID: 123, Title: "Synthetic group", Photo: &tg.ChatPhotoEmpty{}}
	mock := tgmock.NewRequire(t)
	for _, messageRange := range ranges {
		for _, folder := range []int{0, 1} {
			wantRange, wantFolder := messageRange, folder
			mock.ExpectFunc(func(input bin.Encoder) {
				wrapped, ok := input.(*tg.InvokeWithMessagesRangeRequest)
				require.True(t, ok)
				assert.Equal(t, tg.MessageRange{MinID: wantRange.MinID, MaxID: wantRange.MaxID}, wrapped.Range)
				request, ok := wrapped.Query.(*tg.MessagesGetDialogsRequest)
				require.True(t, ok)
				gotFolder, present := request.GetFolderID()
				assert.True(t, present)
				assert.Equal(t, wantFolder, gotFolder)
			}).ThenResult(func() *tg.MessagesDialogs {
				if wantFolder == 1 {
					return &tg.MessagesDialogs{}
				}
				topMessage := 7
				if wantRange.MaxID > 10 {
					topMessage = 19
				}
				return &tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: chat.ID}, TopMessage: topMessage}}, Chats: []tg.ChatClass{chat}}
			}())
		}
	}
	dialogs, err := ListDialogsForRanges(context.Background(), tg.NewClient(mock), ranges)
	require.NoError(t, err)
	require.Len(t, dialogs, 1)
	assert.Equal(t, int64(-123), dialogs[0].ChatID)
	assert.Equal(t, 19, dialogs[0].TopMessage)
	assert.Equal(t, ranges, dialogs[0].MessageRanges)
	assert.True(t, mock.AllWereMet())
}

func TestListDialogsKeepsAdministratorMonoforumDisabledAndReportsSelection(t *testing.T) {
	ctx := context.Background()
	channel := &tg.Channel{ID: 1002147483649, Title: "Synthetic forum", Monoforum: true, Creator: true, Photo: &tg.ChatPhotoEmpty{}}
	channel.SetAccessHash(42)
	user := &tg.User{ID: 456, FirstName: "Synthetic"}
	user.SetAccessHash(99)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{
		&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: channel.ID}},
		&tg.Dialog{Peer: &tg.PeerUser{UserID: user.ID}},
	}, Chats: []tg.ChatClass{channel}, Users: []tg.UserClass{user}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	dialogs, err := ListDialogs(ctx, tg.NewClient(mock))
	require.NoError(t, err)
	require.Len(t, dialogs, 2)
	assert.Equal(t, "supergroup", dialogs[0].Kind)
	assert.False(t, dialogs[0].EnabledByDefault)
	assert.Equal(t, "user", dialogs[1].Kind)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Owner")
	require.NoError(t, err)
	state := telegram.NewSyncState()
	save := func(context.Context, *telegram.SyncState) error { return nil }
	require.NoError(t, backfillDialogsWithOptions(ctx, tg.NewClient(tgmock.NewRequire(t)), writer, state, dialogs,
		time.Unix(100, 0).UTC(), false, 0, HistoryOptions{}, save))
	err = backfillDialogsWithOptions(ctx, tg.NewClient(tgmock.NewRequire(t)), writer, state, dialogs,
		time.Unix(100, 0).UTC(), false, dialogs[0].ChatID, HistoryOptions{}, save)
	require.ErrorContains(t, err, "topic-aware history")
}

func TestListDialogsPagesByTopMessage(t *testing.T) {
	mock := tgmock.NewRequire(t)
	user := &tg.User{ID: 123}
	user.SetAccessHash(456)
	mock.Expect().ThenResult(&tg.MessagesDialogsSlice{
		Count:    2,
		Dialogs:  []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 123}, TopMessage: 7}},
		Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 123}, Date: 100, Message: "synthetic"}},
		Users:    []tg.UserClass{user},
	})
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.MessagesGetDialogsRequest)
		require.True(t, ok)
		assert.Equal(t, 7, req.OffsetID)
		assert.Equal(t, 100, req.OffsetDate)
		assert.Equal(t, &tg.InputPeerUser{UserID: 123, AccessHash: 456}, req.OffsetPeer)
		assert.True(t, req.ExcludePinned)
	}).ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	dialogs, err := ListDialogs(context.Background(), tg.NewClient(mock))
	{
		require := require.New(t)
		require.NoError(err)
	}
	require.Len(t, dialogs, 1)
	{
		assert := assert.New(t)
		assert.Equal(int64(123), dialogs[0].ChatID)
	}
	assert.True(t, mock.AllWereMet())
}

func TestListDialogsAdvancesAcrossPeersWithMatchingTopIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	user := &tg.User{ID: 123}
	user.SetAccessHash(456)
	channel := &tg.Channel{ID: 123, Title: "Example", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}
	channel.SetAccessHash(789)
	mock.Expect().ThenResult(&tg.MessagesDialogsSlice{Count: 2, Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 123}, TopMessage: 7}}, Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 123}, Date: 100}}, Users: []tg.UserClass{user}})
	mock.Expect().ThenResult(&tg.MessagesDialogsSlice{Count: 2, Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 7}}, Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100}}, Chats: []tg.ChatClass{channel}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	got, err := ListDialogs(context.Background(), tg.NewClient(mock))
	require.NoError(err)
	require.Len(got, 2)
	assert.Equal(int64(123), got[0].ChatID)
	assert.Equal(int64(-1000000000123), got[1].ChatID)
}

func TestListDialogsRejectsMissingOrRepeatedAnchor(t *testing.T) {
	user := &tg.User{ID: 123}
	user.SetAccessHash(456)
	dialog := &tg.Dialog{Peer: &tg.PeerUser{UserID: 123}, TopMessage: 7}
	message := &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 123}, Date: 100}
	t.Run("missing", func(t *testing.T) {
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.MessagesDialogsSlice{Count: 1, Dialogs: []tg.DialogClass{dialog}, Users: []tg.UserClass{user}})
		_, err := ListDialogs(context.Background(), tg.NewClient(mock))
		require.ErrorContains(t, err, "pagination message")
	})
	t.Run("repeated", func(t *testing.T) {
		mock := tgmock.NewRequire(t)
		page := &tg.MessagesDialogsSlice{Count: 1, Dialogs: []tg.DialogClass{dialog}, Users: []tg.UserClass{user}, Messages: []tg.MessageClass{message}}
		mock.Expect().ThenResult(page)
		mock.Expect().ThenResult(page)
		_, err := ListDialogs(context.Background(), tg.NewClient(mock))
		require.ErrorContains(t, err, "did not advance")
	})
}

func TestConfiguredChannelsEnableOnlySelectedBroadcasts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dialogs := []CatalogDialog{{ChatID: -1000000000123, Kind: "channel"}, {ChatID: -1000000000456, Kind: "channel"}, {ChatID: -123, Kind: "group", EnabledByDefault: true}}
	selected, err := enableConfiguredChannels(dialogs, []int64{-1000000000123})
	require.NoError(err)
	assert.True(selected[0].EnabledByDefault)
	assert.False(selected[1].EnabledByDefault)
	assert.True(selected[2].EnabledByDefault)
	assert.False(dialogs[0].EnabledByDefault)
	_, err = enableConfiguredChannels(dialogs, []int64{-123})
	require.Error(err)
}

func TestConfiguredSupergroupRemainsEnabled(t *testing.T) {
	dialog := CatalogDialog{ChatID: -1000000000789, Kind: "supergroup", EnabledByDefault: true}
	selected, err := enableConfiguredChannels([]CatalogDialog{dialog}, []int64{dialog.ChatID})
	require.NoError(t, err)
	require.Len(t, selected, 1)
	assert.True(t, selected[0].EnabledByDefault)
}
