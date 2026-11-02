package store_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hold the journal clock to pause the real CAS after policy admission and
// before stamps. A separate pool observes locks; the production Store has one
// connection, so adding an outer policy SQL transaction would deadlock here.
func TestEmbedGenGroupPolicyFenceSpansStampTransactionPostgres(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newEmbedGenGroupFixture(t, 1)
	if !f.store.IsPostgreSQL() {
		t.Skip("requires PostgreSQL scratch database")
	}
	versions, metadata := f.snapshot()
	var database, schema string
	requirements.NoError(f.store.DB().QueryRow(`SELECT current_database(), current_schema()`).Scan(&database, &schema))
	address, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
	requirements.NoError(err)
	address.Path = "/" + database
	params := address.Query()
	params.Set("search_path", schema)
	address.RawQuery = params.Encode()
	observer, err := sql.Open("pgx", address.String())
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, observer.Close()) })
	blocker, err := observer.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock_shared(hashtextextended('msgvault.embedding_change_clock',0))`)
	requirements.NoError(err)
	f.store.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		stamped bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		stamped, _, err := f.store.SetEmbedGenGroupIfUnchanged(ctx, versions, metadata, 4)
		done <- result{stamped, err}
	}()
	requirements.Eventually(func() bool {
		var waiting bool
		err := observer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock(%' AND query LIKE '%msgvault.embedding_change_clock%')`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "CAS must reach the journal lock before checking its policy fence")
	var exclusive bool
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('msgvault.conversation_policy',0))`).Scan(&exclusive))
	assertions.False(exclusive, "the stamp transaction must retain policy R after admission")
	requirements.NoError(blocker.Commit())
	select {
	case got := <-done:
		requirements.NoError(got.err)
		assertions.True(got.stamped)
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	assertions.Equal([]sql.NullInt64{{Int64: 4, Valid: true}}, f.embedGens())
}
