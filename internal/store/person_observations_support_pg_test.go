package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
)

func TestObservationConcurrentPremiseWithdrawalVersusDerivedInsert(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL lock serialization")
	}
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	status := observationStatusInput(t, f.store, in.PersonID, id, false, 1)
	in.Proposals = []personobs.ProposedObservation{{Level: personobs.LevelDeductive, Origin: personobs.OriginReasoning, Kind: personobs.KindState, Content: "[PERSON] exercises", ConfidenceBP: 800, Premises: []personobs.PremiseInput{{ID: id, Role: personobs.PremiseRoleSupport}}}}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errors := make(chan error, 2)
	result := make(chan personobs.BatchResult, 1)
	go func() { <-start; _, err := f.store.ApplyPersonFactGenerationContext(ctx, status, nil); errors <- err }()
	go func() {
		<-start
		out, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "test")
		result <- out
		errors <- err
	}()
	close(start)
	for range 2 {
		require.NoError(<-errors)
	}
	out := <-result
	require.Len(out.Items, 1)
	if out.Items[0].Outcome == "created" {
		assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, out.Items[0].ObservationID).Status)
	} else {
		assert.Equal("dropped-invalid", out.Items[0].Outcome)
	}
}
