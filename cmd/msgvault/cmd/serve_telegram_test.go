package cmd

import (
	"context"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
	"golang.org/x/time/rate"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScheduledTelegramSelectsExactIdentifier(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Synthetic Account"))
	sources, retired, err := findScheduledSyncSources(st, source.Identifier)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(source.ID, sources[0].ID)
	assert.False(retired)
	sources, _, err = findScheduledSyncSources(st, "Synthetic Account")
	require.NoError(err)
	assert.Empty(sources)
}

func TestScheduledTelegramUsesAuthenticatedSync(t *testing.T) {
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
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	deps := telegramSyncDeps{
		tokensDir: func() string { return dir }, attachmentsDir: t.TempDir,
		mediaPolicy: func(string) attachmentpolicy.Policy {
			return attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone, MaxBytes: 20}
		},
		connect: func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
			download := telegramMediaDownload(func(context.Context, *telegramapi.MediaReference, string, int64) (*os.File, error) {
				assert.Fail(t, "unexpected download")
				return nil, nil
			})
			return run(context.WithValue(ctx, telegramMediaTransportKey{}, download), tg.NewClient(mock))
		},
	}
	require.NoError(importTelegramSourceForScheduledRun(ctx, st, source, deps))
	_, err = st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(err)
	assert.True(t, mock.AllWereMet())
}

func TestTelegramRequestLimitsUseInvocationConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := config.NewDefaultConfig()
	cfg.Telegram.RequestsPerSecond, cfg.Telegram.MaxFloodWait = 0.5, 12*time.Second
	limiter, wait, err := telegramRequestLimits(withInvocation(context.Background(), &invocation{cfg: cfg}))
	require.NoError(err)
	assert.Equal(rate.Limit(0.5), limiter.Limit())
	assert.Equal(12*time.Second, wait)
	cfg.Telegram.RequestsPerSecond = 0
	_, _, err = telegramRequestLimits(withInvocation(context.Background(), &invocation{cfg: cfg}))
	require.Error(err)
}

func TestScheduledTelegramIncludesConfiguredBroadcast(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0700))
	require.NoError(telegramapi.NewSessionStore(dir, 123, "synthetic-hash", 999).StoreSession(ctx, []byte("synthetic-session")))
	channel := &tg.Channel{ID: 123, Title: "Synthetic", Broadcast: true, Photo: &tg.ChatPhotoEmpty{}}
	channel.SetAccessHash(456)
	dialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}}
	dialog.SetPts(10)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 100})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{dialog}, Chats: []tg.ChatClass{channel}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 100})
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceEmpty{Pts: 10, Final: true})
	deps := telegramSyncDeps{tokensDir: func() string { return dir }, attachmentsDir: t.TempDir,
		channels: func(string) []int64 { return []int64{-1000000000123} },
		mediaPolicy: func(string) attachmentpolicy.Policy {
			return attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone, MaxBytes: 20}
		},
		connect: func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
			download := telegramMediaDownload(func(context.Context, *telegramapi.MediaReference, string, int64) (*os.File, error) {
				assert.Fail(t, "unexpected download")
				return nil, nil
			})
			return run(context.WithValue(ctx, telegramMediaTransportKey{}, download), tg.NewClient(mock))
		},
	}
	require.NoError(importTelegramSourceForScheduledRun(ctx, st, source, deps))
	run, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(err)
	state, err := telegram.LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	require.NotNil(state.Dialogs["-1000000000123"].ChannelPTS)
	{
		assert := assert.New(t)
		assert.Equal(10, *state.Dialogs["-1000000000123"].ChannelPTS)
	}
	assert.True(t, mock.AllWereMet())
}
