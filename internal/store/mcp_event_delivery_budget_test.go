package store_test

import (
	"fmt"
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
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	require.NoError(err)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	require.NoError(err)
	// The subscription excludes own messages, so these rows are skipped
	// without a delivery. The last row is the first one it receives.
	const filtered = 300
	recorded := now.Format(time.RFC3339Nano)
	for seq := 1; seq <= filtered+1; seq++ {
		fromMe := seq <= filtered
		_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?,?,?,?,?,?,?)`), seq, clock.Epoch, f.ConvID, fmt.Sprintf("message:%d", seq), seq, f.ConvID, f.Source.ID, fromMe, recorded, recorded, fmt.Sprintf(`{"kind":"message","from_me":%t}`, fromMe))
		require.NoError(err)
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), filtered+1)
	require.NoError(err)
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
