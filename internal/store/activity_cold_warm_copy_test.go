package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestColdEmailCopiesPreferEligibleWarmCopy(t *testing.T) {
	f := newTimelineActivityFixture(t)
	warmSource, err := f.Store.GetOrCreateSource("gmail", "warm@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(warmSource.ID, "owner@example.com", "manual"))
	warmRoom, err := f.Store.EnsureConversationWithType(warmSource.ID, "warm-copy", "email_thread", "Warm copy")
	require.NoError(t, err)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	coldID := f.add(t, f.ConvID, f.Source.ID, at, false, "<shared-copy@example.com>")
	warmID := f.add(t, warmRoom, warmSource.ID, at, false, "<shared-copy@example.com>")
	projectColdTimeline(t, f, "UTC")
	setTimelineConversationTier(t, f, f.Source.ID, "default-thread", conversationpolicy.Cold)
	projectColdTimeline(t, f, "UTC")
	retractTimelineActivity(t, f, coldID)

	for _, scope := range []struct {
		name     string
		sources  []int64
		wantWarm int
		wantCold int
	}{
		{"both sources", []int64{f.Source.ID, warmSource.ID}, 1, 0},
		{"cold source only", []int64{f.Source.ID}, 0, 1},
	} {
		t.Run(scope.name, func(t *testing.T) {
			ctx := sourcescope.WithSourceIDs(t.Context(), scope.sources)
			q := timelineBurstQuery()
			warm, err := f.Store.ListActivityBurstsContext(ctx, q)
			require.NoError(t, err)
			require.Len(t, warm.Bursts, scope.wantWarm)
			if scope.wantWarm == 1 {
				assert.Equal(t, []int64{warmID}, warm.Bursts[0].LatestMessageIDs)
			}
			cold, err := f.Store.ListColdActivityBurstsContext(ctx, q)
			require.NoError(t, err)
			assert.Len(t, cold.Bursts, scope.wantCold)
			count, err := f.Store.CountColdActivityMessagesContext(ctx, q)
			require.NoError(t, err)
			assert.Equal(t, scope.wantCold, count)
		})
	}
}
