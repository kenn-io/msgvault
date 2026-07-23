//go:build fts5 && sqlite_vec

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestSearchCoverageRealGenerationStateMatrix(t *testing.T) {
	vectorCfg := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Model: "test-model", Dimension: 2, MaxInputChars: 1000,
		},
	}
	tests := []struct {
		name         string
		generation   string
		vectorStatus VectorStatus
		wantStatus   SearchCoverageStatus
		wantEligible int64
		wantEmbedded int64
		wantActions  []SearchCoverageAction
	}{
		{
			name: "no generation", wantStatus: SearchCoverageUnavailable,
			wantActions: []SearchCoverageAction{SearchCoverageActionRetry, SearchCoverageActionBuildIndex},
		},
		{
			name: "matching first build", generation: "building-matching",
			vectorStatus: VectorStatusInitializing,
			wantStatus:   SearchCoverageInitializing,
		},
		{
			name: "different first build", generation: "building-different",
			vectorStatus: VectorStatusInitializing,
			wantStatus:   SearchCoverageUnavailable,
			wantActions:  []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "matching active", generation: "active-matching",
			wantStatus: SearchCoverageReady, wantEligible: 2, wantEmbedded: 2,
		},
		{
			name: "stale active", generation: "active-different",
			wantStatus: SearchCoverageStale, wantEligible: 2, wantEmbedded: 2,
			wantActions: []SearchCoverageAction{SearchCoverageActionBuildIndex},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vectorStatus := tt.vectorStatus
			if vectorStatus == "" {
				vectorStatus = VectorStatusReady
			}
			backend := newRealCoverageBackend(t, vectorCfg, tt.generation)
			cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: vectorCfg}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: newExploreDuckDBFixture(t),
				Backend: backend, VectorCfg: vectorCfg, VectorStatus: vectorStatus, Logger: testLogger(),
			})

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{
				"filters":[{"dimension":"source","values":["1"]}]
			}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var body SearchCoverageResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, tt.wantStatus, body.Status)
			assert.Equal(t, tt.wantEligible, body.EligibleCount)
			assert.Equal(t, tt.wantEmbedded, body.EmbeddedCount)
			assert.ElementsMatch(t, tt.wantActions, body.Actions)
		})
	}
}

func newRealCoverageBackend(t *testing.T, cfg vector.Config, generationState string) *sqlitevec.Backend {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "coverage@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "coverage", "email_thread", "Coverage")
	require.NoError(t, err)
	for _, sourceMessageID := range []string{"one", "two"} {
		_, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: sourceMessageID,
			ConversationID: conversationID, MessageType: "email",
			Subject: sql.NullString{String: sourceMessageID, Valid: true},
		})
		require.NoError(t, err)
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 2, MainDB: st.DB(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	if generationState == "" {
		return backend
	}
	fingerprint := cfg.GenerationFingerprint()
	if generationState == "building-different" || generationState == "active-different" {
		fingerprint = "different"
	}
	generationID, err := backend.CreateGeneration(ctx, "test-model", 2, fingerprint)
	require.NoError(t, err)
	if generationState == "building-matching" || generationState == "building-different" {
		return backend
	}
	require.NoError(t, backend.Upsert(ctx, generationID, []vector.Chunk{
		{MessageID: 1, Vector: []float32{1, 0}},
		{MessageID: 2, Vector: []float32{0, 1}},
	}))
	require.NoError(t, st.SetEmbedGen(ctx, []int64{1, 2}, int64(generationID)))
	require.NoError(t, backend.ActivateGeneration(ctx, generationID, true))
	return backend
}
