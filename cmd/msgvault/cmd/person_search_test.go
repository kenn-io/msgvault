//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestPersonSearchCommandUsesGeneratedRouteWithoutOpeningLocalStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/people/search", r.URL.Path)
		assertions.Equal("application/json", r.Header.Get("Content-Type"))
		assertions.Equal("synthetic-daemon-key", r.Header.Get("X-Api-Key"))
		var body struct {
			Query string `json:"query"`
			Limit int64  `json:"limit"`
		}
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assertions.Equal("synthetic systems architect", body.Query)
		assertions.Equal(int64(2), body.Limit)
		writePersonSearchCommandResponse(t, w)
	}))
	t.Cleanup(server.Close)

	blockedDataDir := filepath.Join(t.TempDir(), "not-a-directory")
	requirements.NoError(os.WriteFile(blockedDataDir, []byte("blocks direct database access"), 0o600))
	withStoreResolverConfig(t, &config.Config{
		HomeDir: blockedDataDir,
		Data:    config.DataConfig{DataDir: blockedDataDir},
		Remote: config.RemoteConfig{
			URL: server.URL, APIKey: "synthetic-daemon-key", AllowInsecure: true,
		},
	})

	command, stdout, stderr := newPersonSearchTestCommand(t)
	command.SetArgs([]string{"--limit", "2", "synthetic", "systems", "architect"})
	requirements.NoError(command.Execute())

	assertions.Equal(int32(1), requests.Load())
	output := stdout.String()
	first := strings.Index(output, "Synthetic Architect")
	second := strings.Index(output, "Test Researcher")
	assertions.GreaterOrEqual(first, 0, "first ranked result")
	assertions.Greater(second, first, "human output preserves daemon relevance order")
	assertions.Regexp(`(?m)^9\s+Synthetic Architect\s+0\.910000$`, output)
	assertions.Regexp(`(?m)^3\s+Test Researcher\s+0\.730000$`, output)
	assertions.Empty(stderr.String(), "person search emits no progress noise")
}

func TestPersonSearchCommandJSONHasStableExplicitOrderedShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writePersonSearchCommandResponse(t, w)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, AllowInsecure: true,
	}})

	command, stdout, stderr := newPersonSearchTestCommand(t)
	command.SetArgs([]string{"--json", "synthetic", "architect"})
	require.NoError(t, command.Execute())

	assert.JSONEq(t, `{
		"results": [
			{"id": 9, "display_name": "Synthetic Architect", "score": 0.91},
			{"id": 3, "display_name": "Test Researcher", "score": 0.73}
		]
	}`, stdout.String())
	assert.Empty(t, stderr.String(), "JSON mode emits no progress noise")
}

func TestPersonSearchCommandJSONPreservesEmptyResultsArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"results": []any{}}))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, AllowInsecure: true,
	}})

	command, stdout, stderr := newPersonSearchTestCommand(t)
	command.SetArgs([]string{"--json", "nobody"})
	require.NoError(t, command.Execute())

	assert.JSONEq(t, `{"results":[]}`, stdout.String())
	assert.Empty(t, stderr.String(), "empty JSON output emits no progress noise")
}

func TestPersonSearchCommandPreservesNilDisplayNameAcrossOutputModes(t *testing.T) {
	assertions := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{
				"person": map[string]any{
					"id": 11, "vcard_uid": "00000000-0000-0000-0000-000000000011",
					"revision": 1, "participant_ids": []int64{110},
					"created_at": "2026-08-20T00:00:00Z", "updated_at": "2026-08-20T00:00:00Z",
				},
				"score": 0.82,
			},
			{
				"person": map[string]any{
					"id": 4, "vcard_uid": "00000000-0000-0000-0000-000000000004",
					"display_name": "Synthetic Named Person", "revision": 2,
					"participant_ids": []int64{40}, "created_at": "2026-08-19T00:00:00Z",
					"updated_at": "2026-08-19T00:00:00Z",
				},
				"score": 0.61,
			},
		}}))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, AllowInsecure: true,
	}})

	t.Run("JSON null and order", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		command, stdout, stderr := newPersonSearchTestCommand(t)
		command.SetArgs([]string{"--json", "synthetic"})
		requirements.NoError(command.Execute())

		checks.JSONEq(`{"results":[
			{"id":11,"display_name":null,"score":0.82},
			{"id":4,"display_name":"Synthetic Named Person","score":0.61}
		]}`, stdout.String())
		checks.Empty(stderr.String(), "nil display-name JSON emits no progress noise")
	})

	t.Run("human dash and order", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		command, stdout, stderr := newPersonSearchTestCommand(t)
		command.SetArgs([]string{"synthetic"})
		requirements.NoError(command.Execute())

		output := stdout.String()
		unnamed := strings.Index(output, "11")
		named := strings.Index(output, "Synthetic Named Person")
		checks.GreaterOrEqual(unnamed, 0, "unnamed result")
		checks.Greater(named, unnamed, "human output preserves relevance order")
		checks.Regexp(`(?m)^11\s+-\s+0\.820000$`, output)
		checks.Regexp(`(?m)^4\s+Synthetic Named Person\s+0\.610000$`, output)
		checks.Empty(stderr.String(), "nil display-name human output emits no progress noise")
	})
}

func TestPersonSearchCommandRejectsInvalidInputBeforeDaemonRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writePersonSearchCommandResponse(t, w)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, AllowInsecure: true,
	}})

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "whitespace query", args: []string{" \t "}, want: "query must contain non-whitespace text"},
		{name: "zero limit", args: []string{"--limit", "0", "synthetic"}, want: "--limit must be between 1 and 100"},
		{name: "oversized limit", args: []string{"--limit", "101", "synthetic"}, want: "--limit must be between 1 and 100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, _, _ := newPersonSearchTestCommand(t)
			command.SetArgs(test.args)
			err := command.Execute()
			require.ErrorContains(t, err, test.want)
		})
	}
	assert.Zero(t, requests.Load(), "invalid input must not reach or start a daemon")
}

func TestPersonSearchCommandPropagatesDisabledAndStaleDaemonErrors(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
	}{
		{name: "disabled", code: "vector_not_enabled", message: "Vector search is not configured"},
		{name: "stale", code: "index_stale", message: "The vector index does not match configured embedding settings"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				assertions.NoError(json.NewEncoder(w).Encode(map[string]string{
					"error": test.code, "message": test.message,
				}))
			}))
			t.Cleanup(server.Close)
			withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
				URL: server.URL, AllowInsecure: true,
			}})

			command, _, _ := newPersonSearchTestCommand(t)
			command.SetArgs([]string{"synthetic"})
			err := command.Execute()
			requirements.Error(err)
			requirements.ErrorContains(err, "API error (503)")
			requirements.ErrorContains(err, test.message)
		})
	}
}

// TestDefaultVectorConfigBlocksCuratedPeopleButKeepsMessageEmbedding catches
// vector enablement alone authorizing curated person documents and person
// queries at the configured embedding provider.
func TestDefaultVectorConfigBlocksCuratedPeopleButKeepsMessageEmbedding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var (
		providerMu     sync.Mutex
		providerInputs []string
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		providerMu.Lock()
		providerInputs = append(providerInputs, request.Input...)
		providerMu.Unlock()
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0, 0, 0}, "index": i}
		}
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(map[string]any{
			"data": data, "model": "synthetic-default-policy-model",
		}))
	}))
	t.Cleanup(provider.Close)

	dataDir := t.TempDir()
	mainPath := filepath.Join(dataDir, "msgvault.db")
	configured := config.NewDefaultConfig()
	configured.HomeDir = dataDir
	configured.Data.DataDir = dataDir
	configured.Vector.Enabled = true
	configured.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
	configured.Vector.Embeddings.Endpoint = provider.URL + "/v1"
	configured.Vector.Embeddings.Model = "synthetic-default-policy-model"
	configured.Vector.Embeddings.Dimension = 4
	configured.Vector.Embeddings.MaxRetries = 1
	withTestConfig(t, configured)
	requirements.NoError(configured.Save())

	mainStore, err := store.Open(mainPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = mainStore.Close() })
	requirements.NoError(mainStore.InitSchema())
	_, err = mainStore.DB().Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (1, 'test', 'default-policy-source');
		INSERT INTO conversations
			(id, source_id, source_conversation_id, conversation_type)
		VALUES (1, 1, 'default-policy-conversation', 'email_thread');
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type, subject)
		VALUES (1, 1, 1, 'default-policy-message', 'email', 'Synthetic message subject');
		INSERT INTO message_bodies (message_id, body_text) VALUES (1, 'Synthetic message body')`)
	requirements.NoError(err)
	participantID, err := mainStore.EnsureParticipantByIdentifier(
		"email", "default-policy@example.test", "Observed Synthetic Name",
	)
	requirements.NoError(err)
	person, _, err := mainStore.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	displayName := "Synthetic Curated Person"
	person, err = mainStore.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, &displayName,
	)
	requirements.NoError(err)

	features, err := setupVectorFeatures(t.Context(), mainStore, mainPath, false)
	requirements.NoError(err)
	requirements.NotNil(features)
	t.Cleanup(func() { _ = features.Close() })
	generation, err := features.Backend.CreateGeneration(
		t.Context(), features.Cfg.Embeddings.Model,
		features.Cfg.Embeddings.Dimension, features.Cfg.GenerationFingerprint(),
	)
	requirements.NoError(err)
	result, err := features.Runner.RunOnce(t.Context(), generation, testCLIEmbeddingPassScope())
	requirements.NoError(err)
	assertions.Equal(1, result.Succeeded, "message embedding must continue while people are disabled")
	convergence, err := features.Convergence.CheckConvergence(t.Context(), generation)
	requirements.NoError(err)
	assertions.True(convergence.PersonCoverageComplete,
		"disabled person embeddings must not block a message generation")

	providerMu.Lock()
	inputsAfterWorker := append([]string(nil), providerInputs...)
	providerMu.Unlock()
	assertions.Equal([]string{"Subject: Synthetic message subject\n\nSynthetic message body"}, inputsAfterWorker,
		"default vector config must not send curated person data")

	requirements.NoError(features.Backend.ActivateGeneration(t.Context(), generation, false))
	_, err = features.PersonSearchEngine.Search(t.Context(), "synthetic person", 5)
	requirements.ErrorContains(err, "[vector.people] enabled = true")
	providerMu.Lock()
	assertions.Equal(inputsAfterWorker, providerInputs,
		"disabled person search must fail before query embedding")
	providerMu.Unlock()

	configured.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}
	requirements.NoError(configured.Save())
	result, err = features.Runner.RunOnce(t.Context(), generation, testCLIEmbeddingPassScope())
	requirements.NoError(err)
	assertions.Zero(result.Succeeded, "unconsented people must be skipped without blocking messages")
	_, err = features.PersonSearchEngine.Search(t.Context(), "synthetic person", 5)
	requirements.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	providerMu.Lock()
	assertions.Equal(inputsAfterWorker, providerInputs,
		"enabled but unconsented people and queries must make zero provider calls")
	providerMu.Unlock()

	semanticProfile, err := configured.Vector.SemanticPersonEmbeddingProfile()
	requirements.NoError(err)
	_, err = mainStore.EnsurePersonSemanticEmbeddingProfile(t.Context(), semanticProfile)
	requirements.NoError(err)
	_, _, err = mainStore.GrantPersonSemanticEmbeddingConsent(
		t.Context(), semanticProfile.Fingerprint, "test",
	)
	requirements.NoError(err)
	result, err = features.Runner.RunOnce(t.Context(), generation, testCLIEmbeddingPassScope())
	requirements.NoError(err)
	assertions.Equal(1, result.Succeeded, "consented exact policy must run the person worker")
	results, err := features.PersonSearchEngine.Search(t.Context(), "synthetic person", 5)
	requirements.NoError(err)
	requirements.Len(results, 1)
	assertions.Equal(person.ID, results[0].Person.ID)

	providerMu.Lock()
	providerCallsAfterConsent := len(providerInputs)
	providerMu.Unlock()
	_, err = mainStore.RevokePersonSemanticEmbeddingConsent(
		t.Context(), semanticProfile.Fingerprint, "test",
	)
	requirements.NoError(err)
	updatedName := "Synthetic Curated Person Updated"
	_, err = mainStore.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, &updatedName,
	)
	requirements.NoError(err)
	_, err = features.Runner.RunOnce(t.Context(), generation, testCLIEmbeddingPassScope())
	requirements.NoError(err)
	_, err = features.PersonSearchEngine.Search(t.Context(), "updated person", 5)
	requirements.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	providerMu.Lock()
	assertions.Len(providerInputs, providerCallsAfterConsent,
		"revocation must stop worker and query provider calls without restart")
	providerMu.Unlock()
}

// TestCurrentSemanticPersonVectorConfigSourceFailsClosedAfterConfigRemoval
// catches deletion of a live config silently falling back to the authorized
// startup snapshot.
func TestCurrentSemanticPersonVectorConfigSourceFailsClosedAfterConfigRemoval(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	configured := config.NewDefaultConfig()
	configured.HomeDir = t.TempDir()
	configured.Data.DataDir = configured.HomeDir
	configured.Vector.Enabled = true
	configured.Vector.Embeddings.Endpoint = "https://embedding.example.test/v1"
	configured.Vector.Embeddings.Model = "synthetic-config-source-model"
	configured.Vector.Embeddings.Dimension = 4
	configured.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}
	withTestConfig(t, configured)
	requirements.NoError(configured.Save())

	source := currentSemanticPersonVectorConfigSource()
	current, err := source()
	requirements.NoError(err)
	assertions.True(current.People.Enabled)
	requirements.NoError(os.Remove(configured.ConfigFilePath()))

	current, err = source()
	requirements.NoError(err)
	assertions.False(current.People.Enabled,
		"a missing live config must not reuse the authorized startup policy")

	cfg = nil
	_, err = source()
	requirements.ErrorContains(err, "configuration is unavailable",
		"an absent runtime config must fail closed instead of using the startup policy")
}

// TestCompletedBuildActivatesWithoutPersonRequestsAfterLivePolicyDrift catches
// convergence treating a fail-closed person authorization state as a fatal
// message-generation error. The runtime must skip curated-person coverage,
// keep all provider traffic fenced, and still activate the completed message
// generation under routing, credential-source, or provider-posture drift.
func TestCompletedBuildActivatesWithoutPersonRequestsAfterLivePolicyDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*vector.Config)
	}{
		{
			name: "endpoint",
			mutate: func(config *vector.Config) {
				config.Embeddings.Endpoint += "-drifted"
			},
		},
		{
			name: "api key environment",
			mutate: func(config *vector.Config) {
				config.Embeddings.APIKeyEnv = "DRIFTED_PERSON_EMBEDDING_KEY"
			},
		},
		{
			name: "provider posture",
			mutate: func(config *vector.Config) {
				config.People.TrainingPosture = "provider_may_train"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			var providerRequests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert := newAssert(t)
				providerRequests.Add(1)
				var request struct {
					Input []string `json:"input"`
				}
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				data := make([]map[string]any, len(request.Input))
				for i := range request.Input {
					data[i] = map[string]any{"embedding": []float32{1, 0}, "index": i}
				}
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{
					"data": data, "model": "synthetic-drift-model",
				}))
			}))
			t.Cleanup(provider.Close)

			dataDir := t.TempDir()
			mainPath := filepath.Join(dataDir, "msgvault.db")
			configured := config.NewDefaultConfig()
			configured.HomeDir = dataDir
			configured.Data.DataDir = dataDir
			configured.Vector.Enabled = true
			configured.Vector.DBPath = filepath.Join(dataDir, "vectors.db")
			configured.Vector.Embeddings.Endpoint = provider.URL + "/v1"
			configured.Vector.Embeddings.Model = "synthetic-drift-model"
			configured.Vector.Embeddings.APIKeyEnv = "INITIAL_PERSON_EMBEDDING_KEY"
			configured.Vector.Embeddings.Dimension = 2
			configured.Vector.Embeddings.MaxRetries = 1
			configured.Vector.People = vector.PeopleConfig{
				Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
			}
			withTestConfig(t, configured)
			require.NoError(configured.Save())

			mainStore, err := store.Open(mainPath)
			require.NoError(err)
			t.Cleanup(func() { _ = mainStore.Close() })
			require.NoError(mainStore.InitSchema())
			profile, err := configured.Vector.SemanticPersonEmbeddingProfile()
			require.NoError(err)
			_, err = mainStore.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
			require.NoError(err)
			_, _, err = mainStore.GrantPersonSemanticEmbeddingConsent(
				t.Context(), profile.Fingerprint, "test",
			)
			require.NoError(err)
			participantID, err := mainStore.EnsureParticipantByIdentifier(
				"email", "drift-person@example.test", "Observed Drift Person",
			)
			require.NoError(err)
			person, _, err := mainStore.CreatePersonFromParticipantContext(t.Context(), participantID)
			require.NoError(err)
			displayName := "Synthetic Drift Person"
			_, err = mainStore.UpdatePersonDisplayNameContext(
				t.Context(), person.ID, person.Revision, &displayName,
			)
			require.NoError(err)

			features, err := setupVectorFeatures(t.Context(), mainStore, mainPath, false)
			require.NoError(err)
			t.Cleanup(func() { _ = features.Close() })
			generation, err := features.Backend.CreateGeneration(
				t.Context(), features.Cfg.Embeddings.Model,
				features.Cfg.Embeddings.Dimension, features.Cfg.GenerationFingerprint(),
			)
			require.NoError(err)

			test.mutate(&configured.Vector)
			require.NoError(configured.Save())
			var stderr bytes.Buffer
			_, err = runEmbeddingPasses(
				t.Context(), features.Runner, generation, false,
				vector.APIFormatOpenAI, &stderr,
			)
			require.NoError(err, stderr.String())
			var stdout bytes.Buffer
			activated, err := activateBuiltGeneration(
				t.Context(), features.Backend, features.Convergence, generation,
				vector.APIFormatOpenAI, &stdout, &stderr,
			)
			require.NoError(err, stderr.String())
			assert.True(activated)
			assert.Contains(stdout.String(), "activated")
			assert.Zero(providerRequests.Load(),
				"authorization drift must fence every curated-person provider request")
		})
	}
}

// TestPersonSearchProductionCompositionDoesNotPublishReadyWithoutThePersonEngine
// catches runtime setup that marks vectors ready after installing only the
// message engine. It drives the complete production path from a curated
// profile document through the provider and SQLite person index to the
// authenticated generated client used by the CLI.
func TestPersonSearchProductionCompositionDoesNotPublishReadyWithoutThePersonEngine(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	var (
		providerMu     sync.Mutex
		providerInputs []string
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/v1/embeddings", r.URL.Path)
		var request struct {
			Input []string `json:"input"`
		}
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		providerMu.Lock()
		providerInputs = append(providerInputs, request.Input...)
		providerMu.Unlock()
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0, 0, 0}, "index": i}
		}
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(map[string]any{
			"data": data, "model": "synthetic-person-model",
		}))
	}))
	t.Cleanup(provider.Close)

	dataDir := t.TempDir()
	mainPath := filepath.Join(dataDir, "msgvault.db")
	vectorPath := filepath.Join(dataDir, "vectors.db")
	configured := config.NewDefaultConfig()
	configured.HomeDir = dataDir
	configured.Data.DataDir = dataDir
	configured.Server.APIKey = "synthetic-person-search-key"
	configured.Vector.Enabled = true
	configured.Vector.DBPath = vectorPath
	configured.Vector.Embeddings.Endpoint = provider.URL + "/v1"
	configured.Vector.Embeddings.Model = "synthetic-person-model"
	configured.Vector.Embeddings.Dimension = 4
	configured.Vector.Embeddings.MaxRetries = 1
	configured.Vector.People = vector.PeopleConfig{
		Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
	}
	withTestConfig(t, configured)
	requirements.NoError(configured.Save())
	savedUseLocal := useLocal
	useLocal = false
	t.Cleanup(func() { useLocal = savedUseLocal })

	mainStore, err := store.Open(mainPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = mainStore.Close() })
	requirements.NoError(mainStore.InitSchema())
	semanticProfile, err := configured.Vector.SemanticPersonEmbeddingProfile()
	requirements.NoError(err)
	_, err = mainStore.EnsurePersonSemanticEmbeddingProfile(t.Context(), semanticProfile)
	requirements.NoError(err)
	_, _, err = mainStore.GrantPersonSemanticEmbeddingConsent(
		t.Context(), semanticProfile.Fingerprint, "test",
	)
	requirements.NoError(err)
	participantID, err := mainStore.EnsureParticipantByIdentifier(
		"email", "synthetic-architect@example.test", "Observed Synthetic Name",
	)
	requirements.NoError(err)
	person, created, err := mainStore.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	requirements.True(created)
	displayName := "Synthetic Architect"
	person, err = mainStore.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, &displayName,
	)
	requirements.NoError(err)
	document, err := mainStore.LoadPersonSemanticDocumentContext(t.Context(), person.ID)
	requirements.NoError(err)
	requirements.Contains(document.Text, displayName)
	requirements.NotContains(document.Text, "synthetic-architect@example.test")

	features, err := setupVectorFeatures(t.Context(), mainStore, mainPath, false)
	requirements.NoError(err)
	requirements.NotNil(features)
	t.Cleanup(func() { _ = features.Close() })
	requirements.NotNil(features.PersonSearchEngine,
		"runtime composition must build the concrete person engine")
	generation, err := features.Backend.CreateGeneration(
		t.Context(), features.Cfg.Embeddings.Model,
		features.Cfg.Embeddings.Dimension, features.Cfg.GenerationFingerprint(),
	)
	requirements.NoError(err)
	requirements.NoError(features.Backend.ActivateGeneration(t.Context(), generation, false),
		"a pre-feature active generation can exist without person coverage")

	apiServer := api.NewServerWithOptions(api.ServerOptions{
		Config: configured, Store: &storeAPIAdapter{store: mainStore},
		Logger: slog.New(slog.DiscardHandler), VectorStatus: api.VectorStatusInitializing,
	})
	apiServer.SetVectorFeatures(
		features.HybridEngine, features.PersonSearchEngine, features.Backend, features.Cfg,
	)
	httpServer := httptest.NewServer(apiServer.Router())
	t.Cleanup(httpServer.Close)

	unauthorizedRequest, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, httpServer.URL+"/api/v1/people/search",
		strings.NewReader(`{"query":"architect"}`),
	)
	requirements.NoError(err)
	unauthorizedRequest.Header.Set("Content-Type", "application/json")
	unauthorizedResponse, err := http.DefaultClient.Do(unauthorizedRequest)
	requirements.NoError(err)
	_ = unauthorizedResponse.Body.Close()
	assertions.Equal(http.StatusUnauthorized, unauthorizedResponse.StatusCode,
		"semantic person search route remains protected")

	unindexedRequest, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, httpServer.URL+"/api/v1/people/search",
		strings.NewReader(`{"query":"architect"}`),
	)
	requirements.NoError(err)
	unindexedRequest.Header.Set("Content-Type", "application/json")
	unindexedRequest.Header.Set("X-Api-Key", configured.Server.APIKey)
	unindexedResponse, err := http.DefaultClient.Do(unindexedRequest)
	requirements.NoError(err)
	unindexedBody, err := io.ReadAll(unindexedResponse.Body)
	requirements.NoError(err)
	requirements.NoError(unindexedResponse.Body.Close())
	assertions.Equal(http.StatusServiceUnavailable, unindexedResponse.StatusCode)
	assertions.Contains(string(unindexedBody), `"error":"index_building"`)
	assertions.Contains(string(unindexedBody), "msgvault embeddings resume --backstop",
		"an upgraded corpus must tell the operator how to make progress")
	providerMu.Lock()
	assertions.Empty(providerInputs,
		"an unindexed upgraded person corpus must not incur a query provider call")
	providerMu.Unlock()

	result, err := features.Runner.RunOnce(t.Context(), generation, testCLIEmbeddingPassScope())
	requirements.NoError(err)
	assertions.Equal(1, result.Succeeded, "one curated person document embedded")

	configured.Remote = config.RemoteConfig{
		URL: httpServer.URL, APIKey: configured.Server.APIKey, AllowInsecure: true,
	}
	command, stdout, stderr := newPersonSearchTestCommand(t)
	command.SetArgs([]string{"--json", "architect"})
	requirements.NoError(command.Execute())
	assertions.JSONEq(`{"results":[{"id":`+
		jsonInt(person.ID)+`,"display_name":"Synthetic Architect","score":1}]}`, stdout.String())
	assertions.Empty(stderr.String())

	providerMu.Lock()
	inputs := append([]string(nil), providerInputs...)
	providerMu.Unlock()
	assertions.Equal([]string{document.Text, "architect"}, inputs,
		"canonical document and free-text query use the same configured provider")
}

func newPersonSearchTestCommand(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	savedLimit, savedJSON := personSearchLimit, personSearchJSON
	personSearchLimit, personSearchJSON = defaultPersonSearchLimit, false
	t.Cleanup(func() {
		personSearchLimit, personSearchJSON = savedLimit, savedJSON
	})
	command := &cobra.Command{
		Use: personSearchCmd.Use, Args: personSearchCmd.Args, RunE: personSearchCmd.RunE,
	}
	command.Flags().IntVar(&personSearchLimit, "limit", defaultPersonSearchLimit, "")
	command.Flags().BoolVar(&personSearchJSON, flagJSON, false, "")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetContext(context.Background())
	return command, stdout, stderr
}

func TestPersonSearchProductionCommandRegistrationAndFlags(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	command, remaining, err := personCmd.Find([]string{"search"})
	must.NoError(err)
	check.Same(personSearchCmd, command)
	check.Empty(remaining)
	limitFlag := command.Flags().Lookup("limit")
	must.NotNil(limitFlag)
	check.Equal(strconv.Itoa(defaultPersonSearchLimit), limitFlag.DefValue)
	check.NotNil(command.Flags().Lookup(flagJSON))
}

func writePersonSearchCommandResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
		{
			"person": map[string]any{
				"id": 9, "vcard_uid": "00000000-0000-0000-0000-000000000009",
				"display_name": "Synthetic Architect", "revision": 4,
				"participant_ids": []int64{90}, "created_at": "2026-08-20T00:00:00Z",
				"updated_at": "2026-08-20T01:00:00Z",
			},
			"score": 0.91,
		},
		{
			"person": map[string]any{
				"id": 3, "vcard_uid": "00000000-0000-0000-0000-000000000003",
				"display_name": "Test Researcher", "revision": 2,
				"participant_ids": []int64{30}, "created_at": "2026-08-19T00:00:00Z",
				"updated_at": "2026-08-19T01:00:00Z",
			},
			"score": 0.73,
		},
	}}))
}

func jsonInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
