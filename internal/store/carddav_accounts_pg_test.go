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
	var blockedDiscoveryWriters, blockedFenceWriters int
	require.Eventually(func() bool {
		err := st.DB().QueryRowContext(ctx, `SELECT
			COUNT(*) FILTER (WHERE POSITION('carddav_discovery_lock' IN query) > 0),
			COUNT(*) FILTER (WHERE POSITION('pg_advisory_xact_lock_shared' IN query) > 0)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(
			&blockedDiscoveryWriters, &blockedFenceWriters)
		return err == nil && blockedDiscoveryWriters == 2 && blockedFenceWriters == 0
	}, 5*time.Second, 10*time.Millisecond,
		"replacement transactions should share the delivery fence and queue on the discovery lock")
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

func TestPostgreSQLCardDAVDiscoveryWaitsForDeliveryFenceBeforeDiscoveryLocks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for CardDAV discovery lock ordering")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	_, _, err := st.ReplaceCardDAVDiscoveryContext(ctx,
		cardDAVConcurrentInput("before", "identity-before"))
	require.NoError(err, "create the account whose identity the replacement changes")

	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err, "begin the transaction holding the delivery fence")
	blockerOpen := true
	t.Cleanup(func() {
		if blockerOpen {
			_ = blocker.Rollback()
		}
	})
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID),
		"read the backend holding the delivery fence")
	_, err = blocker.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(
			hashtextextended('msgvault.delivery_admission:' || current_schema(), 0))`)
	require.NoError(err, "hold the exclusive delivery admission fence")

	replacementCtx, cancelReplacement := context.WithTimeout(ctx, 10*time.Second)
	replacementDone := make(chan error, 1)
	replacementReturned := false
	t.Cleanup(func() {
		cancelReplacement()
		if !replacementReturned {
			select {
			case <-replacementDone:
			case <-time.After(time.Second):
			}
		}
	})
	go func() {
		_, _, replaceErr := st.ReplaceCardDAVDiscoveryContext(replacementCtx,
			cardDAVConcurrentInput("after", "identity-after"))
		replacementDone <- replaceErr
	}()

	require.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.datname = current_database()
				  AND activity.pid <> pg_backend_pid()
				  AND ? = ANY(pg_blocking_pids(activity.pid))
				  AND activity.wait_event_type = 'Lock'
				  AND activity.query LIKE '%pg_advisory_xact_lock_shared%'
			)`), blockerPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond,
		"identity-changing discovery must wait on the delivery admission fence")

	probe, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err, "begin a probe transaction for the discovery lock")
	probeOpen := true
	t.Cleanup(func() {
		if probeOpen {
			_ = probe.Rollback()
		}
	})
	var singleton int
	require.NoError(probe.QueryRowContext(ctx,
		`SELECT singleton FROM carddav_discovery_lock WHERE singleton = 1 FOR UPDATE NOWAIT`).Scan(&singleton),
		"the replacement must wait on the delivery fence before acquiring the discovery lock")
	require.NoError(probe.Rollback(), "release the discovery lock probe")
	probeOpen = false

	require.NoError(blocker.Commit(), "release the delivery admission fence")
	blockerOpen = false
	replaceErr := <-replacementDone
	replacementReturned = true
	require.NoError(replaceErr, "the replacement must finish after the fence is released")

	account, err := st.GetCardDAVAccountByIDContext(ctx, store.DefaultCardDAVAccountID)
	require.NoError(err, "read the replaced account")
	require.NotNil(account)
	assert.Equal("after", account.Username)
	assert.Equal(int64(2), account.ConnectionGeneration)
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
