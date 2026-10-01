//go:build sqlite_vec

package cmd

import (
	"bytes"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

func TestEmbeddingsListJSONUsesActualGenerationMetadataAndCoverage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, mainPath := mcpSQLiteStoreWithPath(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DatabaseURL = mainPath
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.Embeddings.Model = "synthetic-model"
	cfg.Vector.Embeddings.Dimension = 4
	source, err := st.GetOrCreateSource("gmail", "generation@example.com")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
	requirements.NoError(err)
	var messageIDs []int64
	for _, name := range []string{"embedded", "blank", "missing"} {
		id, err := st.UpsertMessage(&store.Message{ConversationID: conversationID, SourceID: source.ID, SourceMessageID: name, MessageType: "email"})
		requirements.NoError(err)
		messageIDs = append(messageIDs, id)
	}
	cfg.Vector.Embed.Scope.Accounts = []string{source.Identifier}
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: mainPath, MainDB: st.DB(), Dimension: 4})
	requirements.NoError(err)
	t.Cleanup(func() { _ = backend.Close() })
	for _, create := range []bool{false, true} {
		if create {
			id, err := backend.CreateGeneration(t.Context(), cfg.Vector.Embeddings.Model, 4, cfg.Vector.GenerationFingerprint())
			requirements.NoError(err)
			requirements.NoError(backend.Upsert(t.Context(), id, []vector.Chunk{{MessageID: messageIDs[0], Vector: []float32{1, 0, 0, 0}}}))
			requirements.NoError(st.SetEmbedGen(t.Context(), messageIDs[:2], int64(id)))
			_, err = backend.DB().ExecContext(t.Context(), `INSERT INTO vector_accelerators
				(generation_id, kind, state, table_name, dimension, indexed_count, last_embedding_id, source_revision, model_config, vec1_version, started_at, last_error)
				VALUES (?, 'vec1_ivf_opq', 'building', 'synthetic_table', 4, 0, 0, 0, '{}', 'synthetic', 1700000000, ?)`, id, "synthetic-private-accelerator-diagnostic")
			requirements.NoError(err)
		}
		// Exercise the owning handler with a real parsed JSON flag and genuine
		// archive/vector stores, without parsing its human presentation table.
		command := &cobra.Command{Use: "list"}
		command.Flags().Bool(flagJSON, true, "structured output")
		command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
		var output bytes.Buffer
		command.SetOut(&output)
		requirements.NoError(runEmbeddingsList(command, nil))
		var report struct {
			Fingerprint string  `json:"configured_generation_fingerprint"`
			SourceIDs   []int64 `json:"source_ids"`
			Generations []struct {
				ID          int64  `json:"id"`
				Model       string `json:"model"`
				State       string `json:"state"`
				Live        int64  `json:"live_count"`
				Embedded    int64  `json:"embedded_count"`
				Blank       int64  `json:"blank_count"`
				Missing     int64  `json:"missing_count"`
				Accelerator struct {
					HasError bool `json:"has_error"`
				} `json:"accelerator"`
			} `json:"generations"`
		}
		requirements.NoError(json.Unmarshal(output.Bytes(), &report), "the real --json path must emit one typed object even with no generations")
		assertions.Equal(cfg.Vector.GenerationFingerprint(), report.Fingerprint)
		assertions.NotContains(output.String(), cfg.Vector.DBPath)
		assertions.NotContains(output.String(), mainPath)
		assertions.NotContains(output.String(), "synthetic-private-accelerator-diagnostic")
		assertions.Equal([]int64{source.ID}, report.SourceIDs)
		if create {
			requirements.Len(report.Generations, 1)
			row := report.Generations[0]
			assertions.Equal("building", row.State)
			assertions.Equal(cfg.Vector.Embeddings.Model, row.Model)
			assertions.Equal(row.Live, row.Embedded+row.Blank+row.Missing)
			assertions.Equal(int64(3), row.Live)
			assertions.Equal(int64(1), row.Embedded)
			assertions.Equal(int64(1), row.Blank)
			assertions.Equal(int64(1), row.Missing)
			assertions.True(row.Accelerator.HasError)
		} else {
			assertions.NotNil(report.Generations)
			assertions.Empty(report.Generations)
		}
	}
}

func TestEmbeddingsListJSONDiscoveryUsesTheRegisteredFlag(t *testing.T) {
	requirements := require.New(t)
	for _, descriptor := range registeredMCPCommandDescriptors() {
		if descriptor.Name == "embeddings list" {
			requirements.Contains(descriptor.Flags, flagJSON)
			requirements.False(descriptor.Delegated)
			return
		}
	}
	requirements.Fail("the actual embeddings list command must advertise its structured contract")
}
