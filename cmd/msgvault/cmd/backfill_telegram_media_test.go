package cmd

import (
	"context"
	"os"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/telegram"
	telegramapi "go.kenn.io/msgvault/internal/telegram/api"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBackfillTelegramMediaUsesIndependentSyncRun(t *testing.T) {
	require := require.New(t)

	{
		assert := assert.New(t)
		assert.True(manualSyncCLICommand([]string{"backfill-telegram-media"}))
	}
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0700))
	require.NoError(telegramapi.NewSessionStore(dir, 123, "synthetic-hash", 999).StoreSession(ctx, []byte("synthetic-session")))
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	connect := func(ctx context.Context, _ *telegramapi.SessionStore, run func(context.Context, *tg.Client) error) error {
		download := telegramMediaDownload(func(context.Context, *telegramapi.MediaReference, string, int64) (*os.File, error) {
			assert.Fail(t, "unexpected download")
			return nil, nil
		})
		return run(context.WithValue(ctx, telegramMediaTransportKey{}, download), tg.NewClient(mock))
	}
	options := telegramapi.MediaOptions{AttachmentsDir: t.TempDir(), TempDir: dir, Policy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 20}}
	_, err = backfillTelegramSourceMedia(ctx, st, source, dir, options, connect)
	require.NoError(err)
	_, err = st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "backfill-telegram-media")
	require.NoError(err)
	_, err = st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.Error(err)
	assert.True(t, mock.AllWereMet())
}
