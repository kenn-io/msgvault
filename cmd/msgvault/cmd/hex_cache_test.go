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
		require.True(t, jobs.waitContext(wait))
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "you@example.com"}
	_, err := adapter.ImportHex(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
	meeting := hex.Meeting{Manifest: hex.Manifest{SchemaVersion: 1, ID: "cache-fixture", Title: "Initial", Status: "failed", CreatedAtMS: 1790000000123}, TranscriptState: "missing"}
	request := hex.RemoteRequest{Action: "upsert", Source: source, Meeting: &meeting}
	result, err := adapter.ImportHex(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	result, err = adapter.ImportHex(t.Context(), request)
	require.NoError(t, err)
	assert.False(t, result.Changed)
	require.True(t, jobs.waitContext(t.Context()))
	assert.Empty(t, modes, "disabled automatic caching stays disabled")
	meeting.Manifest.Title = "Edited"
	request.NoBuildCache = true
	result, err = adapter.ImportHex(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, result.Changed)
	assert.Empty(t, modes)
	result, err = adapter.ImportHex(t.Context(), hex.RemoteRequest{Action: "refresh", Source: source, BuildCache: true})
	require.NoError(t, err)
	assert.Equal(t, "refreshed", result.Status)
	wait, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(t, jobs.waitContext(wait))
	select {
	case mode := <-modes:
		assert.Equal(t, buildCacheModeAuto, mode)
	default:
		require.FailNow(t, "explicit cache refresh was not queued")
	}
}
func TestHexScheduledUnchangedScanChecksDeferredCache(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(hex.SourceType, "recorder")
	require.NoError(t, err)
	root := t.TempDir()
	dir := filepath.Join(root, "fixture")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema_version":1,"id":"fixture","title":"Cache fixture","status":"failed","created_at_ms":1790000000123}`), 0600))
	source := config.HexSource{Identifier: "recorder", AccountEmail: "you@example.com", MeetingsDir: root}
	summary, err := hex.NewImporter(st).Import(t.Context(), hexImportOptions(source))
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.MeetingsAdded)
	original := rebuildHexCacheAfterScheduledSync
	t.Cleanup(func() { rebuildHexCacheAfterScheduledSync = original })
	var requested []string
	rebuildHexCacheAfterScheduledSync = func(_ context.Context, key string) error { requested = append(requested, key); return nil }
	require.NoError(t, runConfiguredHexSync(t.Context(), st, source, t.TempDir()))
	assert.Equal(t, []string{"hex:recorder"}, requested)
}
