package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

const (
	cachePublicationHelperEnv = "MSGVAULT_CACHE_PUBLICATION_HELPER"
	cachePublicationRootEnv   = "MSGVAULT_CACHE_PUBLICATION_ROOT"
	cachePublicationModeEnv   = "MSGVAULT_CACHE_PUBLICATION_MODE"
	cachePublicationKillEnv   = "MSGVAULT_CACHE_PUBLICATION_KILL_PHASE"
	cachePublicationReadyEnv  = "MSGVAULT_CACHE_PUBLICATION_READY"
	cacheRecoveryHelperEnv    = "MSGVAULT_CACHE_RECOVERY_HELPER"
)

func TestCachePublicationRecoversAfterProcessKillAtEveryCommitPhase(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		killPhase string
		wantNew   bool
	}{
		{"full journal prepared", "full", "journal-prepared", false},
		{"full backup moved", "full", "backup-moved", false},
		{"full data moved", "full", "data-moved", false},
		{"full marker prepared", "full", cachePublicationPhaseMarkerPrepared, false},
		{"full marker committed", "full", cachePublicationPhaseMarkerCommitted, true},
		{"incremental backup moved", "incremental", "backup-moved", false},
		{"incremental data moved", "incremental", "data-moved", false},
		{"incremental marker prepared", "incremental", cachePublicationPhaseMarkerPrepared, false},
		{"incremental marker committed", "incremental", cachePublicationPhaseMarkerCommitted, true},
		{"derived backup moved", "derived", "backup-moved", false},
		{"derived data moved", "derived", "data-moved", false},
		{"derived marker prepared", "derived", cachePublicationPhaseMarkerPrepared, false},
		{"derived marker committed", "derived", cachePublicationPhaseMarkerCommitted, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			parent := t.TempDir()
			analyticsDir := filepath.Join(parent, "analytics")
			writePublicationTree(t, analyticsDir, "old.parquet")
			writeCommittedPublicationFixtureState(t, analyticsDir, 1)
			before := snapshotCacheBytes(t, analyticsDir)
			readyPath := filepath.Join(parent, "publication-ready")

			// #nosec G702 -- the test intentionally re-executes its own fixed binary.
			command := exec.Command(
				os.Args[0],
				"-test.run=^TestCachePublicationProcessKillHelper$",
			)
			command.Env = append(os.Environ(),
				cachePublicationHelperEnv+"=1",
				cachePublicationRootEnv+"="+analyticsDir,
				cachePublicationModeEnv+"="+test.mode,
				cachePublicationKillEnv+"="+test.killPhase,
				cachePublicationReadyEnv+"="+readyPath,
			)
			require.NoError(command.Start())
			require.Eventually(func() bool {
				_, err := os.Stat(readyPath)
				return err == nil
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(command.Process.Kill())
			require.Error(command.Wait())

			require.NoError(recoverInterruptedCachePublication(analyticsDir))
			require.NoError(cleanupStaleCacheStaging(analyticsDir))
			readiness, err := query.InspectCacheReadiness(analyticsDir)
			require.NoError(err)
			assert.Equal(query.CacheReady, readiness)
			state, err := query.ReadCacheSyncState(analyticsDir)
			require.NoError(err)
			if test.wantNew {
				assert.Equal(int64(2), state.IdentityRevision)
				assert.NotEqual(before, snapshotCacheBytes(t, analyticsDir))
			} else {
				assert.Equal(int64(1), state.IdentityRevision)
				assert.Equal(before, snapshotCacheBytes(t, analyticsDir))
			}
			assert.NoDirExists(cachePublicationTransactionRoot(analyticsDir))
		})
	}
}

func TestCachePublicationProcessKillHelper(t *testing.T) {
	if os.Getenv(cachePublicationHelperEnv) == "" {
		t.Skip("subprocess helper")
	}
	require := require.New(t)
	analyticsDir := os.Getenv(cachePublicationRootEnv)
	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	writePublicationTree(t, staging.root, "new.parquet")
	killPhase := os.Getenv(cachePublicationKillEnv)
	cachePublicationCheckpointHook = func(phase string) {
		if phase != killPhase {
			return
		}
		require.NoError(os.WriteFile(
			os.Getenv(cachePublicationReadyEnv),
			[]byte(phase),
			0o600,
		))
		select {}
	}
	state := query.CacheSyncState{SchemaVersion: query.CacheSchemaVersion}
	if os.Getenv(cachePublicationModeEnv) != "first" {
		state, err = query.ReadCacheSyncState(analyticsDir)
		require.NoError(err)
	}
	state.IdentityRevision = 2
	switch os.Getenv(cachePublicationModeEnv) {
	case "full", "first":
		data, marshalErr := json.Marshal(state)
		require.NoError(marshalErr)
		require.NoError(publishCache(
			staging,
			analyticsDir,
			cachePublishPlanForMode(true),
			data,
		))
	case "incremental":
		data, marshalErr := json.Marshal(state)
		require.NoError(marshalErr)
		require.NoError(publishCache(
			staging,
			analyticsDir,
			cachePublishPlanForMode(false),
			data,
		))
	case "derived":
		require.NoError(publishDerivedCache(
			staging,
			analyticsDir,
			derivedCachePublishPlan(false),
			state,
		))
	default:
		require.Fail("unknown publication helper mode")
	}
}

func TestCachePublicationFirstBuildRecoveryAbsenceIsRestartableAfterProcessKill(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	readyPath := filepath.Join(parent, "publication-ready")

	runCachePublicationHelperUntilKilled(
		t,
		"TestCachePublicationProcessKillHelper",
		readyPath,
		cachePublicationHelperEnv+"=1",
		cachePublicationRootEnv+"="+analyticsDir,
		cachePublicationModeEnv+"=first",
		cachePublicationKillEnv+"="+cachePublicationPhaseMarkerPrepared,
		cachePublicationReadyEnv+"="+readyPath,
	)
	runCachePublicationHelperUntilKilled(
		t,
		"TestCachePublicationRecoveryProcessKillHelper",
		readyPath,
		cacheRecoveryHelperEnv+"=1",
		cachePublicationRootEnv+"="+analyticsDir,
		cachePublicationKillEnv+"=recovery-rolled-back",
		cachePublicationReadyEnv+"="+readyPath,
	)

	require.NoError(recoverInterruptedCachePublication(analyticsDir))
	require.NoError(cleanupStaleCacheStaging(analyticsDir))
	assert.NoFileExists(query.CacheStatePath(analyticsDir))
	for _, dataset := range query.RequiredParquetDirs {
		assert.NoDirExists(filepath.Join(analyticsDir, dataset))
	}
	assertNoCachePublicationResidue(t, analyticsDir)
}

func TestCachePublicationRecoveryIsRestartableAfterProcessKill(t *testing.T) {
	tests := []struct {
		name          string
		publicationAt string
		recoveryAt    string
		wantNew       bool
	}{
		{"rollback after marker restore", cachePublicationPhaseMarkerPrepared, "recovery-marker-restored", false},
		{"rollback after durable phase", cachePublicationPhaseMarkerPrepared, "recovery-rolled-back", false},
		{"rollback after backup cleanup", cachePublicationPhaseMarkerPrepared, "finalize-backup-removed", false},
		{"rollback after marker cleanup", cachePublicationPhaseMarkerPrepared, "finalize-old-marker-removed", false},
		{"rollback after journal cleanup", cachePublicationPhaseMarkerPrepared, "finalize-journal-removed", false},
		{"rollback after root cleanup", cachePublicationPhaseMarkerPrepared, "finalize-root-removed", false},
		{"commit after backup cleanup", cachePublicationPhaseMarkerCommitted, "finalize-backup-removed", true},
		{"commit after marker cleanup", cachePublicationPhaseMarkerCommitted, "finalize-old-marker-removed", true},
		{"commit after journal cleanup", cachePublicationPhaseMarkerCommitted, "finalize-journal-removed", true},
		{"commit after root cleanup", cachePublicationPhaseMarkerCommitted, "finalize-root-removed", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			parent := t.TempDir()
			analyticsDir := filepath.Join(parent, "analytics")
			writePublicationTree(t, analyticsDir, "old.parquet")
			writeCommittedPublicationFixtureState(t, analyticsDir, 1)
			oldSnapshot := snapshotCacheBytes(t, analyticsDir)
			readyPath := filepath.Join(parent, "publication-ready")

			runCachePublicationHelperUntilKilled(
				t,
				"TestCachePublicationProcessKillHelper",
				readyPath,
				cachePublicationHelperEnv+"=1",
				cachePublicationRootEnv+"="+analyticsDir,
				cachePublicationModeEnv+"=full",
				cachePublicationKillEnv+"="+test.publicationAt,
				cachePublicationReadyEnv+"="+readyPath,
			)
			wantSnapshot := oldSnapshot
			if test.wantNew {
				wantSnapshot = snapshotCacheBytes(t, analyticsDir)
			}

			runCachePublicationHelperUntilKilled(
				t,
				"TestCachePublicationRecoveryProcessKillHelper",
				readyPath,
				cacheRecoveryHelperEnv+"=1",
				cachePublicationRootEnv+"="+analyticsDir,
				cachePublicationKillEnv+"="+test.recoveryAt,
				cachePublicationReadyEnv+"="+readyPath,
			)

			require.NoError(recoverInterruptedCachePublication(analyticsDir))
			require.NoError(cleanupStaleCacheStaging(analyticsDir))
			readiness, err := query.InspectCacheReadiness(analyticsDir)
			require.NoError(err)
			assert.Equal(query.CacheReady, readiness)
			state, err := query.ReadCacheSyncState(analyticsDir)
			require.NoError(err)
			if test.wantNew {
				assert.Equal(int64(2), state.IdentityRevision)
			} else {
				assert.Equal(int64(1), state.IdentityRevision)
			}
			assert.Equal(wantSnapshot, snapshotCacheBytes(t, analyticsDir))
			assertNoCachePublicationResidue(t, analyticsDir)
		})
	}
}

func TestCachePublicationRecoveryProcessKillHelper(t *testing.T) {
	if os.Getenv(cacheRecoveryHelperEnv) == "" {
		t.Skip("subprocess helper")
	}
	killPhase := os.Getenv(cachePublicationKillEnv)
	cachePublicationCheckpointHook = func(phase string) {
		if phase != killPhase {
			return
		}
		require.NoError(t, os.WriteFile(
			os.Getenv(cachePublicationReadyEnv),
			[]byte(phase),
			0o600,
		))
		select {}
	}
	require.NoError(t, recoverInterruptedCachePublication(
		os.Getenv(cachePublicationRootEnv),
	))
}

func runCachePublicationHelperUntilKilled(
	t *testing.T,
	helperName string,
	readyPath string,
	env ...string,
) {
	t.Helper()
	require.NoError(t, os.RemoveAll(readyPath))
	// #nosec G702 -- the test intentionally re-executes its own fixed binary.
	command := exec.Command(
		os.Args[0],
		"-test.run=^"+helperName+"$",
	)
	command.Env = append(os.Environ(), env...)
	require.NoError(t, command.Start())
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, command.Process.Kill())
	require.Error(t, command.Wait())
}

func assertNoCachePublicationResidue(t *testing.T, analyticsDir string) {
	t.Helper()
	assert.NoDirExists(t, cachePublicationTransactionRoot(analyticsDir))
	parent := filepath.Dir(analyticsDir)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(
			t,
			strings.HasPrefix(entry.Name(), cacheStagingPrefix(analyticsDir)),
			"stale cache staging directory %s",
			entry.Name(),
		)
	}
}

func writeCommittedPublicationFixtureState(
	t *testing.T,
	analyticsDir string,
	identityRevision int64,
) {
	t.Helper()
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	state, err := json.Marshal(query.CacheSyncState{
		LastMessageID:      1,
		LastSyncAt:         time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		SchemaVersion:      query.CacheSchemaVersion,
		IdentityRevision:   identityRevision,
		PublishedAt:        time.Date(2026, 7, 27, 12, 1, 0, 0, time.UTC),
		DatasetFingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		query.CacheStatePath(analyticsDir),
		state,
		0o600,
	))
}

func TestCachePublicationCommitsRevisionTimestampAndDatasetFingerprint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	input, err := json.Marshal(query.CacheSyncState{
		SchemaVersion:          query.CacheSchemaVersion,
		LastMessageID:          41,
		LastSyncAt:             time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		LastCacheUpdateCount:   3,
		LastFailedSyncRunCount: 2,
		LastFailedSyncRunIDSum: 19,
	})
	require.NoError(err)

	require.NoError(publishCache(staging, analyticsDir, cachePublishPlanForMode(true), input))
	state, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.False(state.PublishedAt.IsZero())
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(err)
	assert.Equal(fingerprint, state.DatasetFingerprint)
	assert.NotEmpty(state.Revision())
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	require.NoError(err)
	assert.Equal(query.CacheReady, readiness)
}

func TestCachePublicationFullReplacesEveryDatasetAndWritesStateLast(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	oldState := []byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), oldState, 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	newState := []byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`)

	require.NoError(publishCache(staging, analyticsDir, cachePublishPlanForMode(true), newState))

	for _, dataset := range query.RequiredParquetDirs {
		assert.False(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, "new.parquet"), dataset)
	}
	gotState, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(err)
	var committed query.CacheSyncState
	require.NoError(json.Unmarshal(gotState, &committed))
	assert.Equal(time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC), committed.LastSyncAt)
	assert.False(committed.PublishedAt.IsZero())
	assert.NotEmpty(committed.DatasetFingerprint)
}

func TestIncrementalPublicationReplacesDimensionsAndPrefixesAppends(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "data.parquet")

	require.NoError(publishCache(staging, analyticsDir, cachePublishPlanForMode(false),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`)))

	for _, dataset := range []string{
		"participants",
		"participant_identifiers",
		"labels",
		"sources",
		"conversations",
		identityindex.DatasetConversationEdges,
		identityindex.DatasetDirectory,
		identityindex.DatasetRollups,
		identityindex.DatasetDomainRollups,
		identityindex.DatasetRelationships,
		identityindex.DatasetRelationshipDaily,
	} {
		assert.False(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, "data.parquet"), dataset)
	}
	for _, dataset := range []string{
		"message_recipients",
		"message_labels",
		"attachments",
		identityindex.DatasetEntryFacts,
		identityindex.DatasetDirectEdges,
	} {
		assert.True(publicationFileExists(analyticsDir, dataset, "old.parquet"), dataset)
		assert.True(publicationFileExists(analyticsDir, dataset, staging.buildID+"-data.parquet"), dataset)
	}
	assert.True(publicationFileExists(analyticsDir, "messages", "old.parquet"))
	assert.FileExists(filepath.Join(analyticsDir, "messages", "year=2024",
		staging.buildID+"-data.parquet"))
}

func TestCachePublicationCollisionFailsBeforeInvalidation(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	oldState := []byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`)
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), oldState, 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "data.parquet")
	collision := filepath.Join(analyticsDir, "message_recipients", staging.buildID+"-data.parquet")
	require.NoError(os.WriteFile(collision, []byte("collision"), 0o600))

	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(false),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	require.ErrorContains(err, "already exists")
	gotState, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(readErr)
	require.Equal(oldState, gotState)
}

func TestCachePublicationFailureBeforeMovesPreservesCommittedCache(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	require.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	publishErr := errors.New("publish interrupted")
	buildCacheBeforePublicationMovesHook = func() error { return publishErr }
	t.Cleanup(func() { buildCacheBeforePublicationMovesHook = nil })
	before := snapshotCacheBytes(t, analyticsDir)
	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(true),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	require.ErrorIs(err, publishErr)
	assert.Equal(t, before, snapshotCacheBytes(t, analyticsDir))
}

func TestCachePublicationRollsBackFullReplacementAfterEarlierMoves(t *testing.T) {
	requirements := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	requirements.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	before := snapshotCacheBytes(t, analyticsDir)

	buildCacheBeforePublicationMovesHook = func() error {
		return os.RemoveAll(filepath.Join(
			staging.root,
			identityindex.DatasetRelationshipDaily,
		))
	}
	t.Cleanup(func() { buildCacheBeforePublicationMovesHook = nil })
	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(true),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	requirements.ErrorContains(err, "publish cache path")
	assert.Equal(t, before, snapshotCacheBytes(t, analyticsDir))
}

func TestCachePublicationRollsBackIncrementalAppendsAfterLaterMoveFailure(t *testing.T) {
	requirements := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	requirements.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	before := snapshotCacheBytes(t, analyticsDir)

	buildCacheBeforePublicationMovesHook = func() error {
		return os.RemoveAll(filepath.Join(
			staging.root,
			identityindex.DatasetRelationshipDaily,
		))
	}
	t.Cleanup(func() { buildCacheBeforePublicationMovesHook = nil })
	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(false),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	requirements.ErrorContains(err, "publish cache path")
	assert.Equal(t, before, snapshotCacheBytes(t, analyticsDir))
}

func TestCachePublicationRollsBackWhenMarkerPreparationFails(t *testing.T) {
	requirements := require.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	requirements.NoError(os.WriteFile(query.CacheStatePath(analyticsDir),
		[]byte(`{"last_sync_at":"2026-07-15T10:00:00Z"}`), 0o600))

	staging, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")
	before := snapshotCacheBytes(t, analyticsDir)

	stateErr := errors.New("prepare marker")
	buildCacheWriteStateFile = func(string, []byte, os.FileMode) error {
		return stateErr
	}
	t.Cleanup(func() { buildCacheWriteStateFile = os.WriteFile })
	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(false),
		[]byte(`{"last_sync_at":"2026-07-15T11:00:00Z"}`))
	requirements.ErrorIs(err, stateErr)
	assert.Equal(t, before, snapshotCacheBytes(t, analyticsDir))
}

func TestCachePublicationCleansOnlyPrivateStagingDirectories(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	stale := filepath.Join(parent, ".analytics.build-stale")
	unrelated := filepath.Join(parent, ".other.build-stale")
	publication := cachePublicationTransactionRoot(analyticsDir)
	require.NoError(os.MkdirAll(stale, 0o755))
	require.NoError(os.MkdirAll(unrelated, 0o755))
	require.NoError(os.MkdirAll(publication, 0o700))
	require.NoError(os.WriteFile(query.CacheBuildLockPath(analyticsDir), []byte("lock"), 0o600))

	require.NoError(cleanupStaleCacheStaging(analyticsDir))
	assert.NoDirExists(stale)
	assert.DirExists(unrelated)
	assert.DirExists(publication)
	assert.FileExists(query.CacheBuildLockPath(analyticsDir))
}

func writePublicationTree(t *testing.T, root, filename string) {
	t.Helper()
	for _, dataset := range cachePublishPlanForMode(true).datasets() {
		dir := filepath.Join(root, dataset)
		if dataset == "messages" {
			dir = filepath.Join(dir, "year=2024")
		}
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, filename), []byte(dataset), 0o600))
	}
}

func publicationFileExists(root, dataset, filename string) bool {
	if dataset == "messages" {
		_, err := os.Stat(filepath.Join(root, dataset, "year=2024", filename))
		return err == nil
	}
	_, err := os.Stat(filepath.Join(root, dataset, filename))
	return err == nil
}
