//go:build pgvector

package pgvector

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
)

func TestDocumentBackendConcurrentFirstPutSerializesGenerationDimensionPostgres(t *testing.T) {
	db := openPGTestDB(t)
	parent, err := Open(t.Context(), Options{DB: db})
	require.NoError(t, err)
	backend := parent.DocumentBackend()
	require.NoError(t, EnsureDocumentVectorIndex(t.Context(), db, 3))
	require.NoError(t, EnsureDocumentVectorIndex(t.Context(), db, 4))

	for round := range 12 {
		generationID := document.GenerationID(100 + round)
		start := make(chan struct{})
		type result struct {
			dimension int
			err       error
		}
		results := make(chan result, 2)
		var ready sync.WaitGroup
		ready.Add(2)
		put := func(dimension int, embedding document.Embedding) {
			ready.Done()
			<-start
			results <- result{dimension: dimension, err: backend.PutUnpublished(
				t.Context(), generationID, dimension, []document.Embedding{embedding})}
		}
		go put(3, document.Embedding{Token: "concurrent-3-" + string(rune('a'+round)), Vector: []float32{1, 0, 0}})
		go put(4, document.Embedding{Token: "concurrent-4-" + string(rune('a'+round)), Vector: []float32{1, 0, 0, 0}})
		ready.Wait()
		close(start)

		first, second := <-results, <-results
		outcomes := []result{first, second}
		winnerDimension := 0
		for _, outcome := range outcomes {
			if outcome.err == nil {
				require.Zero(t, winnerDimension, "round %d admitted two dimensions", round)
				winnerDimension = outcome.dimension
				continue
			}
			require.ErrorIs(t, outcome.err, document.ErrInvalidVector, "round %d loser", round)
		}
		require.NotZero(t, winnerDimension, "round %d admitted no writer", round)

		var authorityDimension, rowCount, minDimension, maxDimension int
		require.NoError(t, db.QueryRow(`
			SELECT dimension FROM document_vector_backend_generations WHERE generation_id = $1`,
			int64(generationID)).Scan(&authorityDimension))
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*), MIN(dimension), MAX(dimension)
			FROM document_vector_embeddings WHERE generation_id = $1`,
			int64(generationID)).Scan(&rowCount, &minDimension, &maxDimension))
		assert.Equal(t, winnerDimension, authorityDimension)
		assert.Equal(t, 1, rowCount)
		assert.Equal(t, winnerDimension, minDimension)
		assert.Equal(t, winnerDimension, maxDimension)
	}
}

func TestDocumentBackendGenerationAuthorityRollsBackWithBatchPostgres(t *testing.T) {
	db := openPGTestDB(t)
	parent, err := Open(t.Context(), Options{DB: db})
	require.NoError(t, err)
	backend := parent.DocumentBackend()
	require.NoError(t, backend.PutUnpublished(t.Context(), 801, 3, []document.Embedding{
		{Token: "authority-owner", Vector: []float32{1, 0, 0}},
	}))

	err = backend.PutUnpublished(t.Context(), 802, 3, []document.Embedding{
		{Token: "authority-partial", Vector: []float32{0, 1, 0}},
		{Token: "authority-owner", Vector: []float32{0, 0, 1}},
	})
	require.ErrorIs(t, err, document.ErrInvalidVector)
	var generations, embeddings int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM document_vector_backend_generations WHERE generation_id = 802`).Scan(&generations))
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM document_vector_embeddings WHERE generation_id = 802`).Scan(&embeddings))
	assert.Zero(t, generations, "failed token batch rolls back generation authority")
	assert.Zero(t, embeddings, "failed token batch rolls back embeddings")

	require.NoError(t, backend.PutUnpublished(t.Context(), 802, 4, []document.Embedding{
		{Token: "authority-after-rollback", Vector: []float32{1, 0, 0, 0}},
	}), "a later dimension may establish authority after the failed transaction")
}

func TestDocumentBackendSearchPageReadsBeyondHNSWBudgetPostgres(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	db := openPGTestDB(t)
	db.SetMaxOpenConns(1)
	parent, err := Open(t.Context(), Options{DB: db})
	requirements.NoError(err)
	backend := parent.DocumentBackend()

	const total = 1105
	embeddings := make([]document.Embedding, total)
	for index := range embeddings {
		embeddings[index] = document.Embedding{
			Token: fmt.Sprintf("%064x", index), Vector: []float32{1, float32(index) / total, 0},
		}
	}
	requirements.NoError(backend.PutUnpublished(t.Context(), 901, 3, embeddings[:1000]))
	requirements.NoError(backend.PutUnpublished(t.Context(), 901, 3, embeddings[1000:]))
	_, err = db.ExecContext(t.Context(), `SET enable_seqscan = off`)
	requirements.NoError(err)
	_, err = db.ExecContext(t.Context(), `SET hnsw.ef_search = 40`)
	requirements.NoError(err)

	firstPage, err := backend.SearchPage(t.Context(), 901, 3, []float32{1, 0, 0}, "", 1000)
	requirements.NoError(err)
	requirements.False(firstPage.Exhausted)
	page, err := backend.SearchPage(t.Context(), 901, 3, []float32{1, 0, 0}, firstPage.NextCursor, 100)

	requirements.NoError(err)
	requirements.Len(page.Hits, 100)
	assertions.False(page.Exhausted)
	assertions.Equal(fmt.Sprintf("%064x", 1000), page.Hits[0].Token)
	assertions.Equal(1001, page.Hits[0].Rank)
}

func TestDocumentBackendContractPostgres(t *testing.T) {
	db := openPGTestDB(t)
	parent, err := Open(context.Background(), Options{DB: db, Dimension: 3})
	require.NoError(t, err)
	backend := parent.DocumentBackend()
	testPGDocumentBackendContract(t, backend, func(token string) int {
		t.Helper()
		var count int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM document_vector_embeddings WHERE token = $1`, token).Scan(&count))
		return count
	})

	var definition string
	require.NoError(t, db.QueryRow(`SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid = $1::regclass`,
		DocumentVectorIndexName(3)).Scan(&definition))
	assert.Contains(t, definition, "vector_cosine_ops")
	assert.Contains(t, definition, "WHERE (dimension = 3)")
}

func testPGDocumentBackendContract(t *testing.T, backend document.Backend, tokenCount func(string) int) {
	t.Helper()
	ctx := context.Background()
	gen1 := document.GenerationID(11)
	gen2 := document.GenerationID(12)
	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, nil))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, nil))
	empty, err := backend.Search(ctx, 99, 5, []float32{1, 0, 0, 0, 0}, 1)
	require.NoError(t, err)
	assert.Empty(t, empty)
	embeddings := []document.Embedding{
		{Token: "token-d", Vector: []float32{-1, 0, 0}},
		{Token: "token-c", Vector: []float32{0, -1, 0}},
		{Token: "token-a", Vector: []float32{1, 0, 0}},
		{Token: "token-b", Vector: []float32{0, 1, 0}},
	}
	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, embeddings))
	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, embeddings))
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	err = backend.PutUnpublished(canceledCtx, gen1, 3, []document.Embedding{
		{Token: "canceled-first", Vector: []float32{1, 0, 0}},
		{Token: "canceled-second", Vector: []float32{0, 1, 0}},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, tokenCount("canceled-first"))
	assert.Zero(t, tokenCount("canceled-second"), "a canceled put exposes no partial batch")

	hits, err := backend.Search(ctx, gen1, 3, []float32{1, 0, 0}, 4)
	require.NoError(t, err)
	require.Len(t, hits, 4)
	assert.Equal(t, []string{"token-a", "token-b", "token-c", "token-d"}, pgHitTokens(hits))
	assert.Equal(t, []int{1, 2, 3, 4}, pgHitRanks(hits))
	assert.InDelta(t, 1, hits[0].Score, 1e-6)
	assert.InDelta(t, 0, hits[1].Score, 1e-6)
	assert.InDelta(t, 0, hits[2].Score, 1e-6)
	assert.InDelta(t, -1, hits[3].Score, 1e-6)
	paged, ok := backend.(document.PagedBackend)
	require.True(t, ok)
	firstPage, err := paged.SearchPage(ctx, gen1, 3, []float32{1, 0, 0}, "", 2)
	require.NoError(t, err)
	assert.False(t, firstPage.Exhausted)
	assert.Equal(t, []string{"token-a", "token-b"}, pgHitTokens(firstPage.Hits))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, []string{"token-a"}))
	secondPage, err := paged.SearchPage(ctx, gen1, 3, []float32{1, 0, 0}, firstPage.NextCursor, 2)
	require.NoError(t, err)
	assert.True(t, secondPage.Exhausted)
	assert.Equal(t, []string{"token-c", "token-d"}, pgHitTokens(secondPage.Hits))
	assert.Equal(t, []int{3, 4}, pgHitRanks(secondPage.Hits))

	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-a", Vector: []float32{0, 0, 1}},
	}))
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "token-a", hits[0].Token)

	require.NoError(t, backend.PutUnpublished(ctx, gen2, 4, []document.Embedding{
		{Token: "token-four", Vector: []float32{0, 0, 0, 1}},
	}))
	hits, err = backend.Search(ctx, gen2, 4, []float32{0, 0, 0, 1}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "token-four", hits[0].Token)
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 10)
	require.NoError(t, err)
	assert.NotContains(t, pgHitTokens(hits), "token-four")

	err = backend.PutUnpublished(ctx, gen2, 4, []document.Embedding{
		{Token: "token-new", Vector: []float32{1, 0, 0, 0}},
		{Token: "token-a", Vector: []float32{0, 1, 0, 0}},
	})
	require.ErrorIs(t, err, document.ErrInvalidVector)
	assert.Zero(t, tokenCount("token-new"))

	err = backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-partial", Vector: []float32{1, 0, 0}},
		{Token: "token-wrong", Vector: []float32{1, 0}},
	})
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)
	assert.Zero(t, tokenCount("token-partial"))
	err = backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-partial-nonfinite", Vector: []float32{1, 0, 0}},
		{Token: "token-nonfinite", Vector: []float32{1, float32(math.NaN()), 0}},
	})
	require.ErrorIs(t, err, document.ErrInvalidVector)
	assert.Zero(t, tokenCount("token-partial-nonfinite"))

	for name, batch := range map[string][]document.Embedding{
		"duplicate": {
			{Token: "duplicate", Vector: []float32{1, 0, 0}},
			{Token: "duplicate", Vector: []float32{0, 1, 0}},
		},
		"empty token": {{Token: "", Vector: []float32{1, 0, 0}}},
		"long token":  {{Token: strings.Repeat("x", 1025), Vector: []float32{1, 0, 0}}},
		"nul token":   {{Token: "x\x00y", Vector: []float32{1, 0, 0}}},
		"nonfinite":   {{Token: "nan", Vector: []float32{1, float32(math.NaN()), 0}}},
		"infinite":    {{Token: "inf", Vector: []float32{1, float32(math.Inf(1)), 0}}},
		"zero norm":   {{Token: "zero", Vector: []float32{0, 0, 0}}},
	} {
		t.Run("put rejects "+name, func(t *testing.T) {
			require.ErrorIs(t, backend.PutUnpublished(ctx, gen1, 3, batch), document.ErrInvalidVector)
		})
	}

	tooMany := make([]document.Embedding, 1001)
	for i := range tooMany {
		tooMany[i] = document.Embedding{Token: "bounded-" + string(rune(i+1)), Vector: []float32{1, 0, 0}}
	}
	for name, call := range map[string]func() error{
		"put generation": func() error { return backend.PutUnpublished(ctx, 0, 3, embeddings[:1]) },
		"put dimension":  func() error { return backend.PutUnpublished(ctx, gen1, 0, embeddings[:1]) },
		"put batch":      func() error { return backend.PutUnpublished(ctx, gen1, 3, tooMany) },
		"delete gen":     func() error { return backend.DeleteTokens(ctx, 0, []string{"token-a"}) },
		"delete batch":   func() error { return backend.DeleteTokens(ctx, gen1, make([]string, 1001)) },
		"search gen": func() error {
			_, err := backend.Search(ctx, 0, 3, []float32{1, 0, 0}, 1)
			return err
		},
		"search dimension": func() error {
			_, err := backend.Search(ctx, gen1, 0, []float32{1, 0, 0}, 1)
			return err
		},
		"search k zero": func() error {
			_, err := backend.Search(ctx, gen1, 3, []float32{1, 0, 0}, 0)
			return err
		},
		"search k large": func() error {
			_, err := backend.Search(ctx, gen1, 3, []float32{1, 0, 0}, 1001)
			return err
		},
		"search nonfinite": func() error {
			_, err := backend.Search(ctx, gen1, 3, []float32{1, float32(math.Inf(-1)), 0}, 1)
			return err
		},
	} {
		t.Run("bounds reject "+name, func(t *testing.T) {
			require.ErrorIs(t, call(), document.ErrInvalidVector)
		})
	}
	_, err = backend.Search(ctx, gen1, 3, []float32{1, 0}, 1)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)

	require.NoError(t, backend.DeleteTokens(ctx, gen2, []string{"token-a"}))
	assert.Equal(t, 1, tokenCount("token-a"))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, []string{"token-a"}))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, []string{"token-a"}))
	assert.Zero(t, tokenCount("token-a"))
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 10)
	require.NoError(t, err)
	assert.NotContains(t, pgHitTokens(hits), "token-a")
}

func pgHitTokens(hits []document.Hit) []string {
	out := make([]string, len(hits))
	for i := range hits {
		out[i] = hits[i].Token
	}
	return out
}

func pgHitRanks(hits []document.Hit) []int {
	out := make([]int, len(hits))
	for i := range hits {
		out[i] = hits[i].Rank
	}
	return out
}
