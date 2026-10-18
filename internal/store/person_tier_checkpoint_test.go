package store_test

import (
	"context"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestScheduledPersonTierRunPersistsEachCommittedCheckpoint(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	first, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("first-checkpoint@example.org", "First Checkpoint", "example.org"))
	require.NoError(err)
	second, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("second-checkpoint@example.org", "Second Checkpoint", "example.org"))
	require.NoError(err)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	request := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500}
	run, err := f.Store.StartPersonTierRunContext(t.Context(), request, "scheduled", "apply")
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checkpoints := 0
	_, err = f.Store.ProcessPersonTierRunPageWithCheckpointContext(ctx, *run, 500, "people-tiers", func() {
		checkpoints++
		cancel()
	})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, checkpoints)

	resumed, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(resumed)
	assert.NotEmpty(resumed.Cursor)
	request.Cursor = resumed.Cursor
	remaining, err := f.Store.PlanPersonTiersContext(t.Context(), request)
	require.NoError(err)
	require.Len(remaining.People, 1)
	assert.Equal(second.ID, remaining.People[0].PersonID)
	assert.NotEqual(first.ID, remaining.People[0].PersonID)

	firstTracking, err := f.Store.GetPersonTrackingContext(t.Context(), first.ID)
	require.NoError(err)
	assert.True(firstTracking.Tracked)
	secondTracking, err := f.Store.GetPersonTrackingContext(t.Context(), second.ID)
	require.NoError(err)
	assert.False(secondTracking.Tracked)

	_, err = f.Store.ProcessPersonTierRunPageContext(t.Context(), *resumed, 500, "people-tiers")
	require.NoError(err)
	secondTracking, err = f.Store.GetPersonTrackingContext(t.Context(), second.ID)
	require.NoError(err)
	assert.True(secondTracking.Tracked)
	completed, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(completed)
}
