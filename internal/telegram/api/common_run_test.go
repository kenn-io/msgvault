package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCommonDifferenceAppliesNewEditAndDeletionBeforeSaving(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", EnabledByDefault: true}
	original := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "original"}
	require.NoError(t, ArchiveAPIMessageWithWriter(ctx, writer, dialog, original, nil, nil))
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "edited synthetic"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{
		NewMessages:  []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 110, Message: "new synthetic"}},
		OtherUpdates: []tg.UpdateClass{&tg.UpdateEditMessage{Message: edited, Pts: 11, PtsCount: 1}, &tg.UpdateDeleteMessages{Messages: []int{8}, Pts: 12, PtsCount: 1}},
		State:        tg.UpdatesState{Pts: 12, Date: 200},
	})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	var saved bool
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, func(_ context.Context, next *telegram.SyncState) error {
		var editedID int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&editedID))
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), editedID).Scan(&body))
		assert.Equal(t, "edited synthetic", body)
		var messageCount int
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT c.message_count FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(&messageCount))
		assert.Equal(t, 2, messageCount, "conversation stats are refreshed before the difference cursor is saved")
		var deleted bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:8").Scan(&deleted))
		assert.True(t, deleted)
		assert.Equal(t, 12, next.Common.Pts)
		saved = true
		return nil
	})
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.True(saved)
	}
	assert.Equal(t, 12, state.Common.Pts)
	assert.True(t, mock.AllWereMet())
}

func TestCommonDifferenceRefreshesConflictingSameSecondEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", EnabledByDefault: true}

	archived := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "later snapshot"}
	archived.SetEditDate(150)
	require.NoError(ArchiveAPIMessageWithWriter(ctx, writer, dialog, archived, nil, nil))
	stale := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "earlier update"}
	stale.SetEditDate(150)
	current := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "later snapshot"}
	current.SetEditDate(150)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{
		OtherUpdates: []tg.UpdateClass{&tg.UpdateEditMessage{Message: stale, Pts: 11, PtsCount: 1}},
		State:        tg.UpdatesState{Pts: 11, Date: 151},
	})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{current}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	saved := false
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, func(_ context.Context, next *telegram.SyncState) error {
		var messageID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&messageID))
		var body string
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
		assert.Equal("later snapshot", body)
		assert.Equal(11, next.Common.Pts)
		saved = true
		return nil
	})
	require.NoError(err)
	assert.True(saved)
	assert.Equal(11, state.Common.Pts)
	assert.True(mock.AllWereMet())
}

func TestCommonDifferenceRecoversGapBeforeSavingPosition(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	now := time.Now().UTC()
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", TopMessage: 7, EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceTooLong{Pts: 20})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 21, Date: int(now.Unix())})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: int(now.Unix()), Message: "recovered"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: int(now.Unix())})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: int(now.Add(-time.Hour).Unix())}
	saves := 0
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, func(_ context.Context, next *telegram.SyncState) error {
		if saves == 0 {
			var count int
			require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
			assert.Equal(t, 1, count)
			assert.Equal(t, 21, next.Common.Pts)
			assert.Equal(t, 1, next.Gaps["common"].Count)
		}
		saves++
		return nil
	})
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.Equal(2, saves)
	}
	assert.Equal(t, 21, state.Common.Pts)
	assert.Equal(t, 1, state.Gaps["common"].Count)
	assert.True(t, mock.AllWereMet())
}

func TestCommonGapRecoverySaveFailureKeepsPreviousPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceTooLong{Pts: 20})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 21, Date: 200})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, nil, func(context.Context, *telegram.SyncState) error {
		return errors.New("save failed")
	})
	require.ErrorContains(err, "save failed")
	assert.Equal(10, state.Common.Pts)
	assert.Empty(state.Gaps)
	assert.True(mock.AllWereMet())
}

func TestCommonDifferenceDoesNotAdvanceForUnknownDialog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{NewMessages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}, State: tg.UpdatesState{Pts: 11, Date: 101}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	saved := false
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, nil, func(context.Context, *telegram.SyncState) error { saved = true; return nil })
	require.ErrorContains(err, "catalog")
	assert.False(saved)
	assert.Equal(10, state.Common.Pts)
}

func TestCommonDifferenceHonorsAfterBound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", EnabledByDefault: true}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{NewMessages: []tg.MessageClass{
		&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "boundary"},
		&tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 101, Message: "included"},
	}, State: tg.UpdatesState{Pts: 11, Date: 102}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	err = runCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(100, 0), func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
	assert.True(mock.AllWereMet())
}

func TestCommonDifferenceAppliesOlderEditsWithAfterBound(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", EnabledByDefault: true}
	original := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 50, Message: "original"}
	require.NoError(t, ArchiveAPIMessageWithWriter(ctx, writer, dialog, original, nil, nil))
	require.NoError(t, writer.FlushConversationStats(ctx))
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 50, Message: "edited synthetic"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{OtherUpdates: []tg.UpdateClass{&tg.UpdateEditMessage{Message: edited, Pts: 11, PtsCount: 1}}, State: tg.UpdatesState{Pts: 11, Date: 151}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	saved := false
	err = runCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(100, 0), func(_ context.Context, next *telegram.SyncState) error {
		var id int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
		assert.Equal(t, "edited synthetic", body)
		assert.Equal(t, 11, next.Common.Pts)
		saved = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, saved)
	assert.True(t, mock.AllWereMet())
}

func TestCommonDifferenceSkipsUnseenOlderEditsWithAfterBound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", EnabledByDefault: true}
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 50, Message: "old unseen edit"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{OtherUpdates: []tg.UpdateClass{
		&tg.UpdateEditMessage{Message: edited, Pts: 11, PtsCount: 1},
	}, State: tg.UpdatesState{Pts: 11, Date: 151}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	err = runCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, time.Unix(100, 0), func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Zero(count, "--after must not backfill a message solely because it has a recent edit")
	assert.True(mock.AllWereMet())
}

func TestCommonDifferenceLeavesChannelUpdatesForChannelCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", EnabledByDefault: true}
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "channel"}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{OtherUpdates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: message, Pts: 11, PtsCount: 1}}, State: tg.UpdatesState{Pts: 20, Date: 100}})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 19, Date: 100}
	channelPTS := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &channelPTS}
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, []CatalogDialog{dialog}, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Equal(20, state.Common.Pts)
	assert.Equal(10, *state.Dialogs["-1000000000123"].ChannelPTS)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&count))
	assert.Equal(1, count)
	assert.True(mock.AllWereMet())
}

func TestCommonDifferenceDrainsSlicesInOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceSlice{IntermediateState: tg.UpdatesState{Pts: 11, Date: 101}})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 102})
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 10, Date: 100}
	var positions []int
	err = RunCommonDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, nil, func(_ context.Context, next *telegram.SyncState) error {
		positions = append(positions, next.Common.Pts)
		return nil
	})
	require.NoError(err)
	assert.Equal([]int{11, 11}, positions)
	assert.Equal(102, state.Common.Date)
	assert.True(mock.AllWereMet())
}
