package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestDerivedOnlyRefusesStaleSchemaBeforeCreatingStaging(t *testing.T) {
	requirementsForTest := require.New(t)
	parent := t.TempDir()
	dbPath := filepath.Join(parent, "msgvault.db")
	analyticsDir := filepath.Join(parent, "analytics")
	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.InitSchema())
	requirementsForTest.NoError(st.Close())
	requirementsForTest.NoError(os.MkdirAll(analyticsDir, 0o755))
	marker, err := json.Marshal(query.CacheSyncState{
		LastSyncAt:    time.Now().UTC(),
		SchemaVersion: query.CacheSchemaVersion - 1,
	})
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))

	_, err = buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
	staging, globErr := filepath.Glob(filepath.Join(
		parent,
		cacheStagingPrefix(analyticsDir)+"*",
	))
	requirementsForTest.NoError(globErr)
	assert.Empty(t, staging)
}

func TestDerivedOnlyRefreshCarriesStatsAndRefreshesMembershipRollups(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirementsForTest.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirementsForTest.NoError(err)
	messagesBefore := snapshotDatasetBytes(t, analyticsDir, tableMessages)

	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	_, err = st.DB().Exec(`
		INSERT INTO conversation_participants (conversation_id, participant_id)
		VALUES (102, 3)
	`)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.Close())

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.True(result.IdentityOnly)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(before.Stats, after.Stats)
	assertionsForTest.NotEqual(
		before.ConversationParticipantsFingerprint,
		after.ConversationParticipantsFingerprint,
	)
	assertionsForTest.False(after.PublishedAt.Before(before.PublishedAt))
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(fingerprint, after.DatasetFingerprint)
	assertionsForTest.Equal(messagesBefore,
		snapshotDatasetBytes(t, analyticsDir, tableMessages))

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "test-duckdb-tmp")),
	)
	requirementsForTest.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var membershipRows int64
	requirementsForTest.NoError(duckDB.QueryRow(`
		SELECT count(DISTINCT message_id)
		FROM read_parquet(?, hive_partitioning = true)
		WHERE conversation_id = 102
		  AND canonical_id = 3
		  AND is_conversation_member
	`, filepath.Join(
		analyticsDir,
		identityindex.DatasetActivity,
		"**",
		"*.parquet",
	)).Scan(&membershipRows))
	assertionsForTest.Positive(membershipRows)
}

func TestDerivedOnlyFailurePreservesMarkerAndLeavesDetectableDrift(t *testing.T) {
	requirementsForTest := require.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirementsForTest.NoError(err)

	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	_, err = st.LinkParticipants(2, 3)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.Close())

	beforeMarker, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirementsForTest.NoError(err)
	sentinel := errors.New("derived publication sentinel")
	derivedPublishBeforeMarkerHook = func() error { return sentinel }
	t.Cleanup(func() { derivedPublishBeforeMarkerHook = nil })

	_, err = buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.ErrorIs(err, sentinel)
	afterMarker, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirementsForTest.NoError(readErr)
	assert.Equal(t, beforeMarker, afterMarker)
	assert.Equal(t, query.CacheDrifted, mustInspectCacheReadiness(t, analyticsDir))
}

func TestStoreAPIIdentityRefreshLaunchesChildWithoutParentCacheLock(t *testing.T) {
	requirementsForTest := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.InitSchema())
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirementsForTest.NoError(os.MkdirAll(analyticsDir, 0o755))

	const revision = int64(42)
	marker, err := json.Marshal(query.CacheSyncState{IdentityRevision: revision})
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))

	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context) error {
		lock := flock.New(query.CacheBuildLockPath(analyticsDir))
		locked, lockErr := lock.TryLock()
		require.NoError(t, lockErr)
		require.True(t, locked, "parent daemon must not hold the cache lock")
		require.NoError(t, lock.Unlock())
		return nil
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = old })

	adapter := &storeAPIAdapter{store: st, analyticsDir: analyticsDir}
	got, err := adapter.RefreshIdentityDatasets(context.Background())
	requirementsForTest.NoError(err)
	assert.Equal(t, revision, got)
}

func TestDerivedChildEscalatesTypedPreconditionFailureToFullBuild(t *testing.T) {
	old := executeBuildCacheSubprocessMode
	var modes []buildCacheMode
	executeBuildCacheSubprocessMode = func(
		_ context.Context,
		mode buildCacheMode,
	) error {
		modes = append(modes, mode)
		if mode == buildCacheModeDerived {
			return ErrDerivedRefreshRequiresFullBuild
		}
		return nil
	}
	t.Cleanup(func() { executeBuildCacheSubprocessMode = old })

	require.NoError(t, runDerivedCacheSubprocess(context.Background()))
	assert.Equal(t,
		[]buildCacheMode{buildCacheModeDerived, buildCacheModeFull},
		modes,
	)
}

func TestBuildCacheInternalModesAreMutuallyExclusive(t *testing.T) {
	_, err := requestedBuildCacheMode(true, true, false)
	require.ErrorContains(t, err, "mutually exclusive")
	_, err = requestedBuildCacheMode(false, true, true)
	require.ErrorContains(t, err, "mutually exclusive")
}

func snapshotCacheBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	}))
	return out
}

func snapshotDatasetBytes(t *testing.T, root, dataset string) map[string]string {
	t.Helper()
	return snapshotCacheBytes(t, filepath.Join(root, dataset))
}

func TestRebuildCacheAfterWriteReturnsError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	require.NoError(st.Close())

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("cache export sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err = rebuildCacheAfterWrite(dbPath)
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "refresh analytics cache")
}

func TestRepairEncodingReturnsCacheRefreshError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("repair cache sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err := runRepairEncodingLocal(&cobra.Command{})
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "encoding repair completed")
	require.ErrorContains(err, "analytics cache refresh failed")
}

func TestScheduledCacheRefreshFailurePreservesCompletedSyncRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	const identifier = "imaps://user@example.com@imap.example.com:993"
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: 7,
		MessagesAdded:     5,
		MessagesUpdated:   2,
	}))
	require.NoError(st.CompleteSync(syncID, "cursor-2"))

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("scheduled cache sentinel")
	oldRunBuild := runBuildCacheSubprocess
	runBuildCacheSubprocess = func(context.Context, bool, bool) error { return sentinel }
	t.Cleanup(func() { runBuildCacheSubprocess = oldRunBuild })

	getOAuthMgr := func(string) (*oauth.Manager, error) {
		return nil, errors.New("unexpected Gmail OAuth path")
	}
	err = runScheduledSync(context.Background(), identifier, st, getOAuthMgr)
	require.ErrorIs(err, sentinel, "cache failure must reach the scheduled job result")
	require.ErrorContains(err, "refresh analytics cache")

	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(syncID, latest.ID)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(7), latest.MessagesProcessed)
	assert.Equal(int64(5), latest.MessagesAdded)
	assert.Equal(int64(2), latest.MessagesUpdated)
}
