package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChannelDifferenceAppliesEffectsBeforeSaving(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", Title: "Synthetic", EnabledByDefault: true}
	original := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "original"}
	require.NoError(t, ArchiveAPIMessageWithWriter(ctx, writer, dialog, original, nil, nil))
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "edited synthetic"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 12, Final: true,
		NewMessages:  []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 110, Message: "new synthetic"}},
		OtherUpdates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: edited, Pts: 11, PtsCount: 1}, &tg.UpdateDeleteChannelMessages{ChannelID: 123, Messages: []int{8}, Pts: 12, PtsCount: 1}},
	})
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	var saved bool
	err = RunChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, func(_ context.Context, next *telegram.SyncState) error {
		var editedID int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&editedID))
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), editedID).Scan(&body))
		assert.Equal(t, "edited synthetic", body)
		var messageCount int
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT c.message_count FROM conversations c JOIN messages m ON m.conversation_id = c.id WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&messageCount))
		assert.Equal(t, 2, messageCount, "conversation stats are refreshed before the channel cursor is saved")
		var deleted bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:8").Scan(&deleted))
		assert.True(t, deleted)
		assert.Equal(t, 12, *next.Dialogs["-1000000000123"].ChannelPTS)
		saved = true
		return nil
	})
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.True(saved)
	}
	assert.Equal(t, 12, *state.Dialogs["-1000000000123"].ChannelPTS)
	assert.True(t, mock.AllWereMet())
}

func TestChannelDifferenceRecoversGapAndProcessesResponse(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", Title: "Synthetic", TopMessage: 7}
	remote := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 7}
	remote.SetPts(20)
	now := int(time.Now().Unix())
	oldEdit := &tg.Message{ID: 6, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 50, Message: "unseen old edit"}
	oldEdit.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceTooLong{Final: true, Dialog: remote, Messages: []tg.MessageClass{
		oldEdit,
		&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: now, Message: "recovered"},
	}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceEmpty{Pts: 20, Final: true})
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	var saves int
	err = runChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, time.Unix(100, 0), func(_ context.Context, next *telegram.SyncState) error {
		if saves == 0 {
			var count int
			require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
			assert.Equal(t, 1, count)
			assert.Equal(t, 20, *next.Dialogs["-1000000000123"].ChannelPTS)
			assert.Equal(t, 1, next.Gaps["-1000000000123"].Count)
		}
		saves++
		return nil
	})
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.Equal(2, saves)
	}
	assert.Equal(t, 1, state.Gaps["-1000000000123"].Count)
	assert.True(t, mock.AllWereMet())
}

func TestChannelGapRecoverySaveFailureKeepsPreviousPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"}
	remote := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}}
	remote.SetPts(20)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceTooLong{Final: true, Dialog: remote})
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	err = RunChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, func(context.Context, *telegram.SyncState) error {
		return errors.New("save failed")
	})
	require.ErrorContains(err, "save failed")
	assert.Equal(10, *state.Dialogs["-1000000000123"].ChannelPTS)
	assert.Empty(state.Gaps)
	assert.True(mock.AllWereMet())
}

func TestChannelDifferenceHonorsAfterBound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"}
	oldEdit := &tg.Message{ID: 9, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 50, Message: "unseen old edit"}
	oldEdit.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 11, Final: true, NewMessages: []tg.MessageClass{
		&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 100, Message: "boundary"},
		&tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 101, Message: "included"},
	}, OtherUpdates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: oldEdit, Pts: 11, PtsCount: 1}}})
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	err = runChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, time.Unix(100, 0), func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
	assert.True(mock.AllWereMet())
}

func TestChannelDifferenceAppliesOlderEditsWithAfterBound(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", Title: "Synthetic", EnabledByDefault: true}
	original := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 50, Message: "original"}
	require.NoError(t, ArchiveAPIMessageWithWriter(ctx, writer, dialog, original, nil, nil))
	require.NoError(t, writer.FlushConversationStats(ctx))
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Date: 50, Message: "edited synthetic"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 11, Final: true, OtherUpdates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: edited, Pts: 11, PtsCount: 1}}})
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
	saved := false
	err = runChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, time.Unix(100, 0), func(_ context.Context, next *telegram.SyncState) error {
		var id int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&id))
		var body string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
		assert.Equal(t, "edited synthetic", body)
		assert.Equal(t, 11, *next.Dialogs["-1000000000123"].ChannelPTS)
		saved = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, saved)
	assert.True(t, mock.AllWereMet())
}

func TestChannelDifferenceRecordsLostAccessWithoutAdvancing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenRPCErr(tgerr.New(400, "CHANNEL_PRIVATE"))
	state := telegram.NewSyncState()
	pts := 10
	state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts, Peer: &telegram.DialogPeer{NativeID: 123, Kind: "supergroup"}}
	err = RunChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, func(context.Context, *telegram.SyncState) error { return nil })
	require.NoError(err)
	assert.Equal(10, *state.Dialogs["-1000000000123"].ChannelPTS)
	assert.NotNil(state.Dialogs["-1000000000123"].Peer.InaccessibleSince)
	assert.True(mock.AllWereMet())
}

func TestChannelDifferenceDrainsPagesAndRejectsWrongPeer(t *testing.T) {
	for _, wrongPeer := range []bool{false, true} {
		t.Run(map[bool]string{false: "pages", true: "wrong peer"}[wrongPeer], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			ctx := context.Background()
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
			require.NoError(err)
			writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
			require.NoError(err)
			dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", Title: "Synthetic"}
			mock := tgmock.NewRequire(t)
			if wrongPeer {
				mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 11, Final: true, NewMessages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 124}, Date: 100, Message: "synthetic"}}})
			} else {
				mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 11})
				mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceEmpty{Pts: 12, Final: true})
			}
			state := telegram.NewSyncState()
			pts := 10
			state.Dialogs["-1000000000123"] = telegram.DialogState{ChannelPTS: &pts}
			var positions []int
			err = RunChannelDifferences(ctx, st, source.ID, tg.NewClient(mock), writer, state, dialog, func(_ context.Context, next *telegram.SyncState) error {
				positions = append(positions, *next.Dialogs["-1000000000123"].ChannelPTS)
				return nil
			})
			if wrongPeer {
				require.ErrorContains(err, "dialog")
				assert.Empty(positions)
				assert.Equal(10, *state.Dialogs["-1000000000123"].ChannelPTS)
			} else {
				require.NoError(err)
				assert.Equal([]int{11, 12}, positions)
			}
			assert.True(mock.AllWereMet())
		})
	}
}
