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
	var blockedWriters int
	require.Eventually(func() bool {
		// Replacements reserve identity before discovery. The next writer can
		// therefore wait on the first writer rather than directly on blocker.
		err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*)
			FROM pg_stat_activity a
			WHERE a.datname = current_database() AND (
				$1 = ANY(pg_blocking_pids(a.pid)) OR EXISTS (
					SELECT 1 FROM pg_stat_activity holding
					WHERE holding.datname = current_database()
					  AND holding.pid = ANY(pg_blocking_pids(a.pid))
					  AND $1 = ANY(pg_blocking_pids(holding.pid))
				))`, blockerPID).Scan(&blockedWriters)
		return err == nil && blockedWriters == 2
	}, 5*time.Second, 10*time.Millisecond,
		"both replacements must wait behind the held discovery lock")
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
