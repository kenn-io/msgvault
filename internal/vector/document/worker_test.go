package document

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbankdocument "go.kenn.io/docbank/document"
	docembedding "go.kenn.io/docbank/document/embedding"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/embed"
)

var _ Provider = (embed.SemanticClient)(nil)

func TestPrepareDocbankClaimInputsUsesValidatedRawPlan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	policy, err := docbankdocument.NewNormalizePolicy(10_000)
	require.NoError(err)
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{
		Family: "pdf", UnitKind: "page", Units: []docbankdocument.SourceUnit{{
			Index: 0, Markdown: "# Findings\n\nImportant evidence",
		}},
	}, policy)
	require.NoError(err)
	recipe, err := docembedding.NewRecipe(docembedding.RecipeConfig{
		Mode: docembedding.RepresentationRaw, MaxInputRunes: 1000,
	})
	require.NoError(err)
	claim := workerClaim("extract-a", 1, normalized.Chunks[0].Text, strings.Repeat("a", 64))
	claim.ChunkKey = normalized.Chunks[0].Key
	claim.ChunkChecksum = normalized.Chunks[0].Checksum
	ledger := &normalizedFakeDocumentVectorLedger{
		fakeDocumentVectorLedger: newFakeDocumentVectorLedger(claim), normalized: normalized,
	}

	inputs, err := prepareDocbankClaimInputs(t.Context(), ledger, recipe, []*store.DocumentVectorChunkClaim{claim})

	require.NoError(err)
	assert.Contains(inputs[claim.Token], "Heading: Findings")
	assert.Contains(inputs[claim.Token], "Source: page 1")
	assert.Contains(inputs[claim.Token], "Content:\n# Findings\nImportant evidence")
}

type normalizedFakeDocumentVectorLedger struct {
	*fakeDocumentVectorLedger

	normalized docbankdocument.NormalizedDocument
}

func (l *normalizedFakeDocumentVectorLedger) LoadNormalizedDocument(context.Context, string) (docbankdocument.NormalizedDocument, error) {
	return l.normalized, nil
}

func TestWorkerRunRejectsInvalidLimit(t *testing.T) {
	worker := NewWorker(WorkerDeps{})

	for _, limit := range []int{0, 1001} {
		_, err := worker.Run(t.Context(), 1, limit)
		require.ErrorContains(t, err, "limit")
	}
}

func TestWorkerRunRejectsInvalidGenerationAndDimension(t *testing.T) {
	ledger := newFakeDocumentVectorLedger()
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{}, &fakeDocumentVectorBackend{})

	_, err := worker.Run(t.Context(), 0, 1)
	require.ErrorContains(t, err, "generation")

	ledger.generation.State = store.DocumentVectorGenerationActive
	_, err = worker.Run(t.Context(), 1, 1)
	require.ErrorIs(t, err, store.ErrDocumentVectorInvalidGenerationState)

	ledger.generation.State = store.DocumentVectorGenerationBuilding
	ledger.generation.Dimension = 4
	_, err = worker.Run(t.Context(), 1, 1)
	require.ErrorContains(t, err, "dimension")
}

func TestWorkerRunRejectsUnboundedRetryAndIncompleteRestoredCursor(t *testing.T) {
	ledger := newFakeDocumentVectorLedger()
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{}, &fakeDocumentVectorBackend{})
	worker.deps.RetryDelay = 7*24*time.Hour + time.Millisecond

	_, err := worker.Run(t.Context(), 1, 1)
	require.ErrorContains(t, err, "policy")

	worker = newFakeWorker(ledger, &fakeDocumentVectorProvider{}, &fakeDocumentVectorBackend{})
	worker.deps.AfterChunkID = 12
	worker.afterChunkID = 12
	_, err = worker.Run(t.Context(), 1, 1)
	require.ErrorContains(t, err, "policy")
}

func TestWorkerRunPreservesExactTextAndExtractionBoundaries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "  alpha\n", "token-a"),
		workerClaim("extract-a", 2, "beta", "token-b"),
		workerClaim("extract-b", 3, "γamma", "token-c"),
	)
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{
		{{1, 2, 3}, {4, 5, 6}},
		{{7, 8, 9}},
	}}
	provider.call = func(_ context.Context, _ []embed.DocumentInput) ([][][]float32, error) {
		assert.Equal(t, 3, ledger.claimCalls, "all provider inputs must be claimed first")
		return provider.vectors, nil
	}
	backend := &fakeDocumentVectorBackend{}
	ledger.beforeCommit = func(testedToken string) {
		assert.NotEmpty(t, backend.puts, "backend put must precede fenced commit")
		assert.Contains(t, embeddingTokens(backend.puts[0]), testedToken)
	}
	worker := newFakeWorker(ledger, provider, backend)

	result, err := worker.Run(t.Context(), 1, 4)
	requirements.NoError(err)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{
		{Chunks: []string{"  alpha\n", "beta"}},
		{Chunks: []string{"γamma"}},
	}, provider.calls[0])
	assertions.Equal(3, result.Claimed)
	assertions.Equal(3, result.Embedded)
	assertions.Equal(3, result.Published)
	assertions.Equal(3, result.Attempted)
	assertions.Equal(3, result.Succeeded)
	assertions.Zero(result.Failed)
	assertions.Equal(1, result.ProviderCalls)
	assertions.Equal(2, result.ProviderDocuments)
	assertions.Equal(3, result.ProviderChunks)
	assertions.Equal(utf8.RuneCountInString("  alpha\n")+utf8.RuneCountInString("beta")+utf8.RuneCountInString("γamma"),
		result.ProviderInputChars,
	)
	assertions.True(result.Exhausted)
	assertions.Zero(result.AfterGenerationID)
	assertions.Zero(result.AfterChunkID)
	requirements.Len(backend.puts, 1)
	assertions.Equal([]string{"token-a", "token-b", "token-c"}, embeddingTokens(backend.puts[0]))
	assertions.Equal([]string{"token-a", "token-b", "token-c"}, ledger.committed)
}

func TestWorkerRunCapsProviderInputOnRuneBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "αβγδε", "token-a"),
	)
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}}}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})
	worker.deps.MaxInputChars = 3
	recipe, err := docembedding.NewRecipe(docembedding.RecipeConfig{
		Mode: docembedding.RepresentationRaw, MaxInputRunes: 3,
	})
	requirements.NoError(err)
	worker.deps.Recipe = recipe

	result, err := worker.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{{Chunks: []string{"αβγ"}}}, provider.calls[0])
	assertions.Equal(3, result.ProviderInputChars)
}

func TestWorkerRunNeverClaimsMoreThanLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "one", "token-a"),
		workerClaim("extract-a", 2, "two", "token-b"),
		workerClaim("extract-a", 3, "three", "token-c"),
	)
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}, {4, 5, 6}}}}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})

	result, err := worker.Run(t.Context(), 1, 2)
	requirements.NoError(err)
	assertions.Equal(2, result.Claimed)
	assertions.False(result.Exhausted)
	assertions.Equal(2, ledger.claimCalls)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{{Chunks: []string{"one", "two"}}}, provider.calls[0])
}

func TestWorkerRunContextualBoundariesDoNotDependOnLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	claims := func() []*store.DocumentVectorChunkClaim {
		return []*store.DocumentVectorChunkClaim{
			workerClaim("extract-a", 1, "one", "token-a"),
			workerClaim("extract-a", 2, "two", "token-b"),
			workerClaim("extract-a", 3, "three", "token-c"),
		}
	}
	wantInputs := []embed.DocumentInput{
		{Chunks: []string{"one"}}, {Chunks: []string{"two"}}, {Chunks: []string{"three"}},
	}

	boundedProvider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}, {{4, 5, 6}}}}
	boundedWorker := newFakeWorker(newFakeDocumentVectorLedger(claims()...), boundedProvider, &fakeDocumentVectorBackend{})
	boundedWorker.deps.ContextualDocuments = true
	first, err := boundedWorker.Run(t.Context(), 1, 2)
	requirements.NoError(err)
	requirements.False(first.Exhausted)
	boundedProvider.vectors = [][][]float32{{{7, 8, 9}}}
	second, err := boundedWorker.Run(t.Context(), 1, 2)
	requirements.NoError(err)
	requirements.True(second.Exhausted)

	unboundedProvider := &fakeDocumentVectorProvider{vectors: [][][]float32{
		{{1, 2, 3}}, {{4, 5, 6}}, {{7, 8, 9}},
	}}
	unboundedWorker := newFakeWorker(newFakeDocumentVectorLedger(claims()...), unboundedProvider, &fakeDocumentVectorBackend{})
	unboundedWorker.deps.ContextualDocuments = true
	_, err = unboundedWorker.Run(t.Context(), 1, 3)
	requirements.NoError(err)

	assertions.Equal(wantInputs, append(boundedProvider.calls[0], boundedProvider.calls[1]...))
	assertions.Equal(wantInputs, unboundedProvider.calls[0])
}

func TestWorkerRunBoundsScanningAndCarriesCursorWithoutStarvation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	claims := make([]*store.DocumentVectorChunkClaim, 1500)
	for index := range claims {
		claims[index] = workerClaim(
			"extract-a", int64(index+1), fmt.Sprintf("chunk-%d", index+1), fmt.Sprintf("token-%d", index+1),
		)
	}
	ledger := newFakeDocumentVectorLedger(claims...)
	ledger.unclaimableThrough = 1499
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}}}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})

	first, err := worker.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Zero(first.Claimed)
	assertions.Equal(int64(1000), first.AfterChunkID)
	assertions.False(first.Exhausted)
	assertions.Equal(1000, ledger.claimCalls)
	assertions.Empty(provider.calls)

	second, err := worker.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Equal(1, second.Claimed)
	assertions.Equal(1, second.Published)
	assertions.True(second.Exhausted)
	assertions.Zero(second.AfterGenerationID)
	assertions.Zero(second.AfterChunkID)
	assertions.Equal(1500, ledger.claimCalls)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{{Chunks: []string{"chunk-1500"}}}, provider.calls[0])
}

func TestWorkerRunExhaustedCursorRoundTripsAsReset(t *testing.T) {
	tests := []struct {
		name           string
		claims         []*store.DocumentVectorChunkClaim
		unclaimable    int64
		wantClaimCalls int
	}{
		{name: "empty corpus"},
		{
			name:           "nonempty tail",
			claims:         []*store.DocumentVectorChunkClaim{workerClaim("extract-a", 1, "leased", "token-a")},
			unclaimable:    1,
			wantClaimCalls: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			ledger := newFakeDocumentVectorLedger(test.claims...)
			ledger.unclaimableThrough = test.unclaimable
			worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{}, &fakeDocumentVectorBackend{})

			first, err := worker.Run(t.Context(), 1, 1)
			requirements.NoError(err)
			requirements.True(first.Exhausted)

			resumeDeps := worker.deps
			resumeDeps.AfterGenerationID = first.AfterGenerationID
			resumeDeps.AfterChunkID = first.AfterChunkID
			resumed := NewWorker(resumeDeps)
			second, err := resumed.Run(t.Context(), 1, 1)
			requirements.NoError(err)
			assertions.True(second.Exhausted)
			assertions.Zero(first.AfterGenerationID)
			assertions.Zero(first.AfterChunkID)
			assertions.Zero(second.AfterGenerationID)
			assertions.Zero(second.AfterChunkID)
			assertions.Equal(test.wantClaimCalls, ledger.claimCalls)
		})
	}
}

func TestWorkerRunResetsInMemoryCursorWhenGenerationChanges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	claims := make([]*store.DocumentVectorChunkClaim, 1000)
	for index := range claims {
		claims[index] = workerClaim("extract-a", int64(index+1), "old", fmt.Sprintf("old-%d", index+1))
	}
	ledger := newFakeDocumentVectorLedger(claims...)
	ledger.unclaimableThrough = 1000
	provider := &fakeDocumentVectorProvider{}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})

	first, err := worker.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Equal(GenerationID(1), first.AfterGenerationID)
	assertions.Equal(int64(1000), first.AfterChunkID)

	ledger.generation.ID = 2
	ledger.unclaimableThrough = 0
	newClaim := workerClaim("extract-b", 1, "new generation", "new-token")
	newClaim.GenerationID = 2
	nextClaim := workerClaim("extract-b", 2, "next", "next-token")
	nextClaim.GenerationID = 2
	ledger.claims = []*store.DocumentVectorChunkClaim{newClaim, nextClaim}
	provider.vectors = [][][]float32{{{1, 2, 3}}}
	second, err := worker.Run(t.Context(), 2, 1)
	requirements.NoError(err)
	assertions.Equal(1, second.Published)
	assertions.Equal(GenerationID(2), second.AfterGenerationID)
	assertions.Equal(int64(1), second.AfterChunkID)
	assertions.False(second.Exhausted)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{{Chunks: []string{"new generation"}}}, provider.calls[0])
}

func TestWorkerRunPublishesCompletedProviderPrefixAndRetriesOnlySuffix(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	providerErr := errors.New("provider unavailable")
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
	)
	provider := &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}},
		err:     providerErr,
	}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, providerErr)
	assertions.Equal(1, result.Embedded)
	assertions.Equal(1, result.Published)
	assertions.Equal(1, result.Retry)
	assertions.Zero(result.Terminal)
	assertions.Equal(2, result.Attempted)
	assertions.Equal(1, result.Succeeded)
	assertions.Equal(1, result.Failed,
		"a retryable transition is one final chunk failure, not a retry-attempt count")
	assertions.Equal([]string{"token-a"}, ledger.committed)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("token-b", ledger.failures[0].token)
	assertions.False(ledger.failures[0].terminal)
	assertions.Equal("provider_transient", ledger.failures[0].errorCode)
	requirements.NotNil(ledger.failures[0].retryAt)
	assertions.Equal(workerNow.Add(time.Minute), *ledger.failures[0].retryAt)
}

func TestWorkerRunMakesPermanentAndMalformedProviderFailuresTerminal(t *testing.T) {
	tests := []struct {
		name     string
		vectors  [][][]float32
		err      error
		wantCode string
	}{
		{
			name:     "permanent provider rejection",
			err:      fmt.Errorf("request rejected: %w", embed.ErrPermanent4xx),
			wantCode: "provider_rejected",
		},
		{
			name:     "document cardinality",
			vectors:  [][][]float32{{{1, 2, 3}}, {{4, 5, 6}}},
			wantCode: "invalid_provider_shape",
		},
		{
			name:     "chunk cardinality",
			vectors:  [][][]float32{{{1, 2, 3}}},
			wantCode: "invalid_provider_shape",
		},
		{
			name:     "vector dimension",
			vectors:  [][][]float32{{{1, 2}, {4, 5, 6}}},
			wantCode: "invalid_provider_vector",
		},
		{
			name:     "nonfinite vector",
			vectors:  [][][]float32{{{1, float32(math.NaN()), 3}, {4, 5, 6}}},
			wantCode: "invalid_provider_vector",
		},
		{
			name:     "zero norm vector",
			vectors:  [][][]float32{{{0, 0, 0}, {4, 5, 6}}},
			wantCode: "invalid_provider_vector",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			ledger := newFakeDocumentVectorLedger(
				workerClaim("extract-a", 1, "first", "token-a"),
				workerClaim("extract-a", 2, "second", "token-b"),
			)
			worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
				vectors: test.vectors, err: test.err,
			}, &fakeDocumentVectorBackend{})

			result, err := worker.Run(t.Context(), 1, 10)
			requirements.Error(err)
			assertions.Zero(result.Published)
			assertions.Equal(2, result.Terminal)
			requirements.Len(ledger.failures, 2)
			assertions.Equal(test.wantCode, ledger.failures[0].errorCode)
			assertions.Equal(test.wantCode, ledger.failures[1].errorCode)
			assertions.True(ledger.failures[0].terminal)
			assertions.Nil(ledger.failures[0].retryAt)
		})
	}
}

func TestWorkerRunMakesMalformedHTTPProviderResponsesTerminal(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantCode string
	}{
		{
			name:     "response shape",
			response: `{"data":[]}`,
			wantCode: "invalid_provider_shape",
		},
		{
			name:     "vector dimension",
			response: `{"data":[{"embedding":[1,2],"index":0}]}`,
			wantCode: "invalid_provider_vector",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(test.response))
				assertions.NoError(err)
			}))
			t.Cleanup(server.Close)
			ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
			client := embed.NewClient(embed.Config{
				Endpoint: server.URL, Model: "embed-test", Dimension: 3, MaxRetries: 1,
			})
			worker := newFakeWorker(ledger, client, &fakeDocumentVectorBackend{})

			result, err := worker.Run(t.Context(), 1, 1)

			requirements.Error(err)
			assertions.Equal(1, result.Terminal)
			assertions.Zero(result.Retry)
			requirements.Len(ledger.failures, 1)
			assertions.True(ledger.failures[0].terminal)
			assertions.Equal(test.wantCode, ledger.failures[0].errorCode)
		})
	}
}

func TestWorkerRunIsolatesMalformedProviderDocumentFromHealthySuffix(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
		workerClaim("extract-c", 3, "third", "token-c"),
	)
	backend := &fakeDocumentVectorBackend{}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{vectors: [][][]float32{
		{{1, 2, 3}}, {{4, 5}}, {{7, 8, 9}},
	}}, backend)

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, errInvalidProviderVector)
	assertions.Equal(2, result.Embedded)
	assertions.Equal(2, result.Published)
	assertions.Equal(1, result.Terminal)
	assertions.Equal([]string{"token-a", "token-c"}, ledger.committed)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("token-b", ledger.failures[0].token)
	assertions.Equal("invalid_provider_vector", ledger.failures[0].errorCode)
	requirements.Len(backend.puts, 1)
	assertions.Equal([]string{"token-a", "token-c"}, embeddingTokens(backend.puts[0]))
}

func TestWorkerRunRecordsPreparationFailureAndPublishesHealthyExtraction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
	)
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}}}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})
	worker.deps.prepareInputs = func(_ context.Context, _ Ledger, _ docembedding.Recipe, claims []*store.DocumentVectorChunkClaim) (map[string]string, error) {
		if claims[0].ExtractionID == "extract-b" {
			return nil, errors.New("normalized document is corrupt")
		}
		return map[string]string{claims[0].Token: claims[0].Text}, nil
	}

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, errInputPreparation)
	assertions.Equal(1, result.Published)
	assertions.Equal(1, result.Retry)
	assertions.Zero(result.Terminal)
	assertions.Equal([]string{"token-a"}, ledger.committed)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("token-b", ledger.failures[0].token)
	assertions.Equal("input_preparation", ledger.failures[0].errorCode)
	requirements.Len(provider.calls, 1)
	assertions.Equal([]embed.DocumentInput{{Chunks: []string{"first"}}}, provider.calls[0])
}

func TestWorkerRunPreparationFailureHonorsAttemptCeiling(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	claim := workerClaim("extract-a", 1, "first", "token-a")
	claim.AttemptCount = 3
	ledger := newFakeDocumentVectorLedger(claim)
	provider := &fakeDocumentVectorProvider{}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})
	worker.deps.prepareInputs = func(context.Context, Ledger, docembedding.Recipe, []*store.DocumentVectorChunkClaim) (map[string]string, error) {
		return nil, errors.New("normalized document is corrupt")
	}

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, errInputPreparation)
	assertions.Zero(result.Published)
	assertions.Zero(result.Retry)
	assertions.Equal(1, result.Terminal)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("attempt_limit", ledger.failures[0].errorCode)
	assertions.Empty(provider.calls)
}

func TestWorkerRunAttemptCeilingMakesTransientFailureTerminal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	claim := workerClaim("extract-a", 1, "first", "token-a")
	claim.AttemptCount = 3
	ledger := newFakeDocumentVectorLedger(claim)
	providerErr := errors.New("provider unavailable")
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{err: providerErr}, &fakeDocumentVectorBackend{})

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, providerErr)
	assertions.Zero(result.Retry)
	assertions.Equal(1, result.Terminal)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("attempt_limit", ledger.failures[0].errorCode)
	assertions.True(ledger.failures[0].terminal)
}

func TestWorkerRunDeletesOnlySourceChangedTokens(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-source"),
		workerClaim("extract-b", 2, "second", "token-lost"),
	)
	ledger.commitErr["token-source"] = store.ErrDocumentVectorSourceChanged
	ledger.commitErr["token-lost"] = store.ErrDocumentVectorClaimLost
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{
		{{1, 2, 3}}, {{4, 5, 6}},
	}}
	backend := &fakeDocumentVectorBackend{}
	worker := newFakeWorker(ledger, provider, backend)

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, store.ErrDocumentVectorClaimLost)
	assertions.Zero(result.Published)
	assertions.Equal(1, result.SourceChanged)
	assertions.Equal(1, result.Attempted)
	assertions.Zero(result.Succeeded)
	assertions.Equal(1, result.Failed,
		"the claim-lost chunk had no durable final decision and must not inflate attempted")
	assertions.Equal([][]string{{"token-source"}}, backend.deletes)
	assertions.NotContains(backend.deletes[0], "token-lost")
}

func TestWorkerRunContextInterruptionLeavesClaimsLeasedForTakeover(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	started := make(chan struct{})
	provider := &fakeDocumentVectorProvider{call: func(ctx context.Context, _ []embed.DocumentInput) ([][][]float32, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	backend := &fakeDocumentVectorBackend{}
	worker := newFakeWorker(ledger, provider, backend)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := worker.Run(ctx, 1, 1)
		done <- err
	}()
	<-started
	cancel()
	requirements.ErrorIs(<-done, context.Canceled)
	assertions.Empty(ledger.failures)
	assertions.Empty(ledger.committed)
	assertions.Empty(backend.puts)

	takeover := workerClaim("extract-a", 1, "first", "token-a")
	takeover.LeaseOwner = "worker-b"
	takeover.LeaseFence = 2
	takeover.AttemptCount = 2
	ledger.claims = append(ledger.claims, takeover)
	resumeProvider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}}}
	resumer := newFakeWorker(ledger, resumeProvider, backend)
	resumer.deps.Owner = "worker-b"
	result, err := resumer.Run(t.Context(), 1, 1)
	requirements.NoError(err)
	assertions.Equal(1, result.Published)
	assertions.Equal([]string{"token-a"}, ledger.committed)
}

func TestWorkerRunReplayDoesNotRepublishReadyWork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	provider := &fakeDocumentVectorProvider{vectors: [][][]float32{{{1, 2, 3}}}}
	backend := &fakeDocumentVectorBackend{}
	worker := newFakeWorker(ledger, provider, backend)

	first, err := worker.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	second, err := worker.Run(t.Context(), 1, 10)
	requirements.NoError(err)
	assertions.Equal(1, first.Published)
	assertions.Zero(second.Claimed)
	assertions.True(second.Exhausted)
	assertions.Len(provider.calls, 1)
	assertions.Len(backend.puts, 1)
	assertions.Equal([]string{"token-a"}, ledger.committed)
}

func TestWorkerRunRetriesBackendPutFailureWithoutCommitting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	backendErr := errors.New("backend unavailable")
	backend := &fakeDocumentVectorBackend{putErr: backendErr}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}},
	}, backend)

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, backendErr)
	assertions.Equal(1, result.Embedded)
	assertions.Zero(result.Published)
	assertions.Equal(1, result.Retry)
	assertions.Empty(ledger.committed)
	requirements.Len(ledger.failures, 1)
	assertions.Equal("backend_transient", ledger.failures[0].errorCode)
}

func TestWorkerRunClassifiesProviderSuffixWhenPrefixBackendPutFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
	)
	providerErr := errors.New("provider unavailable")
	backendErr := errors.New("backend unavailable")
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}}, err: providerErr,
	}, &fakeDocumentVectorBackend{putErr: backendErr})

	result, err := worker.Run(t.Context(), 1, 10)
	requirements.ErrorIs(err, providerErr)
	requirements.ErrorIs(err, backendErr)
	assertions.Equal(2, result.Retry)
	requirements.Len(ledger.failures, 2)
	assertions.Equal("token-a", ledger.failures[0].token)
	assertions.Equal("backend_transient", ledger.failures[0].errorCode)
	assertions.Equal("token-b", ledger.failures[1].token)
	assertions.Equal("provider_transient", ledger.failures[1].errorCode)
}

func TestWorkerRunRenewsClaimsDuringLongProviderCall(t *testing.T) {
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	ledger.renewed = make(chan struct{})
	provider := &fakeDocumentVectorProvider{call: func(ctx context.Context, _ []embed.DocumentInput) ([][][]float32, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ledger.renewed:
			return [][][]float32{{{1, 2, 3}}}, nil
		case <-time.After(100 * time.Millisecond):
			return nil, errors.New("heartbeat did not renew claim")
		}
	}}
	worker := newFakeWorker(ledger, provider, &fakeDocumentVectorBackend{})
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Published)
	assert.Positive(t, ledger.renewCallCount())
}

func TestWorkerRunRenewsClaimsDuringLongBackendPut(t *testing.T) {
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	ledger.renewed = make(chan struct{})
	backend := &fakeDocumentVectorBackend{put: func(ctx context.Context, _ []Embedding) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ledger.renewed:
			return nil
		case <-time.After(100 * time.Millisecond):
			return errors.New("heartbeat did not renew during backend put")
		}
	}}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}},
	}, backend)
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Published)
	assert.Positive(t, ledger.renewCallCount())
}

func TestWorkerRunCancelsBackendPutWhenHeartbeatLosesClaim(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	ledger.renewErr = store.ErrDocumentVectorClaimLost
	backendCanceled := make(chan struct{})
	backend := &fakeDocumentVectorBackend{put: func(ctx context.Context, _ []Embedding) error {
		select {
		case <-ctx.Done():
			close(backendCanceled)
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return errors.New("heartbeat did not cancel backend put")
		}
	}}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}},
	}, backend)
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 1)
	requirements.ErrorIs(err, store.ErrDocumentVectorClaimLost)
	assertions.Zero(result.Published)
	assertions.Zero(result.Retry)
	assertions.Zero(result.Terminal)
	assertions.Empty(ledger.committed)
	assertions.Empty(ledger.failures)
	assertions.Empty(backend.deletes)
	select {
	case <-backendCanceled:
	default:
		requirements.Fail("backend context was not canceled after renewal loss")
	}
}

func TestWorkerRunStopsRenewingClaimBeforeCommitAfterSynchronousRenewal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
	ledger.beforeCommit = func(string) {
		close(commitStarted)
		<-releaseCommit
	}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{
		vectors: [][][]float32{{{1, 2, 3}}},
	}, &fakeDocumentVectorBackend{})
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond
	type outcome struct {
		result RunResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := worker.Run(t.Context(), 1, 1)
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-commitStarted:
	case <-time.After(time.Second):
		requirements.FailNow("worker did not reach commit")
	}
	before := ledger.renewTokenCallCount("token-a")
	time.Sleep(30 * time.Millisecond)
	after := ledger.renewTokenCallCount("token-a")
	close(releaseCommit)
	completed := <-done
	time.Sleep(30 * time.Millisecond)
	afterStop := ledger.renewTokenCallCount("token-a")

	requirements.NoError(completed.err)
	assertions.Equal(1, completed.result.Published)
	assertions.Equal(1, before, "claim is synchronously renewed before commit")
	assertions.Equal(before, after, "heartbeat no longer owns a claim once commit begins")
	assertions.Equal(after, afterStop, "worker shutdown leaves no heartbeat goroutine renewing claims")
}

func TestWorkerRunCancelsCommitWhenAnotherClaimLosesHeartbeat(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
	)
	ledger.renewErrByToken["token-b"] = store.ErrDocumentVectorClaimLost
	commitCanceled := make(chan struct{})
	ledger.commit = func(ctx context.Context, token string) error {
		if token != "token-a" {
			return nil
		}
		select {
		case <-ctx.Done():
			close(commitCanceled)
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return errors.New("heartbeat loss did not cancel commit")
		}
	}
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{vectors: [][][]float32{
		{{1, 2, 3}}, {{4, 5, 6}},
	}}, &fakeDocumentVectorBackend{})
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 2)
	requirements.ErrorIs(err, store.ErrDocumentVectorClaimLost)
	assertions.Zero(result.Published)
	assertions.Empty(ledger.committed)
	assertions.Empty(ledger.failures)
	select {
	case <-commitCanceled:
	default:
		requirements.Fail("commit context was not canceled after another renewal loss")
	}
}

func TestWorkerRunCancelsFailureTransitionWhenAnotherClaimLosesHeartbeat(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(
		workerClaim("extract-a", 1, "first", "token-a"),
		workerClaim("extract-b", 2, "second", "token-b"),
	)
	ledger.renewErrByToken["token-b"] = store.ErrDocumentVectorClaimLost
	failureCanceled := make(chan struct{})
	ledger.fail = func(ctx context.Context, token string) error {
		if token != "token-a" {
			return nil
		}
		select {
		case <-ctx.Done():
			close(failureCanceled)
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return errors.New("heartbeat loss did not cancel failure transition")
		}
	}
	providerErr := errors.New("provider unavailable")
	worker := newFakeWorker(ledger, &fakeDocumentVectorProvider{err: providerErr}, &fakeDocumentVectorBackend{})
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 2)
	requirements.ErrorIs(err, store.ErrDocumentVectorClaimLost)
	requirements.ErrorIs(err, providerErr)
	assertions.Zero(result.Retry)
	assertions.Zero(result.Terminal)
	assertions.Empty(ledger.committed)
	assertions.Empty(ledger.failures)
	select {
	case <-failureCanceled:
	default:
		requirements.Fail("failure context was not canceled after another renewal loss")
	}
}

func TestWorkerRunCancelsPublicationWhenHeartbeatLosesClaim(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ledger := newFakeDocumentVectorLedger(workerClaim("extract-a", 1, "first", "token-a"))
	ledger.renewErr = store.ErrDocumentVectorClaimLost
	providerCanceled := make(chan struct{})
	provider := &fakeDocumentVectorProvider{call: func(ctx context.Context, _ []embed.DocumentInput) ([][][]float32, error) {
		select {
		case <-ctx.Done():
			close(providerCanceled)
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil, errors.New("heartbeat did not cancel provider")
		}
	}}
	backend := &fakeDocumentVectorBackend{}
	worker := newFakeWorker(ledger, provider, backend)
	worker.deps.LeaseDuration = 100 * time.Millisecond
	worker.deps.HeartbeatInterval = 10 * time.Millisecond

	result, err := worker.Run(t.Context(), 1, 1)
	requirements.ErrorIs(err, store.ErrDocumentVectorClaimLost)
	assertions.Zero(result.Embedded)
	assertions.Empty(backend.puts)
	assertions.Empty(backend.deletes)
	assertions.Empty(ledger.committed)
	assertions.Empty(ledger.failures)
	select {
	case <-providerCanceled:
	default:
		requirements.Fail("provider context was not canceled after renewal loss")
	}
}

type fakeDocumentVectorLedger struct {
	mu                 sync.Mutex
	generation         store.DocumentVectorGeneration
	claims             []*store.DocumentVectorChunkClaim
	claimCalls         int
	committed          []string
	failures           []fakeDocumentVectorFailure
	commitErr          map[string]error
	commit             func(context.Context, string) error
	fail               func(context.Context, string) error
	beforeCommit       func(string)
	renewErr           error
	renewErrByToken    map[string]error
	renewCalls         int
	renewTokens        map[string]int
	renewed            chan struct{}
	renewOnce          sync.Once
	unclaimableThrough int64
}

type fakeDocumentVectorFailure struct {
	token     string
	retryAt   *time.Time
	terminal  bool
	errorCode string
}

func newFakeDocumentVectorLedger(claims ...*store.DocumentVectorChunkClaim) *fakeDocumentVectorLedger {
	return &fakeDocumentVectorLedger{
		generation: store.DocumentVectorGeneration{
			ID: 1, State: store.DocumentVectorGenerationBuilding,
			DocumentVectorGenerationSpec: store.DocumentVectorGenerationSpec{Dimension: 3},
		},
		claims: claims, commitErr: map[string]error{}, renewTokens: map[string]int{},
		renewErrByToken: map[string]error{},
	}
}

func (l *fakeDocumentVectorLedger) GetDocumentVectorGeneration(context.Context, int64) (store.DocumentVectorGeneration, error) {
	return l.generation, nil
}

func (l *fakeDocumentVectorLedger) ListDocumentVectorChunkCandidates(_ context.Context, _ int64, after int64, limit int) ([]store.DocumentVectorChunkCandidate, error) {
	candidates := make([]store.DocumentVectorChunkCandidate, 0, min(limit, len(l.claims)))
	for _, claim := range l.claims {
		if claim.ChunkID > after {
			candidates = append(candidates, claim.DocumentVectorChunkCandidate)
			if len(candidates) == limit {
				break
			}
		}
	}
	return candidates, nil
}

func (l *fakeDocumentVectorLedger) ClaimDocumentVectorChunk(_ context.Context, _ int64, after int64, _ int, _ string, _ time.Time, _ time.Duration) (*store.DocumentVectorChunkClaim, error) {
	l.claimCalls++
	for index, claim := range l.claims {
		if claim.ChunkID <= after {
			continue
		}
		if claim.ChunkID <= l.unclaimableThrough {
			return nil, nil //nolint:nilnil // No claim is a valid ledger result.
		}
		l.claims = append(l.claims[:index], l.claims[index+1:]...)
		return claim, nil
	}
	return nil, nil //nolint:nilnil // No claim is a valid ledger result.
}

func (l *fakeDocumentVectorLedger) RenewDocumentVectorChunkClaim(_ context.Context, _ int64, token, _ string, _ int64, _ time.Time, _ time.Duration) (time.Time, error) {
	l.mu.Lock()
	l.renewCalls++
	l.renewTokens[token]++
	err := l.renewErr
	if tokenErr := l.renewErrByToken[token]; tokenErr != nil {
		err = tokenErr
	}
	renewed := l.renewed
	l.mu.Unlock()
	if renewed != nil {
		l.renewOnce.Do(func() { close(renewed) })
	}
	return workerNow.Add(time.Minute), err
}

func (l *fakeDocumentVectorLedger) renewTokenCallCount(token string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renewTokens[token]
}

func (l *fakeDocumentVectorLedger) renewCallCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renewCalls
}

func (l *fakeDocumentVectorLedger) CommitDocumentVectorPublication(ctx context.Context, _ int64, token, _ string, _ int64, _ time.Time) error {
	if l.beforeCommit != nil {
		l.beforeCommit(token)
	}
	if l.commit != nil {
		if err := l.commit(ctx, token); err != nil {
			return err
		}
	}
	if err := l.commitErr[token]; err != nil {
		return err
	}
	l.committed = append(l.committed, token)
	return nil
}

func (l *fakeDocumentVectorLedger) FailDocumentVectorChunk(ctx context.Context, _ int64, token, _ string, _ int64, _ time.Time, retryAt *time.Time, terminal bool, errorCode string) error {
	if l.fail != nil {
		if err := l.fail(ctx, token); err != nil {
			return err
		}
	}
	l.failures = append(l.failures, fakeDocumentVectorFailure{
		token: token, retryAt: retryAt, terminal: terminal, errorCode: errorCode,
	})
	return nil
}

type fakeDocumentVectorProvider struct {
	calls   [][]embed.DocumentInput
	vectors [][][]float32
	err     error
	call    func(context.Context, []embed.DocumentInput) ([][][]float32, error)
}

func (p *fakeDocumentVectorProvider) EmbedDocuments(ctx context.Context, inputs []embed.DocumentInput) ([][][]float32, error) {
	cloned := make([]embed.DocumentInput, len(inputs))
	for index := range inputs {
		cloned[index].Chunks = append([]string(nil), inputs[index].Chunks...)
	}
	p.calls = append(p.calls, cloned)
	if p.call != nil {
		return p.call(ctx, inputs)
	}
	return p.vectors, p.err
}

type fakeDocumentVectorBackend struct {
	puts      [][]Embedding
	deletes   [][]string
	putErr    error
	deleteErr error
	put       func(context.Context, []Embedding) error
}

func (b *fakeDocumentVectorBackend) PutUnpublished(ctx context.Context, _ GenerationID, _ int, embeddings []Embedding) error {
	b.puts = append(b.puts, append([]Embedding(nil), embeddings...))
	if b.put != nil {
		return b.put(ctx, embeddings)
	}
	return b.putErr
}

func (b *fakeDocumentVectorBackend) DeleteTokens(_ context.Context, _ GenerationID, tokens []string) error {
	b.deletes = append(b.deletes, append([]string(nil), tokens...))
	return b.deleteErr
}

func (*fakeDocumentVectorBackend) Search(context.Context, GenerationID, int, []float32, int) ([]Hit, error) {
	return nil, errors.New("not used")
}

var workerNow = time.Date(2026, 8, 20, 22, 0, 0, 0, time.UTC)

func newFakeWorker(ledger *fakeDocumentVectorLedger, provider Provider, backend Backend) *Worker {
	return NewWorker(WorkerDeps{
		Ledger: ledger, Provider: provider, Backend: backend,
		Owner: "worker-a", Dimension: 3, MaxInputChars: 1000, LeaseDuration: time.Minute,
		HeartbeatInterval: 10 * time.Second, RetryDelay: time.Minute,
		MaxAttempts: 3, Now: func() time.Time { return workerNow },
		prepareInputs: func(_ context.Context, _ Ledger, recipe docembedding.Recipe, claims []*store.DocumentVectorChunkClaim) (map[string]string, error) {
			inputs := make(map[string]string, len(claims))
			for _, claim := range claims {
				runes := []rune(claim.Text)
				if len(runes) > recipe.Values().MaxInputRunes {
					runes = runes[:recipe.Values().MaxInputRunes]
				}
				inputs[claim.Token] = string(runes)
			}
			return inputs, nil
		},
	})
}

func workerClaim(extractionID string, chunkID int64, text, token string) *store.DocumentVectorChunkClaim {
	return &store.DocumentVectorChunkClaim{
		GenerationID: 1, ExtractionID: extractionID, ChunkID: chunkID,
		ChunkKey: token + "-key", Text: text,
		Token: token, LeaseOwner: "worker-a", LeaseFence: 1,
		LeaseUntil: time.Date(2026, 8, 20, 22, 1, 0, 0, time.UTC), AttemptCount: 1,
	}
}

func embeddingTokens(embeddings []Embedding) []string {
	tokens := make([]string, len(embeddings))
	for index := range embeddings {
		tokens[index] = embeddings[index].Token
	}
	return tokens
}
