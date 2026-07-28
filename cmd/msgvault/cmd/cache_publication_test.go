package cmd

import (
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
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	writeCommittedPublicationFixtureState(t, analyticsDir, 1)

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = staging.cleanup() })
	writePublicationTree(t, staging.root, "new.parquet")

	oldState, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(t, err)
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
	require.NoError(t, err)
	require.NoError(t, publishCache(
		staging,
		analyticsDir,
		cachePublishPlanForMode(true),
		data,
	))

	assert.Equal(t, oldState, observedMarker)
	committed, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(t, err)
	assert.Equal(t, int64(2), committed.LastMessageID)
	assert.False(t, committed.PublishedAt.IsZero())
	assert.NotEmpty(t, committed.DatasetFingerprint)
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, committed.DatasetFingerprint)
	assert.Equal(t, query.CacheReady, mustInspectCacheReadiness(t, analyticsDir))
}

func TestCachePublicationInterruptionLeavesOldMarkerAndDetectableDrift(t *testing.T) {
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	writePublicationTree(t, analyticsDir, "old.parquet")
	writeCommittedPublicationFixtureState(t, analyticsDir, 1)
	oldMarker, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(t, err)

	staging, err := newCacheStaging(analyticsDir)
	require.NoError(t, err)
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
	require.NoError(t, err)

	err = publishCache(staging, analyticsDir, cachePublishPlanForMode(true), stateData)
	require.ErrorIs(t, err, interrupted)
	currentMarker, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	require.NoError(t, readErr)
	assert.Equal(t, oldMarker, currentMarker)
	assert.Equal(t, query.CacheDrifted, mustInspectCacheReadiness(t, analyticsDir))
	assert.NoDirExists(t, filepath.Join(analyticsDir, ".publication"))
}

func TestIncrementalPublicationPlanAppendsActivityAndReplacesCompactDatasets(t *testing.T) {
	plan := cachePublishPlanForMode(false)
	for _, dataset := range []string{
		tableMessages,
		"message_recipients",
		"message_labels",
		tableAttachments,
		identityindex.DatasetActivity,
	} {
		assert.True(t, plan.Append[dataset], dataset)
		assert.False(t, plan.Replace[dataset], dataset)
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
		assert.True(t, plan.Replace[dataset], dataset)
		assert.False(t, plan.Append[dataset], dataset)
	}
	for _, dataset := range []string{
		identityindex.DatasetEntryFacts,
		identityindex.DatasetDirectEdges,
		identityindex.DatasetConversationEdges,
		identityindex.DatasetDirectory,
		identityindex.DatasetRollups,
		identityindex.DatasetRelationships,
	} {
		assert.False(t, plan.Append[dataset], dataset)
		assert.False(t, plan.Replace[dataset], dataset)
	}
}

func TestCleanupStaleCacheStagingUsesBuilderPID(t *testing.T) {
	parent := t.TempDir()
	analyticsDir := filepath.Join(parent, "analytics")
	live, err := newCacheStaging(analyticsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.cleanup() })

	dead := filepath.Join(parent, cacheStagingPrefix(analyticsDir)+"dead")
	missing := filepath.Join(parent, cacheStagingPrefix(analyticsDir)+"missing")
	require.NoError(t, os.MkdirAll(dead, 0o755))
	require.NoError(t, os.MkdirAll(missing, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dead, ".builder-pid"),
		[]byte(strconv.Itoa(1<<30)),
		0o600,
	))

	require.NoError(t, cleanupStaleCacheStaging(analyticsDir))
	assert.DirExists(t, live.root)
	assert.NoDirExists(t, dead)
	assert.NoDirExists(t, missing)
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
