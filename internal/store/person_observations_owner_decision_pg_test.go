package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

func TestPostgreSQLObservationRejectDecisionSerializesWithReinforcement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL owner decision and reinforcement ordering")
	}
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	before := observationGet(t, f.store, id)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	type decisionResult struct {
		row personobs.Observation
		err error
	}
	type reinforcementResult struct {
		batch personobs.BatchResult
		err   error
	}
	decided := make(chan decisionResult, 1)
	reinforced := make(chan reinforcementResult, 1)
	go func() {
		<-start
		row, err := f.store.DecidePersonObservation(ctx, personobs.Decision{
			ObservationID: id, Action: personobs.DecisionActionReject,
			Reason: "wrong", Actor: "owner", ExpectedUpdatedAt: before.UpdatedAt,
		})
		decided <- decisionResult{row: row, err: err}
	}()
	go func() {
		<-start
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "owner")
		reinforced <- reinforcementResult{batch: result, err: err}
	}()
	close(start)

	decision := <-decided
	write := <-reinforced
	require.NoError(write.err)
	require.Len(write.batch.Items, 1)
	final := observationGet(t, f.store, id)
	suppressions, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)

	if decision.err == nil {
		assert.Equal(personobs.StatusRejected, decision.row.Status)
		assert.Equal(personobs.StatusRejected, final.Status)
		assert.Equal("suppressed", write.batch.Items[0].Outcome)
		require.Len(suppressions, 1)
		assert.Equal("wrong", suppressions[0].Reason)
		return
	}
	require.ErrorIs(decision.err, store.ErrObservationConflict)
	assert.Equal(personobs.StatusActive, final.Status)
	assert.Equal("reinforced", write.batch.Items[0].Outcome)
	assert.Empty(suppressions)
}

func TestPostgreSQLObservationRejectDecisionSerializesWithFactGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL fact-generation and owner-decision ordering")
	}
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	before := observationGet(t, f.store, id)
	status := observationStatusInput(t, f.store, in.PersonID, id, false, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	type decisionResult struct {
		row personobs.Observation
		err error
	}
	decided := make(chan decisionResult, 1)
	generated := make(chan error, 1)
	go func() {
		<-start
		_, err := f.store.ApplyPersonFactGenerationContext(ctx, status, nil)
		generated <- err
	}()
	go func() {
		<-start
		row, err := f.store.DecidePersonObservation(ctx, personobs.Decision{
			ObservationID: id, Action: personobs.DecisionActionReject,
			Reason: "wrong", Actor: "owner", ExpectedUpdatedAt: before.UpdatedAt,
		})
		decided <- decisionResult{row: row, err: err}
	}()
	close(start)
	require.NoError(<-generated)
	decision := <-decided
	final := observationGet(t, f.store, id)
	suppressions, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	if decision.err == nil {
		assert.Equal(personobs.StatusRejected, decision.row.Status)
		assert.Equal(personobs.StatusRejected, final.Status)
		require.Len(suppressions, 1)
		assert.Equal("wrong", suppressions[0].Reason)
		return
	}
	require.ErrorIs(decision.err, store.ErrObservationConflict)
	assert.Equal(personobs.StatusUnsupported, final.Status)
	assert.Empty(suppressions)
}
