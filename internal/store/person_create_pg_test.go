package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

// TestStandalonePersonCreateLockOrderMatchesFactGeneration holds the
// organizations table the way person-fact generation does, waits until a
// creation with org and note parks behind it, and then asks for the Notes
// definition row as fact generation would next. Creation must not hold that
// row yet: if it did, the pair would form a lock cycle, and the blocker's
// deferred deadlock detector would make creation the victim.
func TestStandalonePersonCreateLockOrderMatchesFactGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*pgWaitBudget)
	defer cancel()

	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(ctx, `SET LOCAL deadlock_timeout = '1h'`)
	require.NoError(err)
	_, err = blocker.ExecContext(ctx, `LOCK TABLE organizations IN EXCLUSIVE MODE`)
	require.NoError(err)

	created := make(chan error, 1)
	go func() {
		_, createErr := st.CreateStandalonePersonContext(ctx, store.PersonCreateInput{
			Name: "Alex Example", Org: "Example Company", Title: "Engineer", Note: "Met at a conference",
		})
		created <- createErr
	}()
	waitForLockWait(t, st, "LOCK TABLE organizations", "creation did not park behind the organizations lock")

	var definitionID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM attribute_definitions WHERE universal_id = $1 FOR UPDATE`,
		store.AttributeUniversalIDNotes).Scan(&definitionID))
	require.NoError(blocker.Commit())

	select {
	case createErr := <-created:
		require.NoError(createErr, "creation must not deadlock against fact generation's lock order")
	case <-ctx.Done():
		require.FailNow("creation did not finish after the blocker committed", ctx.Err())
	}
	people, err := st.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Len(people, 1)
}
