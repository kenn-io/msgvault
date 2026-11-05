package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHexRemoteCacheFlagsAndUnchangedEvidence(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

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
		wait, stop := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer stop()
		must.True(jobs.waitContext(wait))
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "you@example.com"}
	_, err := adapter.ImportHex(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	must.NoError(err)
	meeting := hex.Meeting{Manifest: hex.Manifest{SchemaVersion: 1, ID: "cache-fixture", Title: "Initial", Status: "failed", CreatedAtMS: 1790000000123}, TranscriptState: "missing"}
	request := hex.RemoteRequest{Action: "upsert", Source: source, Meeting: &meeting}
	result, err := adapter.ImportHex(t.Context(), request)
	must.NoError(err)
	check.True(result.Changed)
	result, err = adapter.ImportHex(t.Context(), request)
	must.NoError(err)
	check.False(result.Changed)
	must.True(jobs.waitContext(t.Context()))
	check.Empty(modes, "disabled automatic caching stays disabled")
	meeting.Manifest.Title = "Edited"
	request.NoBuildCache = true
	result, err = adapter.ImportHex(t.Context(), request)
	must.NoError(err)
	check.True(result.Changed)
	check.Empty(modes)
	result, err = adapter.ImportHex(t.Context(), hex.RemoteRequest{Action: "refresh", Source: source, BuildCache: true})
	must.NoError(err)
	check.Equal("refreshed", result.Status)
	wait, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	must.True(jobs.waitContext(wait))
	select {
	case mode := <-modes:
		check.Equal(buildCacheModeAuto, mode)
	default:
		must.FailNow("explicit cache refresh was not queued")
	}
}
func TestHexScheduledUnchangedScanChecksDeferredCache(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(hex.SourceType, "recorder")
	must.NoError(err)
	root := t.TempDir()
	dir := filepath.Join(root, "fixture")
	must.NoError(os.Mkdir(dir, 0700))
	must.NoError(os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":1,"id":"fixture","title":"Cache fixture","status":"failed","created_at_ms":1790000000123}`), 0600))
	source := config.HexSource{Identifier: "recorder", AccountEmail: "you@example.com", MeetingsDir: root}
	summary, err := hex.NewImporter(st).Import(t.Context(), hexImportOptions(source))
	must.NoError(err)
	check.Equal(int64(1), summary.MeetingsAdded)
	original := rebuildHexCacheAfterScheduledSync
	t.Cleanup(func() { rebuildHexCacheAfterScheduledSync = original })
	var requested []string
	rebuildHexCacheAfterScheduledSync = func(_ context.Context, key string) error { requested = append(requested, key); return nil }
	must.NoError(runConfiguredHexSync(t.Context(), st, source, t.TempDir()))
	check.Equal([]string{"hex:recorder"}, requested)
}
