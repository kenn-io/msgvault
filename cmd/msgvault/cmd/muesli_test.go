package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveMuesliSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := &config.Config{}
	_, err := resolveMuesliSources(nil, cfg)
	require.Error(err)
	assert.Contains(err.Error(), "[[muesli]]")

	cfg = &config.Config{Muesli: []config.MuesliSource{
		{Identifier: "mac", AccountEmail: "you@example.com"},
		{Identifier: "studio", AccountEmail: "you@example.com"},
	}}
	all, err := resolveMuesliSources(nil, cfg)
	require.NoError(err)
	assert.Len(all, 2)

	one, err := resolveMuesliSources([]string{"STUDIO"}, cfg)
	require.NoError(err)
	require.Len(one, 1)
	assert.Equal("studio", one[0].Identifier)

	_, err = resolveMuesliSources([]string{"laptop"}, cfg)
	require.Error(err)
	assert.Contains(err.Error(), "configured: mac, studio")
}

func TestProbeMuesliDatabaseRejectsForeignFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("not sqlite"), 0o600))

	err := probeMuesliDatabase(context.Background(), path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "db_path")
}

func TestRunConfiguredMuesliSyncRefusesUnregisteredSource(t *testing.T) {
	st := testutil.NewTestStore(t)

	err := runConfiguredMuesliSync(context.Background(), st, config.MuesliSource{
		Identifier: "removed", AccountEmail: "you@example.com",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-muesli removed")
}

func TestServeScheduledMuesliSyncCompletes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.APIPort = freeTCPPort(t)
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Analytics.AutoBuildCache = false
	cfg.Vector.Enabled = false
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT, start_time TEXT, created_at TEXT, raw_transcript TEXT);
		INSERT INTO meetings VALUES (1, 'Planning', '2026-09-01T14:00:00Z', '2026-09-01 14:00:03', 'Synthetic meeting notes')`)
	require.NoError(err)
	require.NoError(db.Close())
	contacts := false
	cfg.Muesli = []config.MuesliSource{{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: path,
		Contacts: &contacts, Enabled: true, Schedule: "0 0 1 1 *",
	}}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource(muesli.SourceType, "mac")
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(err)
		case <-time.After(serveLifecycleTestTimeout):
			require.FailNow("daemon did not stop")
		}
	})
	waitForServeHealth(t, cfg.Server.APIPort, errCh)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.APIPort)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post(baseURL+"/api/v1/sync/mac?source_type=muesli", "application/json", nil)
	require.NoError(err)
	require.NoError(response.Body.Close())
	require.Equal(http.StatusAccepted, response.StatusCode)
	var status api.SourceStatusResponse
	require.Eventually(func() bool {
		response, err := client.Get(baseURL + "/api/v1/sources/status?source_type=muesli")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return json.UnmarshalRead(response.Body, &status) == nil && len(status.Sources) == 1 &&
			status.Sources[0].LastSuccessfulSync != nil && status.Sources[0].CanSync
	}, serveLifecycleTestTimeout, 20*time.Millisecond, "scheduled import did not finish")
	assert.Equal(int64(1), status.Sources[0].LastSuccessfulSync.MessagesAdded)
	assert.Empty(status.Sources[0].SchedulerLastError, "post-import cache refresh must receive the daemon configuration")
}

func TestFinishMuesliImportRefreshesCacheAfterPartialWrites(t *testing.T) {
	refreshed := 0
	err := finishMuesliImport("mac", &muesli.ImportSummary{MeetingsAdded: 1},
		errors.New("meeting 3 failed"), func() error { refreshed++; return nil })

	require.Error(t, err)
	assert.Equal(t, 1, refreshed)
	assert.Contains(t, err.Error(), "muesli sync mac failed")
}

func TestWriteMuesliSummaryReportsSkippedMeetings(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{
		MeetingsProcessed: 3, MeetingsAdded: 2, SkippedDeleted: 4, SkippedEmpty: 1, SkippedInProgress: 1,
	})

	assert.Contains(t, out.String(), "Deleted in Muesli:  4 (kept archived)")
	assert.Contains(t, out.String(), "Empty:              1 (no notes or transcript)")
	assert.Contains(t, out.String(), "Still in progress:  1")
}

func TestWriteMuesliSummaryOmitsZeroSkipCounts(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{MeetingsProcessed: 1, MeetingsAdded: 1})

	assert.NotContains(t, out.String(), "Deleted in Muesli")
	assert.NotContains(t, out.String(), "Empty:")
}

func TestWriteMuesliSummaryReportsContactsState(t *testing.T) {
	var out bytes.Buffer

	writeMuesliSummary(&out, &muesli.ImportSummary{ContactsState: muesli.ContactsUnavailable})

	assert.Contains(t, out.String(), "Contacts:           unavailable")
	assert.Contains(t, out.String(), "Full Disk Access")
}

func TestMuesliImportOptionsCarryContactsSettings(t *testing.T) {
	enabled := false
	opts := muesliImportOptions(config.MuesliSource{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: "/tmp/muesli.db",
		Contacts: &enabled, ContactsPath: "/tmp/AddressBook", PhoneCountryCode: "44",
	})

	assert.Equal(t, muesli.ImportOptions{
		Identifier: "mac", AccountEmail: "you@example.com", DBPath: "/tmp/muesli.db",
		ContactsEnabled: false, ContactsPath: "/tmp/AddressBook", PhoneCountryCode: "44",
	}, opts)
}

func TestMuesliCommandsUseInvocationConfiguration(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := &config.Config{Muesli: []config.MuesliSource{{Identifier: "mac"}}}
	for _, command := range []*cobra.Command{addMuesliCmd, syncMuesliCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))

			err := command.RunE(cmd, []string{"missing"})

			assert.ErrorContains(t, err, `no [[muesli]] entry with identifier "missing" (configured: mac)`)
		})
	}
}
