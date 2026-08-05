package cmd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

type scheduledProviderInventory struct {
	records []fastmail.Record
	calls   int
}

func (i *scheduledProviderInventory) ListIdentityRecords(context.Context) ([]fastmail.Record, error) {
	i.calls++
	return append([]fastmail.Record(nil), i.records...), nil
}

func TestAutomaticProviderIdentityRefreshIsOptInAndRunsAfterIMAPCompletion(t *testing.T) {
	st := testutil.NewTestStore(t)
	const sourceIdentifier = "imaps://user@example.test@imap.example.test:993"
	source, err := st.GetOrCreateSource(sourceTypeIMAP, sourceIdentifier)
	require.NoError(t, err)

	savedCfg := cfg
	savedFactory := fastmailIdentityInventoryFactory
	t.Cleanup(func() {
		cfg = savedCfg
		fastmailIdentityInventoryFactory = savedFactory
	})

	t.Run("default off", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		cfg = &config.Config{Fastmail: []config.FastmailSource{{
			SourceID: source.ID, APIToken: "not-called-token",
		}}}
		calls := 0
		fastmailIdentityInventoryFactory = func(string) provideridentity.Inventory {
			calls++
			return &scheduledProviderInventory{}
		}

		summary := runAutomaticProviderSync(t, st, sourceIdentifier)

		requirements.NotNil(summary)
		assertions.Zero(calls)
	})

	t.Run("enabled", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		cfg = &config.Config{Fastmail: []config.FastmailSource{{
			SourceID: source.ID, APIToken: "provider-token", AutoConfirmIdentities: true,
		}}}
		fastmailIdentityInventoryFactory = func(string) provideridentity.Inventory {
			return &scheduledProviderInventory{records: []fastmail.Record{
				{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
				{Identifier: "waiting@example.test", State: "pending", Kind: "masked-email"},
			}}
		}

		summary := runAutomaticProviderSync(t, st, sourceIdentifier)

		requirements.NotNil(summary)
		identities, err := st.ListAccountIdentities(source.ID)
		requirements.NoError(err)
		requirements.Len(identities, 1)
		assertions.Equal("old@example.test", identities[0].Address)
		assertions.Equal("provider-alias", identities[0].SourceSignal)
	})
}

// setUpScheduledProviderIdentityRefresh wires the opt-in automatic refresh for a
// Gmail source whose cursor sits at history 100, and returns the inventory the
// post-completion hook would reach for.
func setUpScheduledProviderIdentityRefresh(
	t *testing.T,
	st *store.Store,
) (*store.Source, *scheduledProviderInventory) {
	t.Helper()
	const sourceIdentifier = "gmail-user@example.test"
	source, err := st.GetOrCreateSource(sourceTypeGmail, sourceIdentifier)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncCursor(source.ID, "100"))
	source.SyncCursor = sql.NullString{String: "100", Valid: true}

	savedCfg := cfg
	savedFactory := fastmailIdentityInventoryFactory
	t.Cleanup(func() {
		cfg = savedCfg
		fastmailIdentityInventoryFactory = savedFactory
	})
	cfg = &config.Config{Fastmail: []config.FastmailSource{{
		SourceID: source.ID, APIToken: "provider-token", AutoConfirmIdentities: true,
	}}}
	inventory := &scheduledProviderInventory{records: []fastmail.Record{{
		Identifier: "historical@example.test", State: "deleted", Kind: "masked-email",
	}}}
	fastmailIdentityInventoryFactory = func(string) provideridentity.Inventory {
		return inventory
	}
	return source, inventory
}

// runNoOpIncrementalProviderSync drives one incremental sync whose cursor
// already equals the mailbox's current history, so the run is a no-op.
func runNoOpIncrementalProviderSync(
	t *testing.T,
	st *store.Store,
	source *store.Source,
) *gmail.SyncSummary {
	t.Helper()
	client := gmail.NewMockAPI()
	client.Profile = &gmail.Profile{EmailAddress: source.Identifier, HistoryID: 100}
	options := msgsync.DefaultOptions()
	syncer := newMessageSyncer(client, st, options).WithLogger(slog.New(slog.DiscardHandler))
	summary, err := syncer.Incremental(t.Context(), source)
	require.NoError(t, err)
	require.NotNil(t, summary)
	return summary
}

// TestAutomaticProviderIdentityRefreshSkipsProviderInventoryOnNoOpIncrementalSync
// pins the cost side of the automatic refresh: scheduled incremental syncs run
// often and are usually no-ops, and a source whose inventory was refreshed
// recently must not spend a provider round trip on each of them.
func TestAutomaticProviderIdentityRefreshSkipsProviderInventoryOnNoOpIncrementalSync(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, inventory := setUpScheduledProviderIdentityRefresh(t, st)
	requirements.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, nil),
		"the previous refresh succeeded recently")

	runNoOpIncrementalProviderSync(t, st, source)

	assertions.Zero(inventory.calls,
		"an unchanged mailbox with a fresh inventory must not cost a provider round trip")
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	assertions.Empty(identities)
}

// TestAutomaticProviderIdentityRefreshRunsOnNoOpIncrementalSyncWhenNeverRefreshed
// pins the coverage side: enabling auto_confirm_identities on an idle mailbox
// must take effect on the next scheduled sync, not wait for unrelated mail.
func TestAutomaticProviderIdentityRefreshRunsOnNoOpIncrementalSyncWhenNeverRefreshed(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, inventory := setUpScheduledProviderIdentityRefresh(t, st)

	runNoOpIncrementalProviderSync(t, st, source)

	assertions.Equal(1, inventory.calls,
		"a source that has never refreshed owes an inventory read even without mailbox history")
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("historical@example.test", identities[0].Address)

	runNoOpIncrementalProviderSync(t, st, source)

	assertions.Equal(1, inventory.calls,
		"the successful refresh is recorded, so the next no-op sync skips the provider")
}

// TestAutomaticProviderIdentityRefreshRetriesOnNoOpIncrementalSyncAfterFailure
// pins failure recovery: a transient provider failure must be retried by the
// next scheduled sync even when the mailbox itself has not moved.
func TestAutomaticProviderIdentityRefreshRetriesOnNoOpIncrementalSyncAfterFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, inventory := setUpScheduledProviderIdentityRefresh(t, st)
	requirements.NoError(st.RecordProviderIdentityRefreshOutcomeContext(
		t.Context(), source.ID, errors.New("provider unavailable"),
	), "the previous refresh failed")

	runNoOpIncrementalProviderSync(t, st, source)

	assertions.Equal(1, inventory.calls,
		"a failed refresh owes a retry on the next sync, no-op or not")
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("historical@example.test", identities[0].Address)
}

func TestAutomaticProviderIdentityRefreshRunsAfterIncrementalGmailCompletion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, inventory := setUpScheduledProviderIdentityRefresh(t, st)

	client := gmail.NewMockAPI()
	client.Profile = &gmail.Profile{EmailAddress: source.Identifier, HistoryID: 200}
	client.HistoryID = 200
	options := msgsync.DefaultOptions()
	syncer := newMessageSyncer(client, st, options).WithLogger(slog.New(slog.DiscardHandler))

	summary, err := syncer.Incremental(t.Context(), source)

	requirements.NoError(err)
	requirements.NotNil(summary)
	assertions.Equal(1, inventory.calls, "advanced history refreshes the provider inventory")
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("historical@example.test", identities[0].Address)
	assertions.Equal("provider-alias", identities[0].SourceSignal)
}

func runAutomaticProviderSync(
	t *testing.T,
	st *store.Store,
	sourceIdentifier string,
) *gmail.SyncSummary {
	t.Helper()
	client := gmail.NewMockAPI()
	client.Profile = &gmail.Profile{EmailAddress: "user@example.test", HistoryID: 100}
	options := msgsync.DefaultOptions()
	options.SourceType = sourceTypeIMAP
	syncer := newMessageSyncer(client, st, options).WithLogger(slog.New(slog.DiscardHandler))
	summary, err := syncer.Full(t.Context(), sourceIdentifier)
	require.NoError(t, err)
	return summary
}
