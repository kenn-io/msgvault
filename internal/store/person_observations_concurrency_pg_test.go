package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func TestObservationConcurrentDuplicateOnPostgres(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock contract")
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type outcome struct {
		result personobs.BatchResult
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "test")
			results <- outcome{result, err}
		}()
	}
	close(start)
	var outcomes []string
	var ids []int64
	for range 2 {
		result := <-results
		require.NoError(result.err)
		require.Len(result.result.Items, 1)
		outcomes = append(outcomes, result.result.Items[0].Outcome)
		ids = append(ids, result.result.Items[0].ObservationID)
	}
	assert.ElementsMatch([]string{"created", "reinforced"}, outcomes)
	assert.Equal(ids[0], ids[1])
	detail := observationGetConcurrency(t, f.store, ids[0])
	assert.Len(detail.Events, 2)
}

func TestObservationBatchSerializesWithUntrackingOnPostgres(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock contract")
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	lockTx, err := f.store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = lockTx.Rollback() }()
	personID := strconv.FormatInt(in.PersonID, 10)
	lockKey := "person-fact-generation|int64:" + strconv.Itoa(len(personID)) + ":" + personID
	_, err = lockTx.ExecContext(ctx, f.store.Rebind(`
		SELECT pg_advisory_xact_lock(hashtextextended(CAST(? AS TEXT), 0))`), lockKey)
	require.NoError(err)
	result, err := lockTx.ExecContext(ctx, f.store.Rebind(
		`DELETE FROM person_tracking WHERE person_id=?`), in.PersonID)
	require.NoError(err)
	affected, err := result.RowsAffected()
	require.NoError(err)
	require.Equal(int64(1), affected)

	type batchOutcome struct {
		result personobs.BatchResult
		err    error
	}
	results := make(chan batchOutcome, 1)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "extractor")
		results <- batchOutcome{result: result, err: err}
	}()

	var early *batchOutcome
	observedWait := assert.Eventually(func() bool {
		select {
		case outcome := <-results:
			early = &outcome
			return true
		default:
		}
		var waiting bool
		err := f.store.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype='advisory' AND NOT granted
			  AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
		)`).Scan(&waiting)
		return err == nil && waiting
	}, 15*time.Second, 100*time.Millisecond,
		"machine batch should wait for the tracking generation lock")

	require.NoError(lockTx.Commit())
	var applied batchOutcome
	if early != nil {
		applied = *early
	} else {
		applied = <-results
	}
	assert.True(observedWait)
	assert.Nil(early, "machine batch must not pass the held tracking generation lock")
	require.ErrorIs(applied.err, store.ErrPersonFactPersonNotTracked)
	var count int
	require.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind(
		`SELECT COUNT(*) FROM person_observations WHERE person_id=?`), in.PersonID).Scan(&count))
	assert.Zero(count)
}

func observationGetConcurrency(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	require := require.New(t)
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	return detail
}
