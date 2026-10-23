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

func TestObservationConcurrentDuplicateOnPostgres(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock contract")
	}
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
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
		require.NoError(t, result.err)
		require.Len(t, result.result.Items, 1)
		outcomes = append(outcomes, result.result.Items[0].Outcome)
		ids = append(ids, result.result.Items[0].ObservationID)
	}
	assert.ElementsMatch(t, []string{"created", "reinforced"}, outcomes)
	assert.Equal(t, ids[0], ids[1])
	detail := observationGetConcurrency(t, f.store, ids[0])
	assert.Len(t, detail.Events, 2)
}
func TestObservationConcurrentRejectVersusReinforce(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock contract")
	}
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	batch, err := f.store.PreparePersonObservationBatch(t.Context(), in)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	rejected := make(chan error, 1)
	reinforced := make(chan personobs.BatchResult, 1)
	failed := make(chan error, 1)
	go func() { <-start; rejected <- f.store.RejectObservationForTest(ctx, id) }()
	go func() {
		<-start
		result, err := f.store.ApplyPersonObservationBatch(ctx, batch, generation, "test")
		reinforced <- result
		failed <- err
	}()
	close(start)
	require.NoError(t, <-rejected)
	result := <-reinforced
	require.NoError(t, <-failed)
	require.Len(t, result.Items, 1)
	detail := observationGetConcurrency(t, f.store, id)
	assert.Equal(t, personobs.StatusRejected, detail.Status)
	switch result.Items[0].Outcome {
	case "reinforced":
		assert.Len(t, detail.Events, 3)
	case "suppressed":
		assert.Len(t, detail.Events, 2)
	default:
		require.FailNow(t, "unexpected concurrent outcome", result.Items[0].Outcome)
	}
}

func observationGetConcurrency(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(t, err)
	return detail
}
