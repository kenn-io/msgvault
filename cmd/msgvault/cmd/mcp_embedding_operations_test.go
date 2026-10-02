//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// Run the owning Cobra command against actual archive/vector metadata over the
// real daemon stream. No primary command or its database behavior is stubbed.
type inProcessEmbeddingDaemonStore struct {
	*storeAPIAdapter

	state *invocation
}

func (s *inProcessEmbeddingDaemonStore) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	group := &cobra.Command{Use: embeddingsCommandName}
	group.AddCommand(newEmbeddingsListCommand())
	root.AddCommand(group)
	root.SetArgs(req.Args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(withInvocation(ctx, s.state))
	for _, event := range []api.CLIRunEvent{{Type: cliStreamStdout, Data: stdout.String()}, {Type: cliStreamStderr, Data: stderr.String()}} {
		if event.Data != "" {
			if emitErr := emit(event); emitErr != nil {
				return emitErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("execute production generation command: %w", err)
	}
	return nil
}

func TestMCPEmbeddingGenerationReadUsesActualOwnerCLIAndSchema(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	markDaemonCLISubprocessForTest(t)
	st, mainPath := mcpSQLiteStoreWithPath(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DatabaseURL = mainPath
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(t.TempDir(), "vectors.db")
	cfg.Vector.Embeddings.Model = "synthetic-model"
	cfg.Vector.Embeddings.Dimension = 4
	vectorBackend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: cfg.Vector.DBPath, MainPath: mainPath, MainDB: st.DB(), Dimension: 4})
	requirements.NoError(err)
	t.Cleanup(func() { _ = vectorBackend.Close() })
	id, err := vectorBackend.CreateGeneration(t.Context(), cfg.Vector.Embeddings.Model, 4, cfg.Vector.GenerationFingerprint())
	requirements.NoError(err)
	daemon := &inProcessEmbeddingDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: st, mcpCommands: registeredMCPCommandDescriptors()}, state: invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemon, Logger: slog.New(slog.DiscardHandler)}))
	requirements.Contains(backend.capabilities(), "list_embedding_generations")
	result, err := backend.ExecuteOperation(t.Context(), "list_embedding_generations", nil)
	requirements.NoError(err)
	report := operationOutput[vector.GenerationStatusReport](t, result)
	requirements.Len(report.Generations, 1)
	assertions.Equal(id, report.Generations[0].ID)
	assertions.Equal(vector.GenerationBuilding, report.Generations[0].State)
	assertions.True(report.Generations[0].CoverageAvailable)
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_embedding_generations"})
	requirements.NoError(err)
	assertions.False(called.IsError)
	assertions.NotNil(called.StructuredContent)
	requirements.NoError(vectorBackend.RetireGeneration(t.Context(), id, false))
	result, err = backend.ExecuteOperation(t.Context(), "list_embedding_generations", nil)
	requirements.NoError(err)
	report = operationOutput[vector.GenerationStatusReport](t, result)
	requirements.Len(report.Generations, 1)
	assertions.Equal(vector.GenerationRetired, report.Generations[0].State)
	assertions.False(report.Generations[0].CoverageAvailable, "a skipped retired scan cannot claim current live coverage")
	for _, args := range []map[string]any{{"path": "untrusted"}, {"env": "untrusted"}, {"limit": nil}} {
		result, err := backend.ExecuteOperation(t.Context(), "list_embedding_generations", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range capabilities.Commands {
		if capabilities.Commands[i].Name == "embeddings list" {
			capabilities.Commands[i].Flags = nil
		}
	}
	assertions.NotContains(newDaemonMCPOperations(backend.client, capabilities).capabilities(), "list_embedding_generations")
	capabilities.Delegated = true
	assertions.NotContains(newDaemonMCPOperations(backend.client, capabilities).capabilities(), "list_embedding_generations")
}
