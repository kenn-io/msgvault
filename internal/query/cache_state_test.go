package query

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireReadyCacheReadLockRejectsAbsentCache(t *testing.T) {
	_, err := AcquireReadyCacheReadLock(context.Background(), t.TempDir())
	require.ErrorIs(t, err, ErrCacheUnavailable)
}

func TestInspectCacheReadiness(t *testing.T) {
	validState := CacheSyncState{
		LastMessageID: 1,
		LastSyncAt:    time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
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
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(CacheStatePath(dir), data, 0o600))
}
