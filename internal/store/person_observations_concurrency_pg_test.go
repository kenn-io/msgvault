package store_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
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

func TestObservationBatchSerializesWithPersonSplitOnPostgres(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL identity-row lock contract")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	survivor, err := f.store.GetPersonContext(ctx, f.alicePersonID)
	require.NoError(err)
	absorbed, err := f.store.GetPersonContext(ctx, f.bobPersonID)
	require.NoError(err)
	merged, err := f.store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "observation-split-race-merge",
		Actor:                    "test",
	})
	require.NoError(err)

	messageID := f.insertMessage(t, "observation-split-race", "email", f.bobID,
		time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	addSweepBody(t, f, messageID, "I run")
	item := sweepItem(t,
		loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0).Seeds,
		messageID)
	evidence := sweepEvidenceInput(t, item)
	offset := strings.Index(evidence.Excerpt, "I run")
	require.NotEqual(-1, offset)
	in.Proposals[0].Support = []personobs.SupportInput{{
		Evidence: evidence, Quote: "I run",
		QuoteStart: utf8.RuneCountInString(evidence.Excerpt[:offset]),
	}}
	batch, err := f.store.PreparePersonObservationBatch(ctx, in)
	require.NoError(err)
	require.Nil(batch.Items[0].Failure)

	lockTx, err := f.store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = lockTx.Rollback() }()
	var identityRevision string
	require.NoError(lockTx.QueryRowContext(ctx, f.store.Rebind(
		`SELECT value FROM archive_metadata WHERE key=? FOR UPDATE`),
		"identity_revision").Scan(&identityRevision))

	type splitOutcome struct {
		result *store.PersonSplitResult
		err    error
	}
	splitResults := make(chan splitOutcome, 1)
	splitRequest := store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{f.bobID},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "observation-split-race",
		Actor:                  "test",
	}
	go func() {
		result, err := f.store.SplitPersonMergeContext(ctx, splitRequest)
		splitResults <- splitOutcome{result: result, err: err}
	}()

	waitingForIdentity := func(minimum int) bool {
		var count int
		err := f.store.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock'
			  AND query ILIKE '%archive_metadata%'`).Scan(&count)
		return err == nil && count >= minimum
	}
	splitWaiting := assert.Eventually(func() bool {
		return waitingForIdentity(1)
	}, 15*time.Second, 100*time.Millisecond,
		"split should wait on the held identity row")
	require.True(splitWaiting)

	type batchOutcome struct {
		result personobs.BatchResult
		err    error
	}
	batchResults := make(chan batchOutcome, 1)
	go func() {
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "extractor")
		batchResults <- batchOutcome{result: result, err: err}
	}()

	batchWaiting := assert.Eventually(func() bool {
		return waitingForIdentity(2)
	}, 15*time.Second, 100*time.Millisecond,
		"observation batch should share the identity row lock before applying")
	require.True(batchWaiting)
	require.NoError(lockTx.Commit())

	split := <-splitResults
	require.NoError(split.err)
	require.NotNil(split.result)
	assert.True(split.result.ExactReversal)
	batchResult := <-batchResults
	require.NoError(batchResult.err)
	require.Len(batchResult.result.Items, 1)
	assert.Equal("dropped-invalid", batchResult.result.Items[0].Outcome)
	assert.Zero(batchResult.result.Items[0].ObservationID)

	var count int
	require.NoError(f.store.DB().QueryRowContext(ctx, f.store.Rebind(
		`SELECT COUNT(*) FROM person_observations WHERE person_id=? AND content=?`),
		in.PersonID, in.Proposals[0].Content).Scan(&count))
	assert.Zero(count)
}

func observationGetConcurrency(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	require := require.New(t)
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	return detail
}
