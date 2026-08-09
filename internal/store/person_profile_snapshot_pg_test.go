package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPostgreSQLPersonProfileReadsUseOneRevisionSnapshot(t *testing.T) {
	for _, history := range []bool{false, true} {
		name := "current"
		if history {
			name = "history"
		}
		t.Run(name, func(t *testing.T) {
			assertPostgreSQLPersonProfileSnapshot(t, history)
		})
	}
}

func assertPostgreSQLPersonProfileSnapshot(t *testing.T, history bool) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL snapshot regression")
	}
	ctx := context.Background()
	personID := newTestPerson(t, st)
	before, err := st.GetPersonContext(ctx, personID)
	require.NoError(err)

	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(ctx, `LOCK TABLE person_contact_points IN ACCESS EXCLUSIVE MODE`)
	require.NoError(err)

	type readResult struct {
		revision  int64
		addresses []store.PersonAddress
		err       error
	}
	result := make(chan readResult, 1)
	go func() {
		if history {
			profile, readErr := st.GetPersonProfileHistoryContext(ctx, personID)
			if readErr != nil {
				result <- readResult{err: readErr}
				return
			}
			result <- readResult{revision: profile.Person.Revision, addresses: profile.Addresses}
			return
		}
		profile, readErr := st.GetPersonProfileContext(ctx, personID)
		if readErr != nil {
			result <- readResult{err: readErr}
			return
		}
		result <- readResult{revision: profile.Person.Revision, addresses: profile.Addresses}
	}()

	var lockErr error
	require.Eventually(func() bool {
		var waiting int
		lockErr = st.DB().QueryRowContext(ctx, `SELECT COUNT(*)
			FROM pg_locks l
			JOIN pg_class c ON c.oid = l.relation
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relname = 'person_contact_points'
			  AND n.nspname = current_schema()
			  AND l.mode = 'AccessShareLock'
			  AND NOT l.granted`).Scan(&waiting)
		return lockErr == nil && waiting > 0
	}, 5*time.Second, 10*time.Millisecond, "profile read must reach the blocked later table")
	require.NoError(lockErr)

	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind:   store.PersonAddressPostal,
		StreetAddress: new("123 Example St."),
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(blocker.Commit())

	select {
	case read := <-result:
		require.NoError(read.err)
		assert.Equal(before.Revision, read.revision)
		assert.Empty(read.addresses,
			"an old person revision must not be paired with a later profile row")
	case <-time.After(5 * time.Second):
		require.Fail("profile read did not finish after releasing the table lock")
	}
}
