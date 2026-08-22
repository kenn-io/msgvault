//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
)

func openDocumentBackendForTest(t *testing.T) (*Backend, *DocumentBackend) {
	t.Helper()
	b, err := Open(context.Background(), Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 3,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b, b.DocumentBackend()
}

func TestDocumentBackendContractSQLite(t *testing.T) {
	parent, backend := openDocumentBackendForTest(t)
	testDocumentBackendContract(t, backend, func(token string) int {
		t.Helper()
		var count int
		require.NoError(t, parent.db.QueryRow(
			`SELECT COUNT(*) FROM document_vector_embeddings WHERE token = ?`, token).Scan(&count))
		return count
	})
	require.NoError(t, backend.PutUnpublished(t.Context(), 21, 3, []document.Embedding{
		{Token: "stable-rowid", Vector: []float32{1, 0, 0}},
	}))
	var before, after int64
	require.NoError(t, parent.db.QueryRow(
		`SELECT document_vector_id FROM document_vector_embeddings WHERE token = ?`, "stable-rowid").Scan(&before))
	require.NoError(t, backend.PutUnpublished(t.Context(), 21, 3, []document.Embedding{
		{Token: "stable-rowid", Vector: []float32{0, 1, 0}},
	}))
	require.NoError(t, parent.db.QueryRow(
		`SELECT document_vector_id FROM document_vector_embeddings WHERE token = ?`, "stable-rowid").Scan(&after))
	assert.Equal(t, before, after, "replacement preserves the vec0-linked metadata rowid")
}

func TestDocumentVectorSearchProbeLimitStaysBounded(t *testing.T) {
	assert.Equal(t, 10, documentVectorSearchProbeLimit(10, 1))
	assert.Equal(t, 33, documentVectorSearchProbeLimit(10_000, 1))
	assert.Equal(t, 200, documentVectorSearchProbeLimit(10_000, 100))
	assert.Equal(t, 2_000, documentVectorSearchProbeLimit(1_000_000, 1_000))
}

func testDocumentBackendContract(t *testing.T, backend document.Backend, tokenCount func(string) int) {
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
	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, embeddings), "same batch is idempotent")
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
	assert.Equal(t, []string{"token-a", "token-b", "token-c", "token-d"}, hitTokens(hits))
	assert.Equal(t, []int{1, 2, 3, 4}, hitRanks(hits))
	assert.InDelta(t, 1, hits[0].Score, 1e-6)
	assert.InDelta(t, 0, hits[1].Score, 1e-6)
	assert.InDelta(t, 0, hits[2].Score, 1e-6)
	assert.InDelta(t, -1, hits[3].Score, 1e-6)

	require.NoError(t, backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-a", Vector: []float32{0, 0, 1}},
	}))
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "token-a", hits[0].Token)
	assert.InDelta(t, 1, hits[0].Score, 1e-6)

	require.NoError(t, backend.PutUnpublished(ctx, gen2, 4, []document.Embedding{
		{Token: "token-four", Vector: []float32{0, 0, 0, 1}},
	}))
	hits, err = backend.Search(ctx, gen2, 4, []float32{0, 0, 0, 1}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "token-four", hits[0].Token)
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 10)
	require.NoError(t, err)
	assert.NotContains(t, hitTokens(hits), "token-four")

	err = backend.PutUnpublished(ctx, gen2, 4, []document.Embedding{
		{Token: "token-new", Vector: []float32{1, 0, 0, 0}},
		{Token: "token-a", Vector: []float32{0, 1, 0, 0}},
	})
	require.ErrorIs(t, err, document.ErrInvalidVector)
	assert.Zero(t, tokenCount("token-new"), "generation collision rolls back the whole batch")

	err = backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-partial", Vector: []float32{1, 0, 0}},
		{Token: "token-wrong", Vector: []float32{1, 0}},
	})
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)
	assert.Zero(t, tokenCount("token-partial"), "dimension rejection happens before any write")
	err = backend.PutUnpublished(ctx, gen1, 3, []document.Embedding{
		{Token: "token-partial-nonfinite", Vector: []float32{1, 0, 0}},
		{Token: "token-nonfinite", Vector: []float32{1, float32(math.NaN()), 0}},
	})
	require.ErrorIs(t, err, document.ErrInvalidVector)
	assert.Zero(t, tokenCount("token-partial-nonfinite"), "nonfinite rejection happens before any write")

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
			err := backend.PutUnpublished(ctx, gen1, 3, batch)
			require.ErrorIs(t, err, document.ErrInvalidVector)
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

	require.NoError(t, backend.DeleteTokens(ctx, gen2, []string{"token-a"}), "wrong generation is an idempotent no-op")
	assert.Equal(t, 1, tokenCount("token-a"))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, []string{"token-a"}))
	require.NoError(t, backend.DeleteTokens(ctx, gen1, []string{"token-a"}), "repeat delete is idempotent")
	assert.Zero(t, tokenCount("token-a"))
	hits, err = backend.Search(ctx, gen1, 3, []float32{0, 0, 1}, 10)
	require.NoError(t, err)
	assert.NotContains(t, hitTokens(hits), "token-a")
}

func hitTokens(hits []document.Hit) []string {
	out := make([]string, len(hits))
	for i := range hits {
		out[i] = hits[i].Token
	}
	return out
}

func hitRanks(hits []document.Hit) []int {
	out := make([]int, len(hits))
	for i := range hits {
		out[i] = hits[i].Rank
	}
	return out
}

func TestDocumentVectorMigrationSQLite(t *testing.T) {
	parent, _ := openDocumentBackendForTest(t)
	var tableSQL string
	require.NoError(t, parent.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'document_vector_embeddings'`).Scan(&tableSQL))
	assert.Contains(t, tableSQL, "token")
	assert.NotContains(t, tableSQL, "REFERENCES index_generations")

	require.NoError(t, EnsureDocumentVectorTable(context.Background(), parent.db, 4))
	require.NoError(t, EnsureDocumentVectorTable(context.Background(), parent.db, 4), "lazy table migration is idempotent")
	require.NoError(t, parent.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, DocumentVectorTableName(4)).Scan(&tableSQL))
	assert.Contains(t, tableSQL, "distance_metric=cosine")
}
