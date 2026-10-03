package cmd

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMuesliCommandsReadRecorderConfiguration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	serverCfg := config.NewDefaultConfig()
	serverCfg.HomeDir = t.TempDir()
	serverCfg.Data.DataDir = serverCfg.HomeDir
	serverCfg.Analytics.AutoBuildCache = false
	serverCfg.Server.APIKey = "synthetic-owner-key"
	adapter := &storeAPIAdapter{store: st, config: serverCfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServer(serverCfg, adapter, nil, adapter.logger).Router())
	defer server.Close()
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT);INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
	require.NoError(err)
	enabledContacts := false
	clientCfg := config.NewDefaultConfig()
	clientCfg.HomeDir = t.TempDir()
	clientCfg.Data.DataDir = clientCfg.HomeDir
	clientCfg.Remote = config.RemoteConfig{URL: server.URL, APIKey: serverCfg.Server.APIKey, AllowInsecure: true}
	clientCfg.Muesli = []config.MuesliSource{{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Contacts: &enabledContacts}}
	for _, command := range []*cobra.Command{addMuesliCmd, syncMuesliCmd} {
		invocationCmd := &cobra.Command{Use: command.Use}
		invocationCmd.SetContext(testInvocationContext(t.Context(), clientCfg, invocationOptions{}))
		invocationCmd.SetOut(io.Discard)
		require.NoError(command.RunE(invocationCmd, []string{"recorder"}))
	}
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)
	_, err = db.Exec(`UPDATE meetings SET raw_transcript='Synthetic hook edit' WHERE id=42; INSERT INTO meetings VALUES(43,'Other meeting','2026-09-01T15:00:00Z','2026-09-01 15:00:03','Other synthetic transcript')`)
	require.NoError(err)
	hook := &cobra.Command{Use: "muesli-hook"}
	hook.SetContext(testInvocationContext(t.Context(), clientCfg, invocationOptions{}))
	hook.SetIn(strings.NewReader(`{"schemaVersion":1,"event":"meeting.completed","kind":"meeting","id":42,"completedAt":"2026-09-01T16:00:00Z"}`))
	require.NoError(muesliHookCmd.RunE(hook, nil))
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count))
	assert.Equal(1, count, "completion hook must select the named meeting only")
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	assert.Contains(body, "Synthetic hook edit")
	previousID := syncMuesliMeetingID
	t.Cleanup(func() { syncMuesliMeetingID = previousID })
	syncMuesliMeetingID = 42
	command := &cobra.Command{Use: "sync-muesli"}
	command.SetContext(testInvocationContext(t.Context(), clientCfg, invocationOptions{}))
	command.SetOut(io.Discard)
	require.NoError(syncMuesliCmd.RunE(command, []string{"recorder"}))
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count))
	assert.Equal(1, count, "remote meeting selector must not scan the whole database")
}

func TestMuesliRecordErrorsDoNotMaskTransportFailure(t *testing.T) {
	assert.True(t, muesliRecordErrorsOnly(errors.Join(muesli.ErrRemoteValidation, muesli.ErrRemoteTooLarge)))
	assert.False(t, muesliRecordErrorsOnly(errors.Join(muesli.ErrRemoteValidation, context.Canceled)))
	assert.False(t, muesliRecordErrorsOnly(errors.New("server failure")))
}
