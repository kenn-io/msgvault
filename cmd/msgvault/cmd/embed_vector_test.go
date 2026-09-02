//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type cliActivationBackend struct {
	vector.Backend

	activateErr   error
	activateCalls []vector.GenerationID
	sequences     []int64
}

func (b *cliActivationBackend) ActivateGenerationIfConverged(
	_ context.Context, gen vector.GenerationID, expectedSequence int64,
) error {
	b.activateCalls = append(b.activateCalls, gen)
	b.sequences = append(b.sequences, expectedSequence)
	return b.activateErr
}

func (b *cliActivationBackend) ActivateGeneration(_ context.Context, gen vector.GenerationID, force bool) error {
	if force {
		panic("CLI build activation must never force")
	}
	b.activateCalls = append(b.activateCalls, gen)
	return b.activateErr
}

type cliConvergenceChecker struct {
	state scheduler.ConvergenceResult
	err   error
}

type cliPassRunner struct {
	results []embed.RunResult
	errs    []error
	calls   int
}

func (r *cliPassRunner) RunOnce(context.Context, vector.GenerationID) (embed.RunResult, error) {
	call := r.calls
	r.calls++
	result := r.results[min(call, len(r.results)-1)]
	if len(r.errs) == 0 {
		return result, nil
	}
	return result, r.errs[min(call, len(r.errs)-1)]
}

func (r *cliPassRunner) RunBackstop(context.Context, vector.GenerationID) (embed.RunResult, error) {
	return embed.RunResult{}, errors.New("unexpected backstop")
}

func (r *cliPassRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func TestRunEmbeddingPasses_ContextualCLIContinuesUntilConverged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runner := &cliPassRunner{results: []embed.RunResult{
		{Claimed: 2, Succeeded: 2, Contextual: &embed.ContextConvergence{Converged: false}},
		{Claimed: 3, Succeeded: 3, Contextual: &embed.ContextConvergence{Converged: false}},
		{Claimed: 1, Succeeded: 1, Contextual: &embed.ContextConvergence{Converged: true}},
	}}

	result, err := runEmbeddingPasses(t.Context(), runner, 7, false, vector.APIFormatVoyageContextual, &bytes.Buffer{})
	require.NoError(err)
	assert.Equal(3, runner.calls)
	assert.Equal(6, result.Claimed)
	assert.Equal(6, result.Succeeded)
	require.NotNil(result.Contextual)
	assert.True(result.Contextual.Converged)
}

func TestRunEmbeddingPasses_ContextualCLIRejectsNonProgress(t *testing.T) {
	runner := &cliPassRunner{results: []embed.RunResult{{
		Contextual: &embed.ContextConvergence{Converged: false},
	}}}

	_, err := runEmbeddingPasses(t.Context(), runner, 7, false, vector.APIFormatVoyageContextual, &bytes.Buffer{})
	require.ErrorContains(t, err, "made no progress")
	assert.Equal(t, 1, runner.calls)
}

func TestRunEmbeddingPasses_PersonOnlyFailurePreservesMessageProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	personErr := errors.New("person provider failed")
	runner := &cliPassRunner{
		results: []embed.RunResult{{Claimed: 3, Succeeded: 3}},
		errs:    []error{&embed.GenerationRunError{Person: personErr}},
	}

	var stderr bytes.Buffer
	result, err := runEmbeddingPasses(t.Context(), runner, 7, false, vector.APIFormatOpenAI, &stderr)
	require.NoError(err)
	assert.Equal(1, runner.calls)
	assert.Equal(3, result.Claimed)
	assert.Equal(3, result.Succeeded)
	assert.Contains(stderr.String(), personErr.Error())
}

func (c cliConvergenceChecker) CheckConvergence(context.Context, vector.GenerationID) (scheduler.ConvergenceResult, error) {
	return c.state, c.err
}

func TestActivateBuiltGeneration_ContextualBehavior(t *testing.T) {
	newAssert := assert.New
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		LatestJournalSequence:   9,
		ConsumedJournalSequence: 9,
		ReconciliationComplete:  true,
	}
	tests := []struct {
		name   string
		mutate func(*scheduler.ConvergenceResult)
		want   string
	}{
		{name: "message coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.MessageCoverageComplete = false
			s.MessageCoverageMissing = 2
		}, want: "message_coverage_complete=false (missing=2)"},
		{name: "person coverage incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.PersonCoverageComplete = false
			s.PersonCoverageMismatched = 2
		}, want: "person_coverage_complete=false (mismatched=2, rejected=0)"},
		{name: "journal incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ConsumedJournalSequence = 8
		}, want: "journal=8/9"},
		{name: "reconciliation incomplete", mutate: func(s *scheduler.ConvergenceResult) {
			s.ReconciliationComplete = false
		}, want: "reconciliation_complete=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := newAssert(t)
			require := newRequire(t)
			state := complete
			tt.mutate(&state)
			backend := &cliActivationBackend{}
			var stdout, stderr bytes.Buffer
			activated, err := activateBuiltGeneration(t.Context(), backend,
				cliConvergenceChecker{state: state}, 7, vector.APIFormatVoyageContextual,
				&stdout, &stderr)
			require.NoError(err)
			assert.False(activated)
			assert.Empty(backend.activateCalls)
			assert.Empty(stdout.String())
			assert.Contains(stderr.String(), tt.want)
			assert.Contains(stderr.String(), "generation 7 has not converged")
		})
	}

	backend := &cliActivationBackend{}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: complete}, 7, vector.APIFormatVoyageContextual,
		&stdout, &stderr)
	require.NoError(err)
	assert.True(activated)
	assert.Equal([]vector.GenerationID{7}, backend.activateCalls)
	assert.Equal([]int64{9}, backend.sequences)
	assert.Equal("Generation 7 activated.\n", stdout.String())
	assert.Empty(stderr.String())
}

func TestActivateBuiltGeneration_OpenAILegacyHintUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &cliActivationBackend{}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: false,
		MessageCoverageMissing:  3,
		PersonCoverageComplete:  true,
		ReconciliationComplete:  true,
	}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: state}, 7, vector.APIFormatOpenAI, &stdout, &stderr)
	require.NoError(err)
	assert.False(activated)
	assert.Empty(backend.activateCalls)
	assert.Equal(remainingCoverageHint(7, 3), stderr.String())
}

func TestActivateBuiltGeneration_OpenAICompleteUsesLegacyActivation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &cliActivationBackend{}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		ReconciliationComplete:  true,
	}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: state}, 7, vector.APIFormatOpenAI, &stdout, &stderr)
	require.NoError(err)
	assert.True(activated)
	assert.Equal([]vector.GenerationID{7}, backend.activateCalls)
	assert.Empty(backend.sequences, "legacy builds must not require contextual document progress")
	assert.Equal("Generation 7 activated.\n", stdout.String())
	assert.Empty(stderr.String())
}

// TestActivateBuiltGenerationOpenAIReportsPersonCoverage catches the legacy
// CLI claiming zero message stragglers while exact person revisions still
// make the generation unsafe to activate.
func TestActivateBuiltGenerationOpenAIReportsPersonCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &cliActivationBackend{}
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete:  true,
		PersonCoverageComplete:   false,
		PersonCoverageMismatched: 2,
		ReconciliationComplete:   true,
	}
	var stdout, stderr bytes.Buffer
	activated, err := activateBuiltGeneration(t.Context(), backend,
		cliConvergenceChecker{state: state}, 7, vector.APIFormatOpenAI, &stdout, &stderr)
	require.NoError(err)
	assert.False(activated)
	assert.Empty(backend.activateCalls)
	assert.Contains(stderr.String(), "person_coverage_complete=false (mismatched=2, rejected=0)")
}

func TestRejectedOnlyPersonCoverageReportsTerminalRecoveryOptions(t *testing.T) {
	state := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  false,
		PersonCoverageRejected:  2,
		ReconciliationComplete:  true,
	}
	for name, err := range map[string]error{
		"automatic activation": convergenceError(7, state),
		"manual activation":    manualConvergenceError(7, state),
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, err.Error(), "resume --backstop")
			assert.Contains(t, err.Error(), "source revision must change")
			assert.Contains(t, err.Error(), "msgvault embeddings activate 7 --force")
		})
	}
}

func TestActivateBuiltGeneration_ContextualLifecycleErrorsDoNotActivateAnotherGeneration(t *testing.T) {
	complete := scheduler.ConvergenceResult{
		MessageCoverageComplete: true,
		PersonCoverageComplete:  true,
		ReconciliationComplete:  true,
	}
	t.Run("checker rejects retired generation", func(t *testing.T) {
		backend := &cliActivationBackend{}
		activated, err := activateBuiltGeneration(t.Context(), backend,
			cliConvergenceChecker{err: vector.ErrGenerationRetired}, 7,
			vector.APIFormatVoyageContextual, &bytes.Buffer{}, &bytes.Buffer{})
		assert.False(t, activated)
		require.ErrorIs(t, err, vector.ErrGenerationRetired)
		assert.Empty(t, backend.activateCalls)
	})
	t.Run("backend rejects retired generation", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		backend := &cliActivationBackend{activateErr: vector.ErrGenerationRetired}
		activated, err := activateBuiltGeneration(t.Context(), backend,
			cliConvergenceChecker{state: complete}, 7,
			vector.APIFormatVoyageContextual, &bytes.Buffer{}, &bytes.Buffer{})
		assert.False(activated)
		require.ErrorIs(err, vector.ErrGenerationRetired)
		assert.Equal([]vector.GenerationID{7}, backend.activateCalls)
		assert.Equal([]int64{0}, backend.sequences)
	})
}

func setupVectorFeaturesFixture(
	t *testing.T, apiFormat vector.EmbeddingAPIFormat, readOnly bool, mutate ...func(*config.Config),
) *vectorFeatures {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	c := config.NewDefaultConfig()
	c.HomeDir = dir
	c.Data.DataDir = dir
	c.Vector.Enabled = true
	c.Vector.DBPath = filepath.Join(dir, "vectors.db")
	c.Vector.Embeddings.Endpoint = "https://example.invalid/v1"
	c.Vector.Embeddings.Model = "text-embedding-test"
	c.Vector.Embeddings.Dimension = 4
	c.Vector.Embeddings.APIFormat = apiFormat
	c.Attachments.Documents.Index.Embeddings.Enabled = true
	c.Attachments.Documents.Index.Embeddings.Profile = "vector.embeddings"
	if apiFormat == vector.APIFormatVoyageContextual {
		c.Vector.Embeddings.Model = "voyage-context-4"
	}
	for _, apply := range mutate {
		apply(c)
	}
	require.NoError(t, c.Save())
	withTestConfig(t, c)

	s, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.InitSchema())
	if c.Vector.People.Enabled {
		semanticProfile, err := c.Vector.SemanticPersonEmbeddingProfile()
		require.NoError(t, err)
		_, err = s.EnsurePersonSemanticEmbeddingProfile(t.Context(), semanticProfile)
		require.NoError(t, err)
		_, _, err = s.GrantPersonSemanticEmbeddingConsent(
			t.Context(), semanticProfile.Fingerprint, "test",
		)
		require.NoError(t, err)
	}
	fingerprint := strings.Repeat("d", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint, Provider: "synthetic",
		Endpoint: "https://documents.example.test/v1", Region: localValue, Model: "extract-test",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err = s.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(t, err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE document_index_state SET target_profile_id = ? WHERE singleton = 1`), profile.ID)
	require.NoError(t, err)
	spec, err := configuredDocumentVectorSpec(t.Context(), s)
	require.NoError(t, err)
	documentConsent, err := configuredDocumentVectorConsentSpec(spec)
	require.NoError(t, err)
	queryConsent, err := configuredDocumentVectorQueryConsentSpec(spec)
	require.NoError(t, err)
	for _, consentSpec := range []store.DocumentVectorConsentSpec{documentConsent, queryConsent} {
		_, _, err = s.RecordDocumentVectorConsent(t.Context(), consentSpec, time.Now())
		require.NoError(t, err)
	}

	vf, err := setupVectorFeatures(t.Context(), s, mainPath, readOnly)
	require.NoError(t, err)
	require.NotNil(t, vf)
	t.Cleanup(func() { _ = vf.Close() })
	return vf
}

func TestSetupVectorFeaturesDocumentClientRejectsCrossOriginRedirects(t *testing.T) {
	for _, apiFormat := range []vector.EmbeddingAPIFormat{vector.APIFormatOpenAI, vector.APIFormatVoyageContextual} {
		t.Run(string(apiFormat), func(t *testing.T) {
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetRequests.Add(1)
			}))
			t.Cleanup(target.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL+"/outside-consent")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(origin.Close)

			vf := setupVectorFeaturesFixture(t, apiFormat, false, func(c *config.Config) {
				c.Vector.Embeddings.Endpoint = origin.URL
			})
			_, err := vf.SemanticClient.EmbedDocuments(t.Context(), []vector.DocumentInput{{
				Chunks: []string{"private attachment text"},
			}})

			require.ErrorIs(t, err, embed.ErrEmbeddingProviderRedirect)
			_, err = vf.DocumentQueryClient.EmbedQuery(t.Context(), "private query text")
			require.ErrorIs(t, err, embed.ErrEmbeddingProviderRedirect)
			assert.Zero(t, targetRequests.Load(), "attachment text must not reach the redirect target")
		})
	}
}

func TestSetupVectorFeatures_AppliesOpenAIEmbeddingPrefixes(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	maxChunk := strings.Repeat("x", 2000)
	var calls [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !check.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		calls = append(calls, request.Input)
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{
				"embedding": []float32{1, 2, 3, 4},
				"index":     i,
			}
		}
		check.NoError(json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	vf := setupVectorFeaturesFixture(t, vector.APIFormatOpenAI, false, func(c *config.Config) {
		c.Vector.Embeddings.Endpoint = server.URL
		c.Vector.Embeddings.MaxInputChars = len(maxChunk)
		c.Vector.Embeddings.DocumentPrefix = "search_document: "
		c.Vector.Embeddings.QueryPrefix = "search_query: "
		c.Vector.People = vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		}
	})

	_, err := vf.SemanticClient.EmbedDocuments(t.Context(), []vector.DocumentInput{
		{Chunks: []string{"first chunk", "second chunk"}},
	})
	must.NoError(err)
	_, err = vf.DocumentQueryClient.EmbedQuery(t.Context(), "document query")
	must.NoError(err)
	_, err = vf.HybridEngine.EmbedQuery(t.Context(), "message query")
	must.NoError(err)
	_, err = vf.PersonQueryClient.EmbedQuery(t.Context(), "person query")
	must.NoError(err)
	_, err = vf.SemanticClient.EmbedDocuments(t.Context(), []vector.DocumentInput{
		{Chunks: []string{maxChunk}},
	})
	must.NoError(err)

	check.Equal([][]string{
		{"search_document: first chunk", "search_document: second chunk"},
		{"search_query: document query"},
		{"search_query: message query"},
		{"search_query: person query"},
		{"search_document: " + maxChunk},
	}, calls)
	must.Len(calls, 5)
	must.Len(calls[4], 1)
	check.Len(calls[4][0], len(maxChunk)+len("search_document: "))
}

func TestSetupVectorFeatures_AppliesVoyageEmbeddingPrefixes(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	maxChunk := strings.Repeat("x", 2000)
	type voyageCall struct {
		Inputs    [][]string `json:"inputs"`
		InputType string     `json:"input_type"`
	}
	var calls []voyageCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request voyageCall
		if !check.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		calls = append(calls, request)
		outer := make([]map[string]any, len(request.Inputs))
		for i, chunks := range request.Inputs {
			inner := make([]map[string]any, len(chunks))
			for j := range chunks {
				inner[j] = map[string]any{
					"embedding": []float32{1, 2, 3, 4},
					"index":     j,
				}
			}
			outer[i] = map[string]any{"data": inner, "index": i}
		}
		check.NoError(json.NewEncoder(w).Encode(map[string]any{"data": outer}))
	}))
	t.Cleanup(server.Close)
	vf := setupVectorFeaturesFixture(t, vector.APIFormatVoyageContextual, false, func(c *config.Config) {
		c.Vector.Embeddings.Endpoint = server.URL
		c.Vector.Embeddings.MaxInputChars = len(maxChunk)
		c.Vector.Embeddings.DocumentPrefix = "search_document: "
		c.Vector.Embeddings.QueryPrefix = "search_query: "
		c.Vector.People = vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		}
	})

	_, err := vf.SemanticClient.EmbedDocuments(t.Context(), []vector.DocumentInput{
		{Chunks: []string{"first chunk", "second chunk"}},
	})
	must.NoError(err)
	_, err = vf.DocumentQueryClient.EmbedQuery(t.Context(), "document query")
	must.NoError(err)
	_, err = vf.HybridEngine.EmbedQuery(t.Context(), "message query")
	must.NoError(err)
	_, err = vf.PersonQueryClient.EmbedQuery(t.Context(), "person query")
	must.NoError(err)
	_, err = vf.SemanticClient.EmbedDocuments(t.Context(), []vector.DocumentInput{
		{Chunks: []string{maxChunk}},
	})
	must.NoError(err)

	check.Equal([]voyageCall{
		{InputType: "document", Inputs: [][]string{{"search_document: first chunk", "search_document: second chunk"}}},
		{InputType: "query", Inputs: [][]string{{"search_query: document query"}}},
		{InputType: "query", Inputs: [][]string{{"search_query: message query"}}},
		{InputType: "query", Inputs: [][]string{{"search_query: person query"}}},
		{InputType: "document", Inputs: [][]string{{"search_document: " + maxChunk}}},
	}, calls)
	must.Len(calls, 5)
	must.Len(calls[4].Inputs, 1)
	must.Len(calls[4].Inputs[0], 1)
	check.Len(calls[4].Inputs[0][0], len(maxChunk)+len("search_document: "))
}

func TestSetupVectorFeatures_SelectsRunnerByAPIFormat(t *testing.T) {
	t.Run("implicit OpenAI", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, "", false)
		assert.IsType(t, &embed.GenerationWorker{}, vf.Runner)
		assert.IsType(t, &legacyConvergenceChecker{}, vf.Convergence)
		assertConfiguredPersonSearchEngine(t, vf)
	})
	t.Run("explicit OpenAI", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, vector.APIFormatOpenAI, false)
		assert.IsType(t, &embed.GenerationWorker{}, vf.Runner)
		assert.IsType(t, &legacyConvergenceChecker{}, vf.Convergence)
		assertConfiguredPersonSearchEngine(t, vf)
	})
	t.Run("Voyage contextual", func(t *testing.T) {
		vf := setupVectorFeaturesFixture(t, vector.APIFormatVoyageContextual, false)
		assert.IsType(t, &embed.GenerationWorker{}, vf.Runner)
		assert.IsType(t, &contextualConvergenceChecker{}, vf.Convergence)
		assertConfiguredPersonSearchEngine(t, vf)
	})
}

func TestSetupVectorFeatures_ReadOnlyContextualDoesNotStartRunnerWrites(t *testing.T) {
	vf := setupVectorFeaturesFixture(t, vector.APIFormatVoyageContextual, true)
	assert.IsType(t, &embed.GenerationWorker{}, vf.Runner)
	assertConfiguredPersonSearchEngine(t, vf)
}

func assertConfiguredPersonSearchEngine(t *testing.T, vf *vectorFeatures) {
	t.Helper()
	require.NotNil(t, vf.PersonSearchEngine,
		"every embedding API format must install semantic person search")
	_, err := vf.PersonSearchEngine.Search(t.Context(), "synthetic person", 1)
	require.ErrorIs(t, err, vector.ErrSemanticPersonEmbeddingsDisabled,
		"the person engine must enforce the default-off curated-person policy")
}

func TestLegacyConvergenceTreatsAuthorizationUnavailableAsNoRequiredPersonCoverage(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "disabled", err: vector.ErrSemanticPersonEmbeddingsDisabled},
		{name: "revoked or unconsented", err: vector.ErrSemanticPersonEmbeddingConsentRequired},
		{name: "runtime policy drift", err: vector.ErrSemanticPersonEmbeddingRuntimeStale},
		{
			name: "invalid or unavailable live policy",
			err: fmt.Errorf("%w: synthetic config source unavailable",
				vector.ErrSemanticPersonEmbeddingPolicyUnavailable),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := &legacyConvergenceChecker{
				personGate: vector.SemanticPersonEmbeddingGateFunc(
					func(context.Context) error { return test.err },
				),
			}

			coverage, err := checker.CheckPersonCoverage(t.Context(), 7)

			require.NoError(t, err)
			assert.True(t, coverage.Complete())
		})
	}
}

// TestConfiguredConvergenceRequiresExactPersonRevisionsAndAllowsZeroPeople
// catches activation checks that look only at message coverage, ignore stale
// revisions/orphans, or treat an empty curated person corpus as incomplete.
func TestConfiguredConvergenceRequiresExactPersonRevisionsAndAllowsZeroPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	vectorPath := filepath.Join(dir, "vectors.db")
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Vector.DBPath = vectorPath
	c.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	c.Vector.Embeddings.Model = "text-embedding-test"
	c.Vector.Embeddings.Dimension = 2
	c.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}

	mainStore, err := store.Open(mainPath)
	require.NoError(err)
	t.Cleanup(func() { _ = mainStore.Close() })
	require.NoError(mainStore.InitSchema())
	semanticProfile, err := c.Vector.SemanticPersonEmbeddingProfile()
	require.NoError(err)
	_, err = mainStore.EnsurePersonSemanticEmbeddingProfile(t.Context(), semanticProfile)
	require.NoError(err)
	_, _, err = mainStore.GrantPersonSemanticEmbeddingConsent(
		t.Context(), semanticProfile.Fingerprint, "test",
	)
	require.NoError(err)
	require.NoError(sqlitevec.RegisterExtension())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: vectorPath, MainPath: mainPath, Dimension: 2, MainDB: mainStore.DB(),
	})
	require.NoError(err)
	t.Cleanup(func() { _ = backend.Close() })
	gen, err := backend.CreateGeneration(t.Context(), c.Vector.Embeddings.Model, 2, c.Vector.GenerationFingerprint())
	require.NoError(err)
	personGate := vector.NewExactSemanticPersonEmbeddingGate(
		func() (vector.Config, error) { return c.Vector, nil }, mainStore,
	)
	checker, err := newConvergenceChecker(c.Vector, mainStore, backend, personGate)
	require.NoError(err)
	personChecker, ok := checker.(personsearch.CoverageChecker)
	require.True(ok, "configured convergence must expose the person-only readiness check")
	_, err = mainStore.DB().Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'person-coverage@example.test');
		INSERT INTO conversations (id, source_id, conversation_type) VALUES (1, 1, 'email_thread');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
		VALUES (1, 1, 1, 'person-coverage-message', 'email');
	`)
	require.NoError(err)
	personCoverage, err := personChecker.CheckPersonCoverage(t.Context(), gen)
	require.NoError(err)
	assert.True(personCoverage.Complete(),
		"person search readiness must not scan or depend on unrelated message coverage")
	state, err := checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.False(state.MessageCoverageComplete, "precondition: message coverage is incomplete")
	_, err = mainStore.DB().Exec(`UPDATE messages SET embed_gen = ? WHERE id = 1`, gen)
	require.NoError(err)

	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.True(state.PersonCoverageComplete)
	assert.Zero(state.PersonCoverageMismatched)
	assert.True(state.Complete(), "zero-person archive must converge")
	_, err = mainStore.DB().Exec(`INSERT INTO persons (vcard_uid) VALUES (?)`,
		"urn:uuid:00000000-0000-0000-0000-000000000000")
	require.NoError(err)
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.True(state.Complete(), "person without semantic text must not require a vector")
	_, err = mainStore.DB().Exec(`DELETE FROM persons WHERE vcard_uid = ?`,
		"urn:uuid:00000000-0000-0000-0000-000000000000")
	require.NoError(err)

	_, err = mainStore.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?)`,
		"urn:uuid:00000000-0000-0000-0000-000000000001", "Synthetic Person")
	require.NoError(err)
	documents, err := mainStore.ListPersonSemanticDocumentsContext(t.Context())
	require.NoError(err)
	require.Len(documents, 1)

	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.False(state.PersonCoverageComplete)
	assert.Equal(int64(1), state.PersonCoverageMismatched)
	assert.False(state.Complete(), "missing person vector must block activation")

	require.NoError(backend.UpsertPersons(t.Context(), gen, []vector.PersonEmbedding{{
		PersonID: documents[0].PersonID, Revision: documents[0].Revision,
	}}))
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.False(state.PersonCoverageComplete)
	assert.False(state.Complete(), "a terminal provider rejection must remain visible")

	require.NoError(backend.UpsertPersons(t.Context(), gen, []vector.PersonEmbedding{{
		PersonID: documents[0].PersonID, Revision: documents[0].Revision, Vector: []float32{1, 2},
	}}))
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.True(state.Complete())

	_, err = mainStore.DB().Exec(`UPDATE persons SET display_name = ? WHERE id = ?`,
		"Synthetic Person Updated", documents[0].PersonID)
	require.NoError(err)
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.False(state.PersonCoverageComplete)
	assert.Equal(int64(1), state.PersonCoverageMismatched)
	assert.False(state.Complete(), "stale exact digest must block activation")

	_, err = mainStore.DB().Exec(`DELETE FROM persons WHERE id = ?`, documents[0].PersonID)
	require.NoError(err)
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.False(state.PersonCoverageComplete)
	assert.Equal(int64(1), state.PersonCoverageMismatched, "orphaned vector must block activation")
	require.NoError(backend.DeletePersonsNotIn(t.Context(), gen, nil))
	state, err = checker.CheckConvergence(t.Context(), gen)
	require.NoError(err)
	assert.True(state.Complete(), "reconciled zero-person archive must converge")
}

// openTestBackend opens a fresh in-memory-ish sqlitevec backend with a
// single pre-seeded message so the scan-and-fill worker has a message to
// discover and embed.
func openTestBackend(t *testing.T) *sqlitevec.Backend {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	main, err := sql.Open("sqlite3", mainPath)
	require.NoError(t, err, "open main")
	t.Cleanup(func() { _ = main.Close() })
	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);`
	_, err = main.Exec(schema)
	require.NoError(t, err, "schema")
	_, err = main.Exec(`INSERT INTO messages (id) VALUES (1)`)
	require.NoError(t, err, "seed")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    main,
	})
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// openStderrSink returns a *os.File pointing at /dev/null so
// pickEmbedGeneration's status prints do not clutter test output.
func openStderrSink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	require.NoError(t, err, "open /dev/null")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestPickEmbedGeneration_ResumesBuildingGeneration covers the main
// recovery path: after a partial full-rebuild, running `msgvault
// embed` (without --full-rebuild) must return the existing building
// generation and report rebuildInProgress=true, so activation logic
// still runs when pending drains to zero. Previously this path
// errored out with ErrIndexBuilding.
func TestPickEmbedGeneration_ResumesBuildingGeneration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Simulate an interrupted full rebuild: a building generation
	// exists but no active generation.
	gen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration (should resume, not error)")
	assert.Equal(gen, gotGen, "gotGen mismatch")
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true (building generation)")
}

// TestPickEmbedGeneration_NoGenerations_HintsFullRebuild covers the
// "fresh install" path: default-mode embed with no generations must
// surface a clear hint rather than silently doing nothing.
func TestPickEmbedGeneration_NoGenerations_HintsFullRebuild(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected error when no generations exist")
	// Intentional: we wrap the underlying error with a hint, but the
	// underlying sentinel should still be errors.Is-reachable so
	// upstream callers can branch on it.
	require.ErrorIs(t, err, vector.ErrNotEnabled, "err should wrap ErrNotEnabled")
}

// TestPickEmbedGeneration_ResumeFingerprintMismatch rejects a resume
// when the in-progress rebuild was started with a different model or
// dimension than the current config — continuing would silently
// embed against the wrong model.
func TestPickEmbedGeneration_ResumeFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)
	_, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(t, err, "CreateGeneration")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected fingerprint mismatch error")
	require.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint
// regression-guards the precedence bug where pickEmbedGeneration
// targeted an existing active generation even when a building
// generation for the configured embedding settings was in flight. The user
// expectation is that `msgvault embeddings build` drains the in-progress build
// (so it can be activated) rather than continuing to top up the old
// active generation.
func TestPickEmbedGeneration_PrefersBuildingOverActive_MatchingFingerprint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	// Build state: an active generation exists, and a second building
	// generation has been created for the SAME model+dim (the typical
	// "I want to refresh my index" pattern).
	activeGen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, activeGen, true), "ActivateGeneration")
	buildingGen, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (building)")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration")
	assert.Equal(buildingGen, gotGen, "preferring active=%d would leave the build stranded", activeGen)
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true (we picked the building generation)")
}

// TestPickEmbedGeneration_RejectsBuildingWithMismatchedFingerprint
// regression-guards the case where an active generation matches the
// config but a building generation exists for a DIFFERENT model. The
// previous code called ResolveActive first, found the matching active,
// and silently topped it up — leaving the mismatched build stranded
// without any warning. The new precedence-then-mismatch flow should
// either resume a matching build or refuse with a clear error.
func TestPickEmbedGeneration_RejectsBuildingWithMismatchedFingerprint(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	// State: building generation exists for an old model. No active
	// generation, and config now points at a different model.
	_, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(t, err, "CreateGeneration (building)")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected error for mismatched-fingerprint building generation")
	require.ErrorContains(t, err, "fingerprint", "error should mention fingerprint")
}

func TestPickEmbedGeneration_ContextualRejectsWrongGenerationFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	backend := openTestBackend(t)
	_, err := backend.CreateGeneration(ctx, "voyage-context-4", 4,
		"voyage-context-4:4:p1-111111:c32768:e1:avoyage-contextual:v0")
	require.NoError(err)

	_, rebuild, err := pickEmbedGeneration(ctx, backend, embedGenerationOpts{
		Model: "voyage-context-4", Dimension: 4,
		Fingerprint: "voyage-context-4:4:p1-111111:c32768:e1:avoyage-contextual:v1",
		Stderr:      openStderrSink(t),
	})
	require.Error(err)
	assert.False(rebuild)
	assert.Contains(err.Error(), "in-progress rebuild has fingerprint")
	assert.Contains(err.Error(), "avoyage-contextual:v0")
	assert.Contains(err.Error(), "avoyage-contextual:v1")

	building, lookupErr := backend.BuildingGeneration(ctx)
	require.NoError(lookupErr)
	require.NotNil(building)
	assert.Equal(vector.GenerationBuilding, building.State)
}

// TestPickEmbedGeneration_StaleActivePlusMatchingBuilding covers the
// "stale active + matching building" combination R51a calls out: an
// older active generation exists with a fingerprint that no longer
// matches the configured embedding settings, and a newer building generation
// matches. The configured-model build must be drained instead of the
// stale active one being topped up — otherwise the new build stays
// stuck in `building` indefinitely.
func TestPickEmbedGeneration_StaleActivePlusMatchingBuilding(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	staleActive, err := b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(err, "CreateGeneration (stale active)")
	require.NoError(b.ActivateGeneration(ctx, staleActive, true), "ActivateGeneration")
	matchingBuilding, err := b.CreateGeneration(ctx, "new-model", 4, "")
	require.NoError(err, "CreateGeneration (matching building)")

	gotGen, rebuildInProgress, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "new-model",
		Dimension:   4,
		Fingerprint: "new-model:4",
		Stderr:      openStderrSink(t),
	})
	require.NoError(err, "pickEmbedGeneration (should resume matching build)")
	assert.Equal(matchingBuilding, gotGen, "stale active=%d must not steal precedence", staleActive)
	assert.True(rebuildInProgress, "rebuildInProgress=false, want true")
}

// TestPickEmbedGeneration_ActivePlusMismatchedBuildingRejected covers
// the case where the active generation matches the configured
// fingerprint AND a building generation exists for a different model.
// Silently topping up the active would leave the wrong-model build
// stranded forever; the user has to explicitly retire or activate it
// before embedding can proceed. Regression for the bug where the code
// only rejected mismatched builds via the ErrIndexBuilding branch and
// missed this active-also-matches case.
func TestPickEmbedGeneration_ActivePlusMismatchedBuildingRejected(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	b := openTestBackend(t)

	matchingActive, err := b.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(err, "CreateGeneration (active)")
	require.NoError(b.ActivateGeneration(ctx, matchingActive, true), "ActivateGeneration")
	_, err = b.CreateGeneration(ctx, "old-model", 4, "")
	require.NoError(err, "CreateGeneration (stale building)")

	_, _, err = pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: false,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Stderr:      openStderrSink(t),
	})
	require.Error(err, "expected error when a mismatched building exists alongside matching active")
	require.ErrorContains(err, "fingerprint", "error should mention fingerprint")
}

// TestPickEmbedGeneration_FullRebuildAbortsWhenDeclined verifies the
// Confirm hook short-circuits when the user declines a rebuild.
func TestPickEmbedGeneration_FullRebuildAbortsWhenDeclined(t *testing.T) {
	ctx := context.Background()
	b := openTestBackend(t)

	_, _, err := pickEmbedGeneration(ctx, b, embedGenerationOpts{
		FullRebuild: true,
		Model:       "fake",
		Dimension:   4,
		Fingerprint: "fake:4",
		Confirm:     func() bool { return false },
		Stderr:      openStderrSink(t),
	})
	require.Error(t, err, "expected abort error")
}

func TestRemainingCoverageHintMentionsBackstop(t *testing.T) {
	got := remainingCoverageHint(7, 3)

	assert.Contains(t, got, "Generation 7 still has 3 message(s) needing embedding")
	assert.Contains(t, got, "msgvault embeddings resume --backstop")
	assert.NotContains(t, got, "resume` again")
}

func TestNewProgressPrinter_UsesWindowedRate(t *testing.T) {
	assert := assert.New(t)
	var buf bytes.Buffer
	// window=2, total=210 so the percent path runs. The zero
	// interval keeps the test deterministic without sleeping.
	printer := newProgressPrinterWithMinInterval(&buf, 210, 2, 0)

	// Three calls. Pick values so the windowed rate at the final
	// event is different from the cumulative rate the old printer
	// would have shown — that way a regression to cumulative would
	// fail the assertion below, not just pass coincidentally.
	//
	//   call 1: Done=100, BatchMsgs=100, BatchElapsed=1s (lastPrint
	//           starts zero, so this emits and Adds).
	//   call 2: Done=200, BatchMsgs=100, BatchElapsed=1s.
	//   call 3: Done=210, BatchMsgs=10, BatchElapsed=5s.
	//
	// After call 3 the window holds the last two samples: (100,1s) and
	// (10,5s) → windowed rate = 110/6 ≈ 18.33 → printed "18 msg/s".
	// The old cumulative implementation would have printed
	// 210/RunElapsed=7s = 30 → "30 msg/s". Asserting on the final
	// line distinguishes the two.
	printer(embed.ProgressReport{
		Done: 100, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 200, TotalPending: 210,
		BatchMsgs: 100, BatchChars: 1000,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 210, TotalPending: 210,
		BatchMsgs: 10, BatchChars: 100,
		BatchElapsed: 5 * time.Second,
		RunElapsed:   7 * time.Second,
	})

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 emitted lines, got:\n%s", out)
	finalLine := lines[len(lines)-1]

	assert.Contains(finalLine, "(last 2)", "expected `(last 2)` annotation on final line")
	assert.Contains(finalLine, "18 msg/s", "expected windowed `18 msg/s` on final line")
	assert.NotContains(finalLine, "30 msg/s", "final line shows cumulative rate `30 msg/s`; windowed implementation should not produce this")
}

func TestNewProgressPrinter_DoesNotBypassThrottleAfterInitialTotal(t *testing.T) {
	var buf bytes.Buffer
	printer := newProgressPrinter(&buf, 2, 2)

	printer(embed.ProgressReport{
		Done: 2, TotalPending: 2,
		BatchMsgs: 2, BatchChars: 20,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   1 * time.Second,
	})
	printer(embed.ProgressReport{
		Done: 3, TotalPending: 2,
		BatchMsgs: 1, BatchChars: 10,
		BatchElapsed: 1 * time.Second,
		RunElapsed:   2 * time.Second,
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 1, "progress emitted %d lines, want 1 throttled line after initial total:\n%s", len(lines), buf.String())
}
