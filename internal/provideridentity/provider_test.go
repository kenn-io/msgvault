package provideridentity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeInventory struct {
	records []fastmail.Record
}

func (i *fakeInventory) ListIdentityRecords(context.Context) ([]fastmail.Record, error) {
	return append([]fastmail.Record(nil), i.records...), nil
}

func TestAutoRefreshDefaultOffMakesNoProviderCall(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	cfg := &config.Config{Fastmail: []config.FastmailSource{{
		SourceID: source.ID, APIToken: "not-called-token",
	}}}
	calls := 0

	outcomes, enabled, err := provideridentity.AutoRefresh(
		t.Context(), cfg, st, source.ID,
		func(string) provideridentity.Inventory {
			calls++
			return &fakeInventory{}
		},
	)

	requirements.NoError(err)
	assertions.False(enabled)
	assertions.Zero(calls)
	assertions.Empty(outcomes)
}

func TestAutoRefreshAppliesOnlyStrongEvidenceAndRetryIsIdempotent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	cfg := &config.Config{Fastmail: []config.FastmailSource{{
		SourceID: source.ID, APIToken: "provider-token", AutoConfirmIdentities: true,
	}}}
	inventory := &fakeInventory{records: []fastmail.Record{
		{Identifier: "active@example.test", State: "enabled", Kind: "masked-email"},
		{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
		{Identifier: "deleted@example.test", State: "deleted", Kind: "masked-email"},
		{Identifier: "waiting@example.test", State: "pending", Kind: "masked-email"},
		{Identifier: "*@example.test", State: "enabled", Kind: "identity"},
	}}
	factory := func(string) provideridentity.Inventory { return inventory }

	first, enabled, err := provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	requirements.Len(first, 3)
	for _, outcome := range first {
		assertions.True(outcome.Added)
		assertions.Equal([]string{"provider-alias"}, outcome.Signals)
	}

	retry, enabled, err := provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	assertions.Empty(retry)
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	assertions.Len(identities, 3)
}

type countingInventory struct {
	records []fastmail.Record
	err     error
	calls   int
}

func (i *countingInventory) ListIdentityRecords(context.Context) ([]fastmail.Record, error) {
	i.calls++
	if i.err != nil {
		return nil, i.err
	}
	return append([]fastmail.Record(nil), i.records...), nil
}

func TestAutoRefreshIfDueSkipsFreshStateAndRefreshesWhenOwed(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	cfg := &config.Config{Fastmail: []config.FastmailSource{{
		SourceID: source.ID, APIToken: "provider-token", AutoConfirmIdentities: true,
	}}}
	inventory := &countingInventory{records: []fastmail.Record{
		{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
	}}
	factory := func(string) provideridentity.Inventory { return inventory }

	// Never refreshed: the provider read is owed even without mailbox change.
	outcomes, enabled, err := provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	assertions.Equal(1, inventory.calls)
	requirements.Len(outcomes, 1)

	// The success was recorded, so a fresh state skips the provider entirely.
	outcomes, enabled, err = provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	assertions.Equal(1, inventory.calls, "a fresh refresh state must skip the provider round trip")
	assertions.Empty(outcomes)

	// AutoRefresh (mailbox changed) ignores freshness.
	_, enabled, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	assertions.Equal(2, inventory.calls, "a changed mailbox always refreshes")
}

func TestAutoRefreshRecordsFailureSoNoOpSyncsRetry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	cfg := &config.Config{Fastmail: []config.FastmailSource{{
		SourceID: source.ID, APIToken: "provider-token", AutoConfirmIdentities: true,
	}}}
	inventory := &countingInventory{err: errors.New("provider unavailable")}
	factory := func(string) provideridentity.Inventory { return inventory }

	_, enabled, err := provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	requirements.Error(err)
	assertions.True(enabled)
	assertions.Equal(1, inventory.calls)

	// The failure was recorded, so the next no-op sync retries instead of
	// treating the source as fresh.
	inventory.err = nil
	inventory.records = []fastmail.Record{
		{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
	}
	outcomes, enabled, err := provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	requirements.NoError(err)
	assertions.True(enabled)
	assertions.Equal(2, inventory.calls, "a recorded failure owes a retry")
	requirements.Len(outcomes, 1)
}
