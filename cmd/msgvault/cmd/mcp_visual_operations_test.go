package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
	assert.NotContains(string(encoded), `"eligible"`)
	assert.NotContains(string(encoded), `"unknown_role"`)
	assert.NotContains(string(encoded), `"unavailable"`)
	assert.NotContains(string(encoded), `"formats"`)
	var prompt string
	approved := true
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyVisual}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		prompt = request.Params.Message
		if !approved {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(t *testing.T, name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		require := require.New(t)
		assert := assert.New(t)
		want, handled, err := backend.visualOperationDisclosure(name, args)
		require.NoError(err)
		require.True(handled)
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(err)
		assert.Equal(want, prompt)
		if name == "retire_visual_attachment_generation" {
			assert.Contains(prompt, "Irreversibly")
			assert.Contains(prompt, "delete its vectors")
		} else {
			assert.Contains(prompt, "eligible attachment bytes and bounded message content")
			assert.Contains(prompt, "configured provider")
			assert.Contains(prompt, "consent")
			assert.Contains(prompt, "charges may apply")
		}
		return result
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"start_visual_attachment_build", map[string]any{"consent": false}, "invalid_arguments"},
		{"retry_visual_attachment_owner", map[string]any{"message_id": 42, "blob_hash": strings.Repeat("z", 64)}, "validating"},
		{"retire_visual_attachment_generation", map[string]any{"generation_id": 0}, "validating"},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(err)
		require.NotNil(result)
		assert.True(result.IsError)
		data, err := json.Marshal(result)
		require.NoError(err)
		assert.Contains(string(data), tc.want)
	}
	assert.Zero(builds)
	assert.Zero(retries)
	assert.Zero(retires)
	approved = false
	declined, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "start_visual_attachment_build", Arguments: map[string]any{"consent": true}})
	require.NoError(err)
	require.True(declined.IsError)
	declinedData, err := json.Marshal(declined.Content)
	require.NoError(err)
	assert.Contains(string(declinedData), "action cancelled")
	assert.Zero(builds)
	approved = true
	t.Run("build", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		result := call(t, "start_visual_attachment_build", map[string]any{"consent": true})
		require.False(result.IsError)
		assert.Equal(1, builds)
		current, err := st.GetVisualGeneration(t.Context(), generation.ID)
		require.NoError(err)
		assert.True(current.Consented)
	})
	t.Run("resume", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		result := call(t, "resume_visual_attachment_build", map[string]any{})
		require.False(result.IsError)
		assert.Equal(1, resumes)
	})
	t.Run("retry", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		result := call(t, "retry_visual_attachment_owner", map[string]any{"message_id": 42, "blob_hash": strings.Repeat("AB", 32)})
		require.False(result.IsError)
		assert.Equal(1, retries)
		assert.Equal(int64(42), ownerID)
		assert.Equal(strings.Repeat("ab", 32), hash)
	})
	t.Run("retire", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		result := call(t, "retire_visual_attachment_generation", map[string]any{"generation_id": generation.ID + 1})
		require.True(result.IsError)
		assert.Equal(0, retires)
		data, err := json.Marshal(result.StructuredContent)
		require.NoError(err)
		assert.Contains(string(data), "visual_generation_changed")
		assert.Contains(string(data), `"operation_may_have_completed":false`)
		result = call(t, "retire_visual_attachment_generation", map[string]any{"generation_id": generation.ID})
		require.False(result.IsError)
		current, err := st.GetVisualGeneration(t.Context(), generation.ID)
		require.NoError(err)
		assert.Equal(store.VisualGenerationRetired, current.State)
		assert.Equal(1, retires)
	})
}

func TestMCPVisualMutationFailureIsUncertainAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     map[string]any
		failsRun bool
	}{
		{"resume_visual_attachment_build", nil, true},
		{"retire_visual_attachment_generation", map[string]any{"generation_id": 1}, true},
		{"start_visual_attachment_build", map[string]any{"consent": true}, false},
		{"retry_visual_attachment_owner", map[string]any{"message_id": 42, "blob_hash": strings.Repeat("ab", 32)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			failure := errors.New("synthetic private diagnostic")
			calls := 0
			run := func(context.Context, operations.PassScope) error {
				calls++
				if tc.failsRun {
					return failure
				}
				return nil
			}
			fixture.SetVisualOperations(run, run,
				func(ctx context.Context, scope operations.PassScope, _ int64, _ string) error { return run(ctx, scope) },
				func(context.Context, bool) (visual.Status, error) {
					if !tc.failsRun {
						return visual.Status{}, failure
					}
					return visual.Status{Generation: store.VisualGeneration{ID: 1}}, nil
				},
				func(context.Context) error { calls++; return failure })
			backend := sourceOperationFixture(t, fixture)
			result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
			require.NoError(t, err)
			require.True(t, result.IsError)
			assert.Equal(t, 1, calls)
			data, err := json.Marshal(result.Output)
			require.NoError(t, err)
			assert.Contains(t, string(data), `"operation_may_have_completed":true`)
			assert.NotContains(t, string(data), "synthetic private diagnostic")
		})
	}
}

func TestMCPVisualDiscoveryRequiresNativeFields(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	for _, tc := range []struct{ name, id, field string }{
		{"get_visual_attachment_status", "getVisualAttachmentStatus", ""},
		{"start_visual_attachment_build", "startVisualAttachmentBuild", "consent"},
		{"resume_visual_attachment_build", "resumeVisualAttachmentBuild", ""},
		{"retry_visual_attachment_owner", "retryVisualAttachmentOwner", "message_id"},
		{"retry_visual_attachment_owner", "retryVisualAttachmentOwner", "blob_hash"},
		{"retire_visual_attachment_generation", "retireVisualAttachmentGeneration", "generation_id"},
	} {
		t.Run(tc.name+tc.field, func(t *testing.T) {
			for _, missing := range []string{"method", "path", "field"} {
				if missing == "field" && tc.field == "" {
					continue
				}
				caps, err := backend.client.MCPCapabilities(t.Context())
				require.NoError(t, err)
				for i := range caps.Routes {
					route := &caps.Routes[i]
					if route.OperationID != tc.id {
						continue
					}
					switch missing {
					case "method":
						route.Method = "DELETE"
					case "path":
						route.Path = "/missing"
					case "field":
						for j, property := range route.RequestProperties {
							if property == tc.field {
								route.RequestProperties = append(route.RequestProperties[:j], route.RequestProperties[j+1:]...)
								break
							}
						}
					}
				}
				assert.NotContains(t, newDaemonMCPOperations(backend.client, caps).capabilities(), tc.name)
			}
		})
	}
}

func TestMCPVisualCoverageRequiresAdvertisedQuery(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		t.Run(map[bool]string{false: "current daemon", true: "future daemon"}[advertised], func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			fixture := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			called, coverage := false, false
			fixture.SetVisualOperations(nil, nil, nil, func(_ context.Context, requested bool) (visual.Status, error) {
				called, coverage = true, requested
				return visual.Status{Formats: []visual.FormatCoverage{}, DuplicateCost: visual.DuplicateCostRisk{AtLeastOnce: true, Detail: "private diagnostic"}}, nil
			}, nil)
			backend := sourceOperationFixture(t, fixture)
			if advertised {
				caps, err := backend.client.MCPCapabilities(t.Context())
				require.NoError(t, err)
				for i := range caps.Routes {
					if caps.Routes[i].OperationID == "getVisualAttachmentStatus" {
						caps.Routes[i].QueryParameters = append(caps.Routes[i].QueryParameters, "coverage")
					}
				}
				backend = newDaemonMCPOperations(backend.client, caps)
			}
			result, err := backend.ExecuteOperation(t.Context(), "get_visual_attachment_status", map[string]any{"coverage": true})
			require.NoError(t, err)
			data, err := json.Marshal(result.Output)
			require.NoError(t, err)
			if !advertised {
				assert.True(t, result.IsError)
				assert.Contains(t, string(data), "operation_not_supported")
				assert.False(t, called)
				return
			}
			assert.False(t, result.IsError)
			assert.True(t, called)
			assert.True(t, coverage)
			assert.Contains(t, string(data), `"eligible":0`)
			assert.Contains(t, string(data), `"at_least_once":true`)
			assert.NotContains(t, string(data), "private diagnostic")
		})
	}
}
