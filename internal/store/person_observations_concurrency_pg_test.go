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
func observationGetConcurrency(t *testing.T, st *store.Store, id int64) personobs.Detail {
	t.Helper()
	require := require.New(t)
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	return detail
}
