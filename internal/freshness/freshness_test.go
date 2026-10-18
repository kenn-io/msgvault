package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceFreshnessPolicy(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, scheduling, outcome string
		age                       time.Duration
		success, stale, warn      bool
		reason                    string
	}{
		{"fresh", "scheduled", "completed", time.Hour, true, false, false, ""},
		{"boundary", "manual", "completed", 24 * time.Hour, true, false, false, ""},
		{"old", "scheduled", "completed", 24*time.Hour + time.Second, true, true, false, "source_stale:owner@example.test"},
		{"never", "scheduled", "never", 0, false, true, false, "source_never_synced:owner@example.test"},
		{"partial", "scheduled", "partial", time.Hour, true, false, false, ""},
		{"failed recently", "scheduled", "failed", time.Hour, true, false, true, ""},
		{"cancelled", "manual", "cancelled", time.Hour, true, false, true, ""},
		{"paused", "manual", "paused", time.Hour, true, false, true, ""},
		{"running", "scheduled", "running", time.Hour, true, false, false, ""},
		{"failed never", "scheduled", "failed", 0, false, true, true, "source_never_synced:owner@example.test"},
		{"unmeasured", "scheduled", "unmeasured", time.Hour, true, true, false, "source_unmeasured:owner@example.test"},
		{"static", "static", "never", 0, false, false, false, ""},
		{"disabled", "disabled", "never", 0, false, false, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := SourceInventoryItem{SourceType: "gmail", Identifier: "owner@example.test", Scheduling: tt.scheduling, Outcome: tt.outcome, SyncCommand: "msgvault sync-incremental owner@example.test"}
			if tt.success {
				last := now.Add(-tt.age)
				source.LastSuccessfulSyncAt = &last
			}
			got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour, Sources: []SourceInventoryItem{source}})
			require.NoError(err)
			assert.Equal(tt.stale, got.Stale)
			require.Len(got.Sources, 1)
			assert.Equal(tt.stale, got.Sources[0].Stale)
			assert.Equal(tt.warn, len(got.Warnings) > 0)
			if tt.reason != "" {
				assert.Equal([]string{tt.reason}, got.StaleReasons)
			} else {
				assert.Empty(got.StaleReasons)
			}
		})
	}
}

func TestProjectionFreshnessAndWireShape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	fresh := now.Add(-time.Minute)
	complete := false
	got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour, Projections: map[string]ProjectionState{
		"open_loops": {ComputedAt: &old, BackfillComplete: &complete, Extra: map[string]any{"queue_depth": 2}},
		"activity":   {}, "other": {ComputedAt: &fresh},
	}})
	require.NoError(err)
	assert.True(got.Stale)
	assert.Equal([]string{"projection_missing:activity", "projection_stale:open_loops"}, got.StaleReasons)
	assert.Equal(int64(90000), *got.Projections["open_loops"].AgeSeconds)
	assert.False(*got.Projections["open_loops"].BackfillComplete)
	assert.False(got.Projections["other"].Stale)
	encoded, err := json.Marshal(got)
	require.NoError(err)
	var fields map[string]any
	require.NoError(json.Unmarshal(encoded, &fields))
	assert.Equal([]any{}, fields["sources"])
	assert.InDelta(float64(86400), fields["stale_after_seconds"], 0)
	assert.Contains(fields, "as_of")
	assert.Contains(fields, "warnings")
}

func TestBuildCanceledOrInvalidThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Build(ctx, Inputs{Now: time.Now(), StaleAfter: time.Hour})
	require.ErrorIs(t, err, context.Canceled)
	_, err = Build(t.Context(), Inputs{Now: time.Now()})
	require.Error(t, err)
}

func TestFreshnessWarningsAndClockSkew(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour, Sources: []SourceInventoryItem{
		{SourceType: "gmail", Identifier: "owner@example.test", Scheduling: "disabled", Outcome: "failed", LastSuccessfulSyncAt: &future},
	}})
	require.NoError(err)
	assert.False(got.Stale)
	assert.Equal(int64(0), *got.Sources[0].AgeSeconds)
	assert.Equal([]string{"Sync is disabled for gmail source owner@example.test.", "The latest sync failed for gmail source owner@example.test."}, got.Warnings)
}

func TestRescanWarnsWithoutInvalidatingCertifiedProgress(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	computed := now.Add(-time.Minute)
	got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour,
		Projections: map[string]ProjectionState{"open_loops": {ComputedAt: &computed, Extra: map[string]any{"rescan_reason": "identity"}}}})
	require.NoError(t, err)
	assert.False(t, got.Stale)
	assert.Equal(t, []string{"The open_loops projection is reconciling archive changes."}, got.Warnings)
}
