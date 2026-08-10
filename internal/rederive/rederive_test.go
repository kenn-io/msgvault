package rederive

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLedgerKeyIsPerSourceAndVersion(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("rederive:beeper:instagramgo:v1", LedgerKey("beeper", "instagramgo", "v1"))
	// One account healing must not mark another as done, and a version bump
	// must make an already-healed source stale again.
	assert.NotEqual(LedgerKey("beeper", "instagramgo", "v1"), LedgerKey("beeper", "whatsapp", "v1"))
	assert.NotEqual(LedgerKey("beeper", "instagramgo", "v1"), LedgerKey("beeper", "instagramgo", "v2"))
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	assert := assert.New(t)
	noop := func(context.Context, *store.Store, int64, func(string)) (*Summary, error) { return &Summary{}, nil }
	Register("test-dup", "v1", noop)
	t.Cleanup(func() { delete(registry, "test-dup") })
	// A silently shadowed pass would leave archives unrepaired with no signal.
	assert.Panics(func() { Register("test-dup", "v1", noop) })
}

func TestLookupReportsUnregisteredTypes(t *testing.T) {
	_, _, ok := Lookup("not-a-source-type")
	assert.False(t, ok)
}

func TestSummaryAddAccumulates(t *testing.T) {
	assert := assert.New(t)
	total := &Summary{}
	total.Add(&Summary{MessagesScanned: 3, BodiesRewritten: 2, Errors: 1})
	total.Add(&Summary{MessagesScanned: 4, AttachmentsTagged: 5, Undecodable: 1})
	total.Add(nil)
	assert.Equal(int64(7), total.MessagesScanned)
	assert.Equal(int64(2), total.BodiesRewritten)
	assert.Equal(int64(5), total.AttachmentsTagged)
	assert.Equal(int64(1), total.Undecodable)
	assert.Equal(int64(1), total.Errors)
}

// TestRunIfStaleRecordsAndSkips covers the gate that keeps a healed archive
// from re-deriving on every sync, and the retry left behind by a failed pass.
func TestRunIfStaleRecordsAndSkips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	calls := 0
	Register("test-gate", "v1", func(context.Context, *store.Store, int64, func(string)) (*Summary, error) {
		calls++
		return &Summary{MessagesScanned: 10}, nil
	})
	t.Cleanup(func() { delete(registry, "test-gate") })

	sum, ran, err := RunIfStale(context.Background(), st, "test-gate", "acct", 1, nil)
	require.NoError(err)
	require.True(ran)
	require.NotNil(sum)
	assert.Equal(int64(10), sum.MessagesScanned)
	assert.Equal(1, calls)
	assert.Equal(int64(1), derivedDataRevision(t, st),
		"a sync-triggered repair must invalidate the analytics cache")

	_, ran, err = RunIfStale(context.Background(), st, "test-gate", "acct", 1, nil)
	require.NoError(err)
	assert.False(ran, "an already-recorded pass must not run again")
	assert.Equal(1, calls)
	assert.Equal(int64(1), derivedDataRevision(t, st),
		"skipping an applied pass must not invalidate the cache again")

	// A different account of the same type is tracked separately.
	_, ran, err = RunIfStale(context.Background(), st, "test-gate", "other", 2, nil)
	require.NoError(err)
	assert.True(ran)
	assert.Equal(2, calls)
	assert.Equal(int64(2), derivedDataRevision(t, st),
		"each completed source repair must advance cache freshness")
}

func TestRunIfStaleEmptyPassRecordsWithoutAdvancingRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	calls := 0
	Register("test-empty", "v1", func(context.Context, *store.Store, int64, func(string)) (*Summary, error) {
		calls++
		return &Summary{}, nil
	})
	t.Cleanup(func() { delete(registry, "test-empty") })

	sum, ran, err := RunIfStale(context.Background(), st, "test-empty", "acct", 1, nil)
	require.NoError(err)
	require.True(ran)
	require.NotNil(sum)
	assert.Zero(sum.MessagesScanned)
	assert.Equal(1, calls)
	assert.Zero(derivedDataRevision(t, st),
		"an empty source has no derived rows that could stale the analytics cache")

	applied, err := st.IsMigrationApplied(LedgerKey("test-empty", "acct", "v1"))
	require.NoError(err)
	assert.True(applied, "an empty successful pass must still be recorded")

	_, ran, err = RunIfStale(context.Background(), st, "test-empty", "acct", 1, nil)
	require.NoError(err)
	assert.False(ran, "the recorded empty pass must not repeat on every sync")
	assert.Equal(1, calls)
}

func TestRunIfStaleRetriesSummaryErrorsWithoutRecordingLedger(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	calls := 0
	Register("test-partial", "v1", func(context.Context, *store.Store, int64, func(string)) (*Summary, error) {
		calls++
		return &Summary{MessagesScanned: 3, Errors: 1}, nil
	})
	t.Cleanup(func() { delete(registry, "test-partial") })

	sum, ran, err := RunIfStale(context.Background(), st, "test-partial", "acct", 1, nil)
	require.NoError(err)
	require.True(ran)
	require.NotNil(sum)
	assert.Equal(int64(1), sum.Errors)

	applied, err := st.IsMigrationApplied(LedgerKey("test-partial", "acct", "v1"))
	require.NoError(err)
	assert.False(applied, "a pass with row-level errors must remain retryable")
	assert.Equal(int64(1), derivedDataRevision(t, st),
		"partial writes must still invalidate the analytics cache")

	_, ran, err = RunIfStale(context.Background(), st, "test-partial", "acct", 1, nil)
	require.NoError(err)
	assert.True(ran, "the next sync must retry an incomplete pass")
	assert.Equal(2, calls)
	assert.Equal(int64(2), derivedDataRevision(t, st))
}

// TestRunRecordsSoASyncDoesNotRepeatIt covers the on-demand path: it runs even
// when the ledger already holds the pass, but still records it, so repairing by
// hand does not leave the next sync re-scanning a current archive.
func TestRunRecordsSoASyncDoesNotRepeatIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	calls := 0
	Register("test-run", "v1", func(context.Context, *store.Store, int64, func(string)) (*Summary, error) {
		calls++
		return &Summary{MessagesScanned: 7}, nil
	})
	t.Cleanup(func() { delete(registry, "test-run") })

	sum, err := Run(context.Background(), st, "test-run", "acct", 1, nil)
	require.NoError(err)
	assert.Equal(int64(7), sum.MessagesScanned)
	assert.Equal(1, calls)
	assert.Equal(int64(1), derivedDataRevision(t, st),
		"an on-demand repair must invalidate the analytics cache")

	// A later sync sees the archive as current.
	_, ran, err := RunIfStale(context.Background(), st, "test-run", "acct", 1, nil)
	require.NoError(err)
	assert.False(ran, "an on-demand repair must record the ledger")
	assert.Equal(1, calls)
	assert.Equal(int64(1), derivedDataRevision(t, st))

	// Asking again by hand still re-runs: that is the point of the escape hatch.
	_, err = Run(context.Background(), st, "test-run", "acct", 1, nil)
	require.NoError(err)
	assert.Equal(2, calls)
	assert.Equal(int64(2), derivedDataRevision(t, st))
}

func derivedDataRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRow(`
		SELECT COALESCE(CAST((
			SELECT value FROM archive_metadata WHERE key = 'derived_data_revision'
		) AS BIGINT), 0)
	`).Scan(&revision))
	return revision
}

func TestRunRejectsUnregisteredType(t *testing.T) {
	_, err := Run(context.Background(), testutil.NewTestStore(t), "no-such-type", "acct", 1, nil)
	require.Error(t, err)
}

func TestRunIfStaleIsNoOpForUnregisteredType(t *testing.T) {
	_, ran, err := RunIfStale(context.Background(), testutil.NewTestStore(t), "no-such-type", "acct", 1, nil)
	require.NoError(t, err)
	assert.False(t, ran)
}
