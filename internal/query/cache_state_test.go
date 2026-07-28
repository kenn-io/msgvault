package query

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipIndexDatasetCatalog(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"identity_entry_facts",
		"identity_direct_edges",
		"identity_conversation_edges",
		"identity_directory",
		"identity_rollups",
		"domain_rollups",
		"relationship_rollups",
		"relationship_daily",
	}, identityindex.RequiredDatasets)
}

func TestCacheRevisionIncludesRelationshipAnchor(t *testing.T) {
	state := CacheSyncState{
		SchemaVersion:          15,
		PublishedAt:            time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		RelationshipAnchorDate: "2026-07-27",
	}
	changed := state
	changed.RelationshipAnchorDate = "2026-07-28"
	assert.NotEqual(t, state.Revision(), changed.Revision())
}

func TestCacheSyncStateEncodesStatsAsNestedObject(t *testing.T) {
	requirementsForTest := require.New(t)
	minYear := int64(2004)
	maxYear := int64(2026)
	state := CacheSyncState{
		Stats: identityindex.CacheStatsSummary{
			TotalMessages:       23,
			Sources:             4,
			UniqueSenders:       11,
			UniqueDomains:       7,
			MinYear:             &minYear,
			MaxYear:             &maxYear,
			TotalSizeBytes:      1024,
			AttachmentSizeBytes: 256,
		},
	}

	data, err := json.Marshal(state)
	requirementsForTest.NoError(err)

	var encoded map[string]json.RawMessage
	requirementsForTest.NoError(json.Unmarshal(data, &encoded))
	requirementsForTest.Contains(encoded, "stats")

	var stats identityindex.CacheStatsSummary
	requirementsForTest.NoError(json.Unmarshal(encoded["stats"], &stats))
	assert.Equal(t, state.Stats, stats)
}

func TestDataBuilderPublishesProductionIdentityDatasets(t *testing.T) {
	assertionsForTest := assert.New(t)
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)

	for _, dataset := range identityindex.RequiredDatasets {
		hasParquet, err := datasetHasParquet(dir, dataset)
		require.NoError(t, err)
		assertionsForTest.True(hasParquet, dataset)
	}
	state, err := ReadCacheSyncState(dir)
	require.NoError(t, err)
	assertionsForTest.Equal(int64(5), state.Stats.TotalMessages)
	assertionsForTest.NotEmpty(state.ConversationParticipantsFingerprint)
	assertionsForTest.Equal("2026-07-15", state.RelationshipAnchorDate)
}

func TestAcquireReadyCacheReadLockRejectsAbsentCache(t *testing.T) {
	_, err := AcquireReadyCacheReadLock(context.Background(), t.TempDir())
	require.ErrorIs(t, err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Equal(t, CacheAbsent, unavailable.Readiness)
}

func TestInspectCacheReadiness(t *testing.T) {
	validState := CacheSyncState{
		LastMessageID: 1,
		LastSyncAt:    time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		PublishedAt:   time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC),
		SchemaVersion: CacheSchemaVersion,
	}

	tests := []struct {
		name  string
		setup func(*testing.T) string
		want  CacheReadiness
	}{
		{
			name: "absent",
			setup: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			want: CacheAbsent,
		},
		{
			name: "complete files and valid state",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := completeReadinessCache(t)
				writeReadinessState(t, dir, validState)
				return dir
			},
			want: CacheReady,
		},
		{
			name: "files without state",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := completeReadinessCache(t)
				require.NoError(t, os.Remove(CacheStatePath(dir)))
				return dir
			},
			want: CacheInterrupted,
		},
		{
			name: "state without files",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				writeReadinessState(t, dir, validState)
				return dir
			},
			want: CacheInterrupted,
		},
		{
			name: "malformed state",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := completeReadinessCache(t)
				require.NoError(t, os.WriteFile(CacheStatePath(dir), []byte("not-json"), 0o600))
				return dir
			},
			want: CacheInterrupted,
		},
		{
			name: "incomplete files",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := completeReadinessCache(t)
				require.NoError(t, os.RemoveAll(filepath.Join(dir, datasetConversations)))
				writeReadinessState(t, dir, validState)
				return dir
			},
			want: CacheInterrupted,
		},
		{
			name: "zero completion time",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := completeReadinessCache(t)
				writeReadinessState(t, dir, CacheSyncState{LastMessageID: 1})
				return dir
			},
			want: CacheInterrupted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			got, err := InspectCacheReadiness(dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInspectCacheReadinessNamesStaleSchemaAndDrift(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := completeReadinessCache(t)
	state, err := ReadCacheSyncState(dir)
	require.NoError(err)

	state.SchemaVersion = CacheSchemaVersion - 1
	writeReadinessState(t, dir, state)
	readiness, err := InspectCacheReadiness(dir)
	require.NoError(err)
	assert.Equal(CacheStaleSchema, readiness)

	state.SchemaVersion = CacheSchemaVersion
	writeReadinessState(t, dir, state)
	require.NoError(os.WriteFile(filepath.Join(dir, datasetSources, "drift.parquet"), []byte("drift"), 0o600))
	readiness, err = InspectCacheReadiness(dir)
	require.NoError(err)
	assert.Equal(CacheDrifted, readiness)
}

func TestCacheSchemaVersionRequiresIdentityRollupPublication(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	assertions.Equal(15, CacheSchemaVersion)

	dir := completeReadinessCache(t)
	state, err := ReadCacheSyncState(dir)
	requirements.NoError(err)
	state.SchemaVersion = 11
	writeReadinessState(t, dir, state)

	readiness, err := InspectCacheReadiness(dir)
	requirements.NoError(err)
	assertions.Equal(CacheStaleSchema, readiness)
}

func TestInspectCacheReadinessPrefersStaleSchemaWhenNewDatasetIsMissing(t *testing.T) {
	require := require.New(t)
	dir := completeReadinessCache(t)
	state, err := ReadCacheSyncState(dir)
	require.NoError(err)
	state.SchemaVersion = CacheSchemaVersion - 1
	require.NoError(os.RemoveAll(filepath.Join(dir, datasetParticipantIdentifiers)))
	writeReadinessState(t, dir, state)

	readiness, err := InspectCacheReadiness(dir)
	require.NoError(err)
	assert.Equal(t, CacheStaleSchema, readiness)
}

func TestCacheRevisionUsesOnlyCommittedStateWatermarks(t *testing.T) {
	assert := assert.New(t)
	state := CacheSyncState{
		SchemaVersion:          CacheSchemaVersion,
		LastMessageID:          41,
		LastCompletedSyncRunID: 5,
		LastCacheAdditionCount: 37,
		LastCacheUpdateCount:   3,
		LastFailedSyncRunCount: 2,
		LastFailedSyncRunIDSum: 19,
		IdentityRevision:       7,
		PublishedAt:            time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
	}
	revision := state.Revision()
	require.NotEmpty(t, revision)

	changed := state
	changed.LastCacheUpdateCount++
	assert.NotEqual(revision, changed.Revision())
	changed = state
	changed.LastFailedSyncRunIDSum++
	assert.NotEqual(revision, changed.Revision())
	changed = state
	changed.IdentityRevision++
	assert.NotEqual(revision, changed.Revision())
	changed = state
	changed.PublishedAt = changed.PublishedAt.Add(time.Second)
	assert.NotEqual(revision, changed.Revision())
	changed = state
	changed.DatasetFingerprint = "filesystem-only"
	assert.Equal(revision, changed.Revision(), "revision inputs are committed state watermarks, not ambient filesystem state")
}

func TestInspectCacheReadinessReturnsFilesystemErrors(t *testing.T) {
	dir := t.TempDir()
	analyticsPath := filepath.Join(dir, "analytics")
	require.NoError(t, os.WriteFile(analyticsPath, []byte("not-a-directory"), 0o600))

	_, err := InspectCacheReadiness(analyticsPath)
	require.Error(t, err)
	assert.NotErrorIs(t, err, os.ErrNotExist)
}

func completeReadinessCache(t *testing.T) string {
	t.Helper()
	dir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)
	return dir
}

func writeReadinessState(t *testing.T, dir string, state CacheSyncState) {
	t.Helper()
	if state.DatasetFingerprint == "" {
		fingerprint, err := CacheDatasetFingerprint(dir)
		if err == nil {
			state.DatasetFingerprint = fingerprint
		}
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(CacheStatePath(dir), data, 0o600))
}
