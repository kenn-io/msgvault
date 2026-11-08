package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecorderRemoteDefaultUnchangedSkipAndExplicitRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		hook      *func(context.Context, string) error
		send      func(context.Context, *storeAPIAdapter, string, string, bool, bool) (bool, string, error)
	}{
		{"Hex", hex.SourceType, &rebuildHexCacheAfterScheduledSync, func(ctx context.Context, adapter *storeAPIAdapter, action, title string, skip, build bool) (bool, string, error) {
			request := hex.RemoteRequest{Action: action, Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "you@example.com"}, NoBuildCache: skip, BuildCache: build}
			if action == "upsert" {
				request.Meeting = &hex.Meeting{Manifest: hex.Manifest{SchemaVersion: 1, ID: "cache-fixture", Title: title, Status: "failed", CreatedAtMS: 1790000000123}, TranscriptState: "missing"}
			}
			result, err := adapter.ImportHex(ctx, request)
			return result.Changed, result.Status, err
		}},
		{"Muesli", muesli.SourceType, &rebuildMuesliCacheAfterScheduledSync, func(ctx context.Context, adapter *storeAPIAdapter, action, title string, skip, build bool) (bool, string, error) {
			request := muesli.RemoteRequest{Action: action, Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}, NoBuildCache: skip, BuildCache: build}
			if action == "upsert" {
				request.Meeting = muesli.NewRemoteMeeting(muesli.Meeting{ID: 42, Title: title, Status: "completed", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", RawTranscript: "Synthetic transcript"})
			}
			result, err := adapter.ImportMuesli(ctx, request)
			return result.Changed, result.Status, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must, check := require.New(t), assert.New(t)
			original := *tc.hook
			t.Cleanup(func() { *tc.hook = original })
			var automatic []string
			*tc.hook = func(ctx context.Context, key string) error { automatic = append(automatic, key); return ctx.Err() }
			st := testutil.NewTestStore(t)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Analytics.AutoBuildCache = false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			modes := make(chan buildCacheMode, 2)
			jobs := newCacheBuildJobs(ctx, nil, func(_ context.Context, mode buildCacheMode) error { modes <- mode; return nil })
			t.Cleanup(func() {
				cancel()
				wait, stop := context.WithTimeout(context.WithoutCancel(t.Context()), serveLifecycleTestTimeout)
				defer stop()
				must.True(jobs.waitContext(wait))
			})
			adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
			_, _, err := tc.send(t.Context(), adapter, "register", "", false, false)
			must.NoError(err)
			changed, _, err := tc.send(t.Context(), adapter, "upsert", "Initial", false, false)
			must.NoError(err)
			check.True(changed)
			check.Equal([]string{tc.key}, automatic, "changed imports request automatic refresh")
			changed, _, err = tc.send(t.Context(), adapter, "upsert", "Initial", false, false)
			must.NoError(err)
			check.False(changed)
			check.Equal([]string{tc.key}, automatic, "unchanged imports do not request another automatic refresh")
			must.True(jobs.waitContext(t.Context()))
			check.Empty(modes, "automatic refresh does not use the explicit refresh job queue")
			changed, _, err = tc.send(t.Context(), adapter, "upsert", "Edited", true, false)
			must.NoError(err)
			check.True(changed)
			check.Equal([]string{tc.key}, automatic, "no-build-cache suppresses the automatic refresh hook")
			check.Empty(modes)
			_, status, err := tc.send(t.Context(), adapter, "refresh", "", false, true)
			must.NoError(err)
			check.Equal("refreshed", status)
			check.Equal([]string{tc.key}, automatic, "explicit refresh uses the job queue rather than the automatic hook")
			wait, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
			defer stop()
			must.True(jobs.waitContext(wait))
			select {
			case mode := <-modes:
				check.Equal(buildCacheModeAuto, mode)
			default:
				must.FailNow("explicit cache refresh was not queued")
			}
		})
	}
}

func TestRecorderScheduledScanRechecksCacheAfterEarlierImport(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, key string
		hook                  *func(context.Context, string) error
		prepare               func(*testing.T, *store.Store) func() error
	}{
		{"Hex", hex.SourceType, "hex:recorder", &rebuildHexCacheAfterScheduledSync, func(t *testing.T, st *store.Store) func() error {
			t.Helper()
			must := require.New(t)
			root := t.TempDir()
			dir := filepath.Join(root, "fixture")
			must.NoError(os.Mkdir(dir, 0700))
			must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":1,"id":"fixture","title":"Cache fixture","status":"failed","created_at_ms":1790000000123}`), 0600))
			source := config.HexSource{Identifier: "recorder", AccountEmail: "you@example.com", MeetingsDir: root}
			summary, err := hex.NewImporter(st).Import(t.Context(), hexImportOptions(source))
			must.NoError(err)
			must.Equal(int64(1), summary.MeetingsAdded)
			return func() error { return runConfiguredHexSync(t.Context(), st, source, t.TempDir()) }
		}},
		{"Muesli", muesli.SourceType, "muesli:recorder", &rebuildMuesliCacheAfterScheduledSync, func(t *testing.T, st *store.Store) func() error {
			t.Helper()
			must := require.New(t)
			path := filepath.Join(t.TempDir(), "muesli.db")
			db, err := sql.Open("sqlite3", path)
			must.NoError(err)
			t.Cleanup(func() { _ = db.Close() })
			_, err = db.ExecContext(t.Context(), `CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT);INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
			must.NoError(err)
			off := false
			source := config.MuesliSource{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Contacts: &off}
			options := muesliImportOptions(source)
			options.MeetingID = 42
			summary, err := muesli.NewImporter(st).Import(t.Context(), options)
			must.NoError(err)
			must.Equal(int64(1), summary.MeetingsAdded)
			return func() error { return runConfiguredMuesliSync(t.Context(), st, source, t.TempDir()) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must, check := require.New(t), assert.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.GetOrCreateSource(tc.sourceType, "recorder")
			must.NoError(err)
			run := tc.prepare(t, st)
			original := *tc.hook
			t.Cleanup(func() { *tc.hook = original })
			var requested []string
			*tc.hook = func(ctx context.Context, key string) error { requested = append(requested, key); return ctx.Err() }
			must.NoError(run())
			check.Equal([]string{tc.key}, requested, "unchanged scan rechecks deferred cache through the actual provider adapter")
		})
	}
}
