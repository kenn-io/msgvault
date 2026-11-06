package freshness

import (
	"context"
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
		success, warn             bool
		reason                    string
	}{
		{"fresh", "scheduled", "completed", time.Hour, true, false, ""},
		{"boundary", "manual", "completed", 24 * time.Hour, true, false, ""},
		{"old", "scheduled", "completed", 24*time.Hour + time.Second, true, false, "stale"},
		{"never", "scheduled", "never", 0, false, false, "never_synced"},
		{"partial", "scheduled", "partial", time.Hour, true, false, ""},
		{"failed recently", "scheduled", "failed", time.Hour, true, true, ""},
		{"cancelled", "manual", "cancelled", time.Hour, true, true, ""},
		{"paused", "manual", "paused", time.Hour, true, true, ""},
		{"running", "scheduled", "running", time.Hour, true, false, ""},
		{"failed never", "scheduled", "failed", 0, false, true, "never_synced"},
		{"unmeasured", "scheduled", "unmeasured", time.Hour, true, false, "unmeasured"},
		{"static", "static", "never", 0, false, false, ""},
		{"disabled", "disabled", "never", 0, false, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			source := SourceInventoryItem{SourceType: "gmail", Identifier: "owner@example.test", Scheduling: tt.scheduling, Outcome: tt.outcome, SyncCommand: "msgvault sync-incremental owner@example.test"}
			if tt.success {
				last := now.Add(-tt.age)
				source.LastSuccessfulSyncAt = &last
			}
			got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour, Sources: []SourceInventoryItem{source}})
			requirements.NoError(err)
			requirements.Len(got.Sources, 1)
			assertions.Equal(tt.warn, len(got.Warnings) > 0)
			assertions.Equal(tt.reason, got.Sources[0].Reason)
		})
	}
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
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	got, err := Build(t.Context(), Inputs{Now: now, StaleAfter: 24 * time.Hour, Sources: []SourceInventoryItem{
		{SourceType: "gmail", Identifier: "owner@example.test", Scheduling: "scheduled", Outcome: "failed", LastSuccessfulSyncAt: &future},
		{SourceType: "gmail", Identifier: "z-disabled@example.test", Scheduling: "disabled"},
	}})
	requirements.NoError(err)
	assertions.Empty(got.Sources[0].Reason)
	assertions.Equal([]string{"The latest sync failed for gmail source owner@example.test.", "Sync is disabled for gmail source z-disabled@example.test."}, got.Warnings)
}
