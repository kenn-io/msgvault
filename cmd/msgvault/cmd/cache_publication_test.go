package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
)

func TestCachePublicationCommitsDatasetsBeforeMarker(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	writeCommittedPublicationFixtureState(t, analyticsDir, 1)

	staging, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")

	oldState, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirements.NoError(err)
	var observedMarker []byte
	cachePublicationAfterMoveHook = func(string) error {
		if observedMarker == nil {
			observedMarker, err = os.ReadFile(query.CacheStatePath(analyticsDir))
		}
		return err
	}
	t.Cleanup(func() { cachePublicationAfterMoveHook = nil })

	state := query.CacheSyncState{
		LastMessageID: 2,
		LastSyncAt:    time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC),
		SchemaVersion: query.CacheSchemaVersion,
	}
	data, err := json.Marshal(state)
	requirements.NoError(err)
	requirements.NoError(publishCache(
		staging,
		analyticsDir,
		cachePublishPlanForMode(true),
		data,
		acquirePublishLock,
	))

	assertions.Equal(oldState, observedMarker)
	committed, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	assertions.Equal(int64(2), committed.LastMessageID)
	assertions.False(committed.PublishedAt.IsZero())
	assertions.NotEmpty(committed.DatasetFingerprint)
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	requirements.NoError(err)
	assertions.Equal(fingerprint, committed.DatasetFingerprint)
	assertions.Equal(query.CacheReady, mustInspectCacheReadiness(t, analyticsDir))
}

func TestCachePublicationInterruptionLeavesOldMarkerAndDetectableDrift(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	writeCommittedPublicationFixtureState(t, analyticsDir, 1)
	oldMarker, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirements.NoError(err)

	staging, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")

	interrupted := errors.New("publication interrupted")
	cachePublicationAfterMoveHook = func(string) error { return interrupted }
	t.Cleanup(func() { cachePublicationAfterMoveHook = nil })
	stateData, err := json.Marshal(query.CacheSyncState{
		LastMessageID: 2,
		LastSyncAt:    time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC),
		SchemaVersion: query.CacheSchemaVersion,
	})
	requirements.NoError(err)

	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(true), stateData, acquirePublishLock)
	requirements.ErrorIs(err, interrupted)
	currentMarker, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirements.NoError(readErr)
	assertions.Equal(oldMarker, currentMarker)
	assertions.Equal(query.CacheDrifted, mustInspectCacheReadiness(t, analyticsDir))
	assertions.NoDirExists(filepath.Join(analyticsDir, ".publication"))
}

func TestIncrementalPublicationPlanAppendsActivityAndReplacesCompactDatasets(t *testing.T) {
	assertions := assert.New(t)
	plan := cachePublishPlanForMode(false)
	for _, dataset := range []string{
		tableMessages,
		"message_recipients",
		"message_labels",
		tableAttachments,
		identityindex.DatasetActivity,
	} {
		assertions.True(plan.Append[dataset], dataset)
		assertions.False(plan.Replace[dataset], dataset)
	}
	for _, dataset := range []string{
		tableParticipants,
		tableParticipantIdentifiers,
		tableLabels,
		"sources",
		tableConversations,
		tableConversationParticipants,
		tableOwnerParticipants,
		tableParticipantClusters,
		identityindex.DatasetPeople,
		identityindex.DatasetDomains,
		identityindex.DatasetRelationshipDaily,
	} {
		assertions.True(plan.Replace[dataset], dataset)
		assertions.False(plan.Append[dataset], dataset)
	}
}

func TestCleanupStaleCacheStagingUsesBuilderPID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	live, err := newCacheStaging(analyticsDir)
	requirements.NoError(err)
	t.Cleanup(func() { _ = live.cleanup() })

	dead := filepath.Join(parent, cacheStagingPrefix(analyticsDir)+"dead")
	missing := filepath.Join(parent, cacheStagingPrefix(analyticsDir)+"missing")
	requirements.NoError(os.MkdirAll(dead, 0o755))
	requirements.NoError(os.MkdirAll(missing, 0o755))
	requirements.NoError(os.WriteFile(
		filepath.Join(dead, ".builder-pid"),
		[]byte(strconv.Itoa(1<<30)),
		0o600,
	))

	requirements.NoError(cleanupStaleCacheStaging(analyticsDir))
	assertions.DirExists(live.root)
	assertions.NoDirExists(dead)
	assertions.NoDirExists(missing)
}

func writeCommittedPublicationFixtureState(
	t *testing.T,
	analyticsDir string,
	identityRevision int64,
) {
	t.Helper()
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	data, err := json.Marshal(query.CacheSyncState{
		LastMessageID:      1,
		LastSyncAt:         time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC),
		SchemaVersion:      query.CacheSchemaVersion,
		IdentityRevision:   identityRevision,
		PublishedAt:        time.Date(2026, 7, 15, 10, 1, 0, 0, time.UTC),
		DatasetFingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), data, 0o600))
}

func writePublicationTree(t *testing.T, root, filename string) {
	t.Helper()
	for _, dataset := range query.RequiredParquetDirs {
		dir := filepath.Join(root, dataset)
		if dataset == tableMessages || dataset == identityindex.DatasetActivity {
			dir = filepath.Join(dir, "year=2026")
		}
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, filename),
			[]byte(dataset+"-"+filename),
			0o600,
		))
	}
}

func mustInspectCacheReadiness(t *testing.T, analyticsDir string) query.CacheReadiness {
	t.Helper()
	readiness, err := query.InspectCacheReadiness(analyticsDir)
	require.NoError(t, err)
	return readiness
}

// TestBuilderLockDoesNotBlockReaders pins the narrowed locking protocol: the
// builder lock held for the whole staging/index-construction phase must not
// exclude readers — they keep querying the committed generation — while
// destructive maintenance (lockCacheAndInvalidateSyncState) still holds the
// reader-coordination lock exclusively for its entire operation.
func TestBuilderLockDoesNotBlockReaders(t *testing.T) {
	requirements := require.New(t)
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	requirements.NoError(os.MkdirAll(analyticsDir, 0o755))

	builderLock, err := acquireCacheBuildLock(analyticsDir)
	requirements.NoError(err, "acquire builder lock")

	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := query.AcquireCacheReadLock(readCtx, analyticsDir)
	requirements.NoError(err, "readers must not block while a build stages under the builder lock")
	release()
	requirements.NoError(builderLock.Unlock(), "release builder lock")

	unlockCache, err := lockCacheAndInvalidateSyncState(analyticsDir)
	requirements.NoError(err, "destructive maintenance locks")
	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelBlocked()
	_, err = query.AcquireCacheReadLock(blockedCtx, analyticsDir)
	requirements.Error(err, "readers must stay excluded during destructive maintenance")
	requirements.NoError(unlockCache(), "release destructive locks")

	release, err = query.AcquireCacheReadLock(context.Background(), analyticsDir)
	requirements.NoError(err, "readers must reacquire after destructive maintenance releases")
	release()
}
