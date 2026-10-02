package provideridentity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type snapshotInventory struct {
	snapshot fastmail.Snapshot
	calls    int
}

func (i *snapshotInventory) ListIdentityRecords(context.Context) ([]fastmail.Record, error) {
	return i.snapshot.Records, nil
}

func (i *snapshotInventory) ListIdentitySnapshot(context.Context) (fastmail.Snapshot, error) {
	i.calls++
	return i.snapshot, nil
}

type failingOutcomeStore struct {
	*store.Store

	fail bool
}

func (s *failingOutcomeStore) RecordProviderIdentityRefreshOutcomeContext(ctx context.Context, sourceID int64, refreshErr error) error {
	if s.fail {
		return errors.New("synthetic marker write failure")
	}
	return s.Store.RecordProviderIdentityRefreshOutcomeContext(ctx, sourceID, refreshErr)
}

func TestAutoRefreshRetriesFailedOutcomeDespiteFreshDurableMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := &failingOutcomeStore{Store: testutil.NewTestStore(t)}
	source, err := st.GetOrCreateSource("gmail", "inbox@example.test")
	require.NoError(err)
	cfg := &config.Config{Fastmail: []config.FastmailSource{{SourceID: source.ID, APIToken: "synthetic-token", AutoConfirmIdentities: true}}}
	inventory := &snapshotInventory{snapshot: fastmail.Snapshot{State: "one", Records: []fastmail.Record{{ID: "mask", Identifier: "mask@example.test", Kind: "masked-email", State: "enabled"}}}}
	factory := func(string) provideridentity.Inventory { return inventory }
	_, _, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	before, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	require.True(before.Fresh(time.Now(), provideridentity.RefreshStaleAfter))
	st.fail = true
	inventory.snapshot.State = "two"
	inventory.snapshot.Records[0].Description = "Changed metadata"
	_, _, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.ErrorContains(err, "synthetic marker write failure")
	require.Equal(2, inventory.calls)
	st.fail = false
	_, _, err = provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	assert.Equal(3, inventory.calls, "failed outcome recording owes one provider recheck, even with an earlier fresh durable success")
	after, _, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal(before, after, "the unchanged-state retry still makes no durable outcome write")
	_, _, err = provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	assert.Equal(3, inventory.calls, "the successful no-op recheck restores idle freshness")
}
