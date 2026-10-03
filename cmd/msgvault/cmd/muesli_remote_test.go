package cmd

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMuesliLocalReaderToRemoteArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = false
	cfg.Server.APIKey = "synthetic-owner-key"
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: adapter.logger, OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	registered, err := client.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	require.NoError(err)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT, start_time TEXT, created_at TEXT, meeting_status TEXT, raw_transcript TEXT, manual_notes TEXT, deleted_at TEXT);
 INSERT INTO meetings VALUES (42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','completed','Initial synthetic transcript','',NULL);
 INSERT INTO meetings VALUES (43,'Unfinished','2026-09-01T14:00:00Z','2026-09-01 14:00:04','processing','unfinished','',NULL)`)
	require.NoError(err)
	options := muesli.ImportOptions{Identifier: source.Identifier, AccountEmail: source.AccountEmail, DBPath: path, LockDir: t.TempDir()}
	scan := func() *muesli.ImportSummary {
		summary, err := muesli.ScanRemote(t.Context(), options, client.ImportMuesli)
		require.NoError(err)
		return summary
	}
	assert.Equal(int64(1), scan().MeetingsAdded)
	unchanged := scan()
	assert.Zero(unchanged.MeetingsUpdated)
	assert.Equal(int64(1), unchanged.SkippedInProgress)
	_, err = db.Exec(`UPDATE meetings SET raw_transcript='Edited synthetic transcript',manual_notes='Later notes' WHERE id=42`)
	require.NoError(err)
	assert.Equal(int64(1), scan().MeetingsUpdated)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=?`), registered.SourceID).Scan(&messageID))
	detail, err := st.GetMessage(messageID)
	require.NoError(err)
	assert.Contains(detail.Body, "Edited synthetic transcript")
	assert.Contains(detail.Body, "Later notes")
	_, err = db.Exec(`UPDATE meetings SET deleted_at='2026-09-02T00:00:00Z' WHERE id=42`)
	require.NoError(err)
	assert.Equal(int64(1), scan().SkippedDeleted)
	detail, err = st.GetMessage(messageID)
	require.NoError(err)
	assert.Contains(detail.Body, "Later notes")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.ImportMuesli(canceled, muesli.RemoteRequest{Action: "register", Source: source})
	require.ErrorIs(err, context.Canceled)
}
