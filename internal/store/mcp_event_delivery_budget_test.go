package store_test

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsPrepareReportsMoreAfterFilteredRowBudget(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	now := time.Now().UTC()
	_, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err)
	// The subscription excludes own messages, so these rows are skipped
	// without a delivery. The last row is the first one it receives.
	const filtered = 300
	owner := f.EnsureParticipant("budget-owner@example.net", "Synthetic owner", "example.net")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "budget-owner@example.net", "manual"))
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now})
	for seq := 1; seq <= filtered+1; seq++ {
		message := f.NewMessage().Build()
		if seq <= filtered {
			message.SenderID.Int64 = owner
			message.SenderID.Valid = true
		}
		_, err := live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message})
		require.NoError(err)
	}
	build := func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"after-filtered-run"}`), nil
	}

	first, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
	require.NoError(err)
	require.NotNil(first, "a call that exhausts its row budget must report progress")
	assert.True(first.More)
	assert.Zero(first.Subscription.PendingSeq)
	progressed, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	assert.Equal(int64(256), progressed.CursorSeq, "skipped rows advance the durable cursor")
	assert.Zero(progressed.PendingSeq)

	second, err := f.Store.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, build)
	require.NoError(err)
	require.NotNil(second)
	assert.False(second.More)
	assert.Equal(int64(filtered+1), second.Event.Seq)
	assert.Equal(1, second.Subscription.AttemptCount)
}
