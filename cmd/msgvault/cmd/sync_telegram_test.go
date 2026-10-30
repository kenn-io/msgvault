package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveTelegramSyncSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, err := resolveTelegramSyncSources(st, "")
	require.ErrorContains(err, "add-telegram")
	first, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(111))
	require.NoError(err)
	second, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(222))
	require.NoError(err)
	_, err = st.GetOrCreateSource("discord", "123")
	require.NoError(err)
	sources, err := resolveTelegramSyncSources(st, "")
	require.NoError(err)
	require.Len(sources, 2)
	assert.Equal(first.ID, sources[0].ID)
	assert.Equal(second.ID, sources[1].ID)
	sources, err = resolveTelegramSyncSources(st, "user222")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(second.ID, sources[0].ID)
	_, err = resolveTelegramSyncSources(st, "222")
	require.Error(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?)`), first.ID, second.ID)
	require.NoError(err)
	sources, err = resolveTelegramSyncSources(st, "")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(second.ID, sources[0].ID)
	_, err = resolveTelegramSyncSources(st, "user111")
	require.ErrorContains(err, "retired")
}

func TestTelegramSyncUsesDaemonCacheAndHelpGroup(t *testing.T) {
	assert.True(t, manualSyncCLICommand([]string{"sync-telegram", "--full"}))
	assert.Equal(t, helpGroupSources, topLevelCommandGroups["sync-telegram"])
}

func TestSyncTelegramSourceUsesSavedSessionAndArchivesHistory(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	session := telegramapi.NewSessionStore(dir, 123, "synthetic-hash", 999)
	require.NoError(t, session.StoreSession(ctx, []byte("synthetic-session")))
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	connect := func(ctx context.Context, session *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
		data, err := session.LoadSession(ctx)
		require.NoError(t, err)
		assert.Equal(t, []byte("synthetic-session"), data)
		return run(ctx, tg.NewClient(mock))
	}
	require.NoError(t, syncTelegramSource(ctx, st, source, dir, time.Unix(100, 0), false, 0, telegramapi.HistoryOptions{}, connect))
	{
		assert := assert.New(t)
		assert.True(mock.AllWereMet())
	}
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(t, 1, count)
	run, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(t, err)
	assert.Equal(t, int64(1), run.MessagesProcessed)
	assert.Equal(t, int64(1), run.MessagesAdded)
	mockAgain := tgmock.NewRequire(t)
	mockAgain.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mockAgain.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mockAgain.Expect().ThenResult(&tg.MessagesDialogs{})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{})
	mockAgain.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	connectAgain := func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
		return run(ctx, tg.NewClient(mockAgain))
	}
	require.NoError(t, syncTelegramSource(ctx, st, source, dir, time.Unix(200, 0), false, 0, telegramapi.HistoryOptions{Full: true}, connectAgain))
	run, err = st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(t, err)
	assert.Equal(t, int64(1), run.MessagesProcessed)
	assert.Zero(t, run.MessagesAdded)
	assert.True(t, mockAgain.AllWereMet())
}

func TestSyncTelegramSourceRejectsInvalidIdentifier(t *testing.T) {
	st := testutil.NewTestStore(t)
	connect := func(context.Context, *telegramapi.SessionStore, func(context.Context, *tg.Client) error) error {
		assert.Fail(t, "connection must not start for an invalid source")
		return nil
	}
	err := syncTelegramSource(context.Background(), st, &store.Source{ID: 1, SourceType: telegram.SourceType, Identifier: "user01"}, t.TempDir(), time.Time{}, false, 0, telegramapi.HistoryOptions{}, connect)
	require.ErrorContains(t, err, "identifier")
}

func TestSyncTelegramSourceRecordsConnectionFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0700))
	require.NoError(telegramapi.NewSessionStore(dir, 123, "synthetic-hash", 999).StoreSession(ctx, []byte("synthetic-session")))
	connectionErr := errors.New("synthetic connection failure")
	connect := func(context.Context, *telegramapi.SessionStore, func(context.Context, *tg.Client) error) error {
		return connectionErr
	}
	require.ErrorIs(syncTelegramSource(ctx, st, source, dir, time.Unix(100, 0), false, 0, telegramapi.HistoryOptions{}, connect), connectionErr)
	run, err := st.GetLatestSyncContext(ctx, source.ID, 0)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.True(run.ErrorMessage.Valid)
	assert.Contains(run.ErrorMessage.String, "synthetic connection failure")
}

func TestSyncTelegramCommandArchivesSelectedChat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0700))
	require.NoError(telegramapi.NewSessionStore(dir, 123, "synthetic-hash", 999).StoreSession(ctx, []byte("synthetic-session")))
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.Expect().ThenResult(&tg.MessageRangeVector{Elems: []tg.MessageRange{{MinID: 1, MaxID: 100}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	mock.Expect().ThenResult(&tg.BoolTrue{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	cacheRefreshed := false
	deps := telegramSyncDeps{
		openStore: func() (*store.Store, func(), error) { return st, func() {}, nil },
		tokensDir: func() string { return dir },
		connect: func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
			return run(ctx, tg.NewClient(mock))
		},
		refreshCache: func() error { cacheRefreshed = true; return nil },
	}
	command := newSyncTelegramLocalCmd(deps)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--takeout", "--full", "--after", "1970-01-01T00:01:39Z", "--chat", "-123", "user999"})
	require.NoError(command.ExecuteContext(ctx))
	assert.True(mock.AllWereMet())
	assert.True(cacheRefreshed)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count))
	assert.Equal(1, count)
	mockAgain := tgmock.NewRequire(t)
	mockAgain.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mockAgain.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChat{ChatID: 123}, TopMessage: 7}}, Chats: []tg.ChatClass{&tg.Chat{ID: 123, Title: "Synthetic", Photo: &tg.ChatPhotoEmpty{}}}})
	mockAgain.Expect().ThenResult(&tg.MessagesDialogs{})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}, &tg.Message{ID: 6, PeerID: &tg.PeerChat{ChatID: 123}, Date: 75, Message: "older synthetic"}}})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{})
	mockAgain.Expect().ThenResult(&tg.MessagesMessages{})
	mockAgain.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	deps.connect = func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
		return run(ctx, tg.NewClient(mockAgain))
	}
	command = newSyncTelegramLocalCmd(deps)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--after", "1970-01-01T00:00:50Z", "--chat", "-123", "user999"})
	require.NoError(command.ExecuteContext(ctx))
	assert.True(mockAgain.AllWereMet())
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(2, count)
}
