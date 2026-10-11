package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meetingimport"
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

func TestMuesliBuildCacheRefreshesThroughRegisteredSource(t *testing.T) {
	type source struct {
		identifier string
		meetings   bool
	}
	for _, tc := range []struct {
		name            string
		sources         []source
		failAfterCommit bool
		wantErrCode     string
		wantBuilds      []buildCacheMode
		wantArchive     int
	}{
		{"earlier source committed before a later failure", []source{{"recorder", true}, {"unregistered", true}}, false, "source_not_found", []buildCacheMode{buildCacheModeAuto}, 1},
		{"nothing committed before a failure", []source{{"unregistered", true}, {"recorder", true}}, false, "source_not_found", nil, 0},
		{"empty unregistered source comes first", []source{{"unregistered", false}, {"recorder", false}}, false, "", []buildCacheMode{buildCacheModeAuto}, 0},
		{"upload committed but its response failed", []source{{"recorder", true}}, true, "internal_error", []buildCacheMode{buildCacheModeAuto}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			serverCfg := config.NewDefaultConfig()
			serverCfg.HomeDir = t.TempDir()
			serverCfg.Data.DataDir = serverCfg.HomeDir
			serverCfg.Analytics.AutoBuildCache = false
			serverCfg.Server.APIKey = "synthetic-owner-key"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			modes := make(chan buildCacheMode, 10)
			jobs := newCacheBuildJobs(ctx, nil, func(_ context.Context, mode buildCacheMode) error { modes <- mode; return nil })
			adapter := &storeAPIAdapter{store: st, config: serverCfg, cacheJobs: jobs, logger: slog.New(slog.DiscardHandler)}
			router := api.NewServer(serverCfg, adapter, nil, adapter.logger).Router()
			handler := router
			if tc.failAfterCommit {
				// The daemon commits the first upload, then the response is lost
				// to a failure, as when checkpoint or link maintenance fails.
				failed := false
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if !assert.NoError(err) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					if failed || !bytes.Contains(body, []byte(`"action":"upsert"`)) {
						router.ServeHTTP(w, r)
						return
					}
					failed = true
					router.ServeHTTP(httptest.NewRecorder(), r)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal_error","message":"Muesli import failed"}`))
				})
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			clientCfg := config.NewDefaultConfig()
			clientCfg.HomeDir = t.TempDir()
			clientCfg.Data.DataDir = clientCfg.HomeDir
			clientCfg.Remote = config.RemoteConfig{URL: server.URL, APIKey: serverCfg.Server.APIKey, AllowInsecure: true}
			disabled := false
			for _, configured := range tc.sources {
				path := filepath.Join(t.TempDir(), "muesli.db")
				db, err := sql.Open("sqlite3", path)
				require.NoError(err)
				_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT)`)
				require.NoError(err)
				if configured.meetings {
					_, err = db.Exec(`INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
					require.NoError(err)
				}
				require.NoError(db.Close())
				clientCfg.Muesli = append(clientCfg.Muesli, config.MuesliSource{Identifier: configured.identifier, AccountEmail: "user@example.com", DBPath: path, Contacts: &disabled})
			}
			register := &cobra.Command{Use: "add-muesli"}
			register.SetContext(testInvocationContext(t.Context(), clientCfg, invocationOptions{}))
			register.SetOut(io.Discard)
			require.NoError(addMuesliCmd.RunE(register, []string{"recorder"}))

			command := addManualSyncCacheFlags(&cobra.Command{Use: "sync-muesli"})
			require.NoError(command.Flags().Set("build-cache", "true"))
			command.SetContext(testInvocationContext(t.Context(), clientCfg, invocationOptions{}))
			command.SetOut(io.Discard)
			err := syncMuesliCmd.RunE(command, nil)
			if tc.wantErrCode == "" {
				require.NoError(err)
			} else {
				apiErr, ok := errors.AsType[*daemonclient.APIError](err)
				require.True(ok, "the failing source's error is returned: %v", err)
				assert.Equal(tc.wantErrCode, apiErr.Code)
			}

			var archived int
			require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&archived))
			assert.Equal(tc.wantArchive, archived)
			cancel()
			wait, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
			defer stop()
			require.True(jobs.waitContext(wait))
			close(modes)
			var builds []buildCacheMode
			for mode := range modes {
				builds = append(builds, mode)
			}
			assert.Equal(tc.wantBuilds, builds, "--build-cache must reach a registered source")
		})
	}
}

func TestMuesliWatchRetriesTransientDaemonFailures(t *testing.T) {
	previous := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = previous })
	type intercept func(r *http.Request, attempt int) int
	healthOnce := func(status int) intercept {
		return func(r *http.Request, attempt int) int {
			if r.URL.Path == "/api/v1/health" && attempt == 1 {
				return status
			}
			return 0
		}
	}
	rejectUploads := func(r *http.Request, _ int) int {
		if r.URL.Path == "/api/v1/import/muesli" {
			return http.StatusUnauthorized
		}
		return 0
	}
	for _, tc := range []struct {
		name       string
		outage     bool
		clientKey  string
		intercept  intercept
		wantStop   int
		wantReport int
	}{
		{"daemon unreachable at start recovers", true, "synthetic-owner-key", nil, 0, 1},
		{"throttled health probe recovers", false, "synthetic-owner-key", healthOnce(http.StatusTooManyRequests), 0, 1},
		{"rejected key at connect stops", false, "synthetic-wrong-key", nil, http.StatusUnauthorized, 0},
		{"credentials rejected after connecting stop", false, "synthetic-owner-key", rejectUploads, http.StatusUnauthorized, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			serverCfg := config.NewDefaultConfig()
			serverCfg.HomeDir = t.TempDir()
			serverCfg.Data.DataDir = serverCfg.HomeDir
			serverCfg.Analytics.AutoBuildCache = false
			serverCfg.Server.APIKey = "synthetic-owner-key"
			adapter := &storeAPIAdapter{store: st, config: serverCfg, logger: slog.New(slog.DiscardHandler)}
			_, err := muesli.NewImporter(st).ImportRemote(t.Context(), muesli.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}})
			require.NoError(err)
			router := api.NewServer(serverCfg, adapter, nil, adapter.logger).Router()
			attempts := map[string]int{}
			var mu sync.Mutex
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts[r.URL.Path]++
				attempt := attempts[r.URL.Path]
				mu.Unlock()
				if tc.intercept != nil {
					if status := tc.intercept(r, attempt); status != 0 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"error":"synthetic","message":"synthetic failure"}`))
						return
					}
				}
				router.ServeHTTP(w, r)
			}))
			defer server.Close()
			// Reserve an address with nothing listening, as during an outage.
			reserved, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(err)
			address := reserved.Addr().String()
			require.NoError(reserved.Close())
			startDaemon := func() {
				listener, err := net.Listen("tcp", address)
				require.NoError(err)
				server.Listener = listener
				server.Start()
			}
			if !tc.outage {
				startDaemon()
			}

			path := filepath.Join(t.TempDir(), "muesli.db")
			db, err := sql.Open("sqlite3", path)
			require.NoError(err)
			_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT);INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
			require.NoError(err)
			require.NoError(db.Close())
			disabled := false
			clientCfg := config.NewDefaultConfig()
			clientCfg.HomeDir = t.TempDir()
			clientCfg.Data.DataDir = clientCfg.HomeDir
			clientCfg.Remote = config.RemoteConfig{URL: "http://" + address, APIKey: tc.clientKey, AllowInsecure: true}
			clientCfg.Muesli = []config.MuesliSource{{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Contacts: &disabled, Enabled: true, Schedule: "*/30 * * * *"}}
			ctx := testInvocationContext(t.Context(), clientCfg, invocationOptions{})
			command := &cobra.Command{Use: "sync-muesli"}
			command.SetContext(ctx)
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			run := &muesliRemoteRun{cmd: command, cfg: clientCfg}
			defer run.close()
			watched, err := muesliWatchSources(clientCfg.Muesli)
			require.NoError(err)

			stopWaiting := errors.New("synthetic end of watch")
			waits := 0
			wait := func(context.Context, time.Time) error {
				waits++
				if waits == 1 {
					if tc.outage {
						startDaemon()
					}
					return nil
				}
				return stopWaiting
			}
			var reported []error
			err = runMuesliWatch(ctx, watched, run.watchScan, wait, func(err error) { reported = append(reported, err) })

			var archived int
			require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&archived))
			if tc.wantStop != 0 {
				apiErr, ok := errors.AsType[*daemonclient.APIError](err)
				require.True(ok, "an unfixable error ends the watch: %v", err)
				assert.Equal(tc.wantStop, apiErr.Status)
				assert.Zero(waits, "no rescan is scheduled after an unfixable error")
				assert.Zero(archived)
				return
			}
			require.ErrorIs(err, stopWaiting)
			require.Len(reported, tc.wantReport, "the failure is reported once and retried")
			assert.True(transientDaemonError(reported[0]), "unexpected error: %v", reported[0])
			assert.Equal(1, archived, "the scheduled rescan uploads once the daemon accepts requests")
		})
	}
}
