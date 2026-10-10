package store_test

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPostgreSQLCardDAVDiscoveryReplacementsSerializeCompleteSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for the CardDAV snapshot serialization regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	blocker, err := st.DB().BeginTx(ctx, &sql.TxOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var singleton int
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT singleton FROM carddav_discovery_lock WHERE singleton = 1 FOR UPDATE`).Scan(&singleton))
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))

	inputs := []store.CardDAVDiscoveryInput{
		cardDAVConcurrentInput("bob", "snapshot-a"),
		cardDAVConcurrentInput("carol", "snapshot-b"),
	}
	errs := make(chan error, 2)
	for _, input := range inputs {
		go func() {
			_, _, replaceErr := st.ReplaceCardDAVDiscoveryContext(ctx, input)
			errs <- replaceErr
		}()
	}
	// The first replacement holds the identity fence while waiting on discovery.
	// The second therefore waits on the first replacement's identity fence.
	var discoveryPID, identityPID int
	require.Eventually(func() bool {
		err := st.DB().QueryRowContext(ctx, `SELECT discovery.pid, identity_writer.pid
			FROM pg_stat_activity discovery
			CROSS JOIN pg_stat_activity identity_writer
			WHERE discovery.datname = current_database()
			  AND identity_writer.datname = current_database()
			  AND $1 = ANY(pg_blocking_pids(discovery.pid))
			  AND POSITION('carddav_discovery_lock' IN discovery.query) > 0
			  AND discovery.pid = ANY(pg_blocking_pids(identity_writer.pid))
			  AND POSITION('archive_metadata' IN identity_writer.query) > 0`,
			blockerPID).Scan(&discoveryPID, &identityPID)
		return err == nil && discoveryPID > 0 && identityPID > 0
	}, 5*time.Second, 10*time.Millisecond,
		"replacements must queue through the discovery and identity locks")
	require.NoError(blocker.Commit())
	require.NoError(<-errs)
	require.NoError(<-errs)

	account, err := st.GetCardDAVAccountByIDContext(ctx, store.DefaultCardDAVAccountID)
	require.NoError(err)
	require.NotNil(account)
	assert.Equal(int64(2), account.ConnectionGeneration)
	assert.Equal(int64(2), account.DiscoveryRevision)
	books, err := st.ListCardDAVAddressBooksContext(ctx, store.AllCardDAVAccounts)
	require.NoError(err)
	got := make([]string, 0, len(books))
	for _, book := range books {
		got = append(got, book.CanonicalURL)
		assert.Equal([]string{"3.0", "4.0"}, book.SupportedVCardVersions)
	}
	sort.Strings(got)
	wantA := []string{"https://contacts.example/snapshot-a/one/", "https://contacts.example/snapshot-a/two/"}
	wantB := []string{"https://contacts.example/snapshot-b/one/", "https://contacts.example/snapshot-b/two/"}
	assert.True(slices.Equal(wantA, got) || slices.Equal(wantB, got),
		"final books must be exactly one authoritative snapshot: %v", got)
}

func cardDAVConcurrentInput(username, prefix string) store.CardDAVDiscoveryInput {
	return store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: username,
		PrincipalURL: "https://contacts.example/principal/" + username + "/",
		HomeURL:      "https://contacts.example/books/" + username + "/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example/" + prefix + "/one/", DiscoveryIndex: 0,
				SupportedVCardVersions: []string{"3.0", "4.0"}},
			{CanonicalURL: "https://contacts.example/" + prefix + "/two/", DiscoveryIndex: 1,
				SupportedVCardVersions: []string{"3.0", "4.0"}},
		},
	}
}
