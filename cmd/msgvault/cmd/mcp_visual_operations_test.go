package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestMCPVisualWriteFlagsDefaultOff(t *testing.T) {
	for _, prefix := range []string{"", "mcp-"} {
		t.Run(prefix, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			command := mcpCmd
			if prefix != "" {
				command = &cobra.Command{}
				addMCPWriteFlags(command, prefix)
			} else {
				saved := mcpAllowVisualWrites
				flag := command.Flags().Lookup("allow-visual-writes")
				require.NotNil(flag)
				changed := flag.Changed
				t.Cleanup(func() { mcpAllowVisualWrites = saved; flag.Changed = changed })
				mcpAllowVisualWrites = false
			}
			var opts mcpserver.ServeOptions
			applyMCPWriteFlags(command, prefix, &opts)
			assert.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyVisual)
			require.NoError(command.Flags().Set(prefix+"allow-visual-writes", "true"))
			opts = mcpserver.ServeOptions{}
			applyMCPWriteFlags(command, prefix, &opts)
			assert.Contains(opts.OperationWriteFamilies, mcpserver.OperationFamilyVisual)
		})
	}
}

func TestMCPVisualNativeOperationsAndApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{Fingerprint: "synthetic", Model: "fixture", Dimension: 1024})
	require.NoError(err)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)})
	var builds, resumes, retries, retires int
	var ownerID int64
	var hash string
	fixture.SetVisualOperations(func(ctx context.Context, _ operations.PassScope) error {
		builds++
		return st.ConsentVisualGeneration(ctx, generation.ID, "fixture-policy")
	},
		func(context.Context, operations.PassScope) error { resumes++; return nil },
		func(_ context.Context, _ operations.PassScope, id int64, h string) error {
			retries++
			ownerID, hash = id, h
			return nil
		},
		func(ctx context.Context, _ bool) (visual.Status, error) {
			current, err := st.GetVisualGeneration(ctx, generation.ID)
			return visual.Status{Generation: current, Formats: []visual.FormatCoverage{}}, err
		},
		func(ctx context.Context) error { retires++; return st.RetireVisualGeneration(ctx, generation.ID) })
	backend := sourceOperationFixture(t, fixture)
	assert.Subset(backend.capabilities(), []string{"get_visual_attachment_status", "start_visual_attachment_build", "resume_visual_attachment_build", "retry_visual_attachment_owner", "retire_visual_attachment_generation"})
	read, err := backend.ExecuteOperation(t.Context(), "get_visual_attachment_status", nil)
	require.NoError(err)
	require.False(read.IsError)
	encoded, err := json.Marshal(read.Output)
	require.NoError(err)
	assert.NotContains(string(encoded), "synthetic")
	assert.NotContains(string(encoded), "fingerprint")
	approved := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyVisual}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		if approved {
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		}
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "start_visual_attachment_build", Arguments: map[string]any{"consent": true}})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(0, builds)
	approved = true
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "start_visual_attachment_build", Arguments: map[string]any{"consent": true}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(1, builds)
	current, err := st.GetVisualGeneration(t.Context(), generation.ID)
	require.NoError(err)
	assert.True(current.Consented)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "resume_visual_attachment_build", Arguments: map[string]any{}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(1, resumes)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retry_visual_attachment_owner", Arguments: map[string]any{"message_id": 42, "blob_hash": "ABCD"}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(1, retries)
	assert.Equal(int64(42), ownerID)
	assert.Equal("abcd", hash)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retire_visual_attachment_generation", Arguments: map[string]any{"generation_id": generation.ID + 1}})
	require.NoError(err)
	require.True(result.IsError)
	assert.Equal(0, retires)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	assert.Contains(string(data), "visual_generation_changed")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retire_visual_attachment_generation", Arguments: map[string]any{"generation_id": generation.ID}})
	require.NoError(err)
	require.False(result.IsError)
	current, err = st.GetVisualGeneration(t.Context(), generation.ID)
	require.NoError(err)
	assert.Equal(store.VisualGenerationRetired, current.State)
	assert.Equal(1, retires)
}

func TestMCPVisualMutationFailureIsUncertainAndRedacted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler)})
	fixture.SetVisualOperations(nil, func(context.Context, operations.PassScope) error {
		return errors.New("synthetic private diagnostic")
	}, nil, func(context.Context, bool) (visual.Status, error) { return visual.Status{}, nil }, nil)
	backend := sourceOperationFixture(t, fixture)
	result, err := backend.ExecuteOperation(t.Context(), "resume_visual_attachment_build", nil)
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(data), `"operation_may_have_completed":true`)
	assert.NotContains(string(data), "synthetic private diagnostic")
}

func TestMCPVisualDiscoveryRequiresNativeFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "retryVisualAttachmentOwner" {
			capabilities.Routes[i].RequestProperties = nil
		}
	}
	assert.NotContains(newDaemonMCPOperations(backend.client, capabilities).capabilities(), "retry_visual_attachment_owner")
	result, err := backend.ExecuteOperation(t.Context(), "get_visual_attachment_status", map[string]any{"coverage": true})
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(data), "operation_not_supported")
}

func TestMCPVisualCoverageRequiresAdvertisedQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler)})
	coverage := false
	fixture.SetVisualOperations(nil, nil, nil, func(_ context.Context, requested bool) (visual.Status, error) {
		coverage = requested
		return visual.Status{Formats: []visual.FormatCoverage{}}, nil
	}, nil)
	backend := sourceOperationFixture(t, fixture)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "getVisualAttachmentStatus" {
			capabilities.Routes[i].QueryParameters = append(capabilities.Routes[i].QueryParameters, "coverage")
		}
	}
	backend = newDaemonMCPOperations(backend.client, capabilities)
	result, err := backend.ExecuteOperation(t.Context(), "get_visual_attachment_status", map[string]any{"coverage": true})
	require.NoError(err)
	require.False(result.IsError)
	assert.True(coverage)
}
