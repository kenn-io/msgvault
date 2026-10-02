//go:build sqlite_vec

package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPVisualOperationsUseActualRuntimeAndRefusePolicyChangeAfterApproval(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, path := mcpSQLiteStoreWithPath(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Vector.Multimodal.Enabled = true
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{Model: cfg.Vector.Multimodal.Model, Dimension: cfg.Vector.Multimodal.Dimension, Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true}})
	requirements.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	requirements.NoError(err)
	cfg.Vector.Multimodal.CapabilitiesFile = filepath.Join(cfg.HomeDir, "capabilities.json")
	requirements.NoError(writeVisualCapabilityManifest(cfg.Vector.Multimodal.CapabilitiesFile, manifest))
	requirements.NoError(cfg.Save())
	vectorBackend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: path, MainDB: st.DB(), Dimension: 4})
	requirements.NoError(err)
	t.Cleanup(func() { _ = vectorBackend.Close() })
	var providerRequests atomic.Int64
	httpClient := &http.Client{Transport: visualCredentialRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerRequests.Add(1)
		return nil, errors.New("unexpected provider request in empty-archive fixture")
	})}
	vf, err := newVisualRuntime(testInvocationContext(t.Context(), cfg, invocationOptions{}), cfg.Vector, st, vectorBackend, unavailableVisualCredentialOpener{}, visualRuntimeCredential{APIKey: "synthetic-key", HTTPClient: httpClient})
	requirements.NoError(err)
	vf.GuardPolicyCheck = visualRuntimePolicyGuard(st, cfg, vf)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, LaneReadinessReader: newDaemonLaneReadinessReader(cfg, st), OperationHistoryReader: st, Logger: slog.New(slog.DiscardHandler)})
	installVisualOperations(srv, vf)
	backend := sourceOperationFixture(t, srv)
	for _, name := range []string{"get_visual_index_status", "build_visual_index", "resume_visual_index", "retry_visual_attachment", "retire_visual_generation", "get_document_vector_status"} {
		requirements.Contains(backend.capabilities(), name)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_visual_index_status", nil)
	requirements.NoError(err)
	requirements.False(result.IsError)
	wire, err := json.Marshal(result.Output)
	requirements.NoError(err)
	var output struct {
		Status generated.Status               `json:"status"`
		Policy *generated.VisualRuntimePolicy `json:"current_policy"`
	}
	requirements.NoError(json.Unmarshal(wire, &output))
	requirements.NotNil(output.Policy)
	assertions.False(output.Status.Generation.Consented)
	readiness, err := backend.ExecuteOperation(t.Context(), "get_lane_readiness", nil)
	requirements.NoError(err)
	requirements.False(readiness.IsError)
	initialized := operationOutput[generated.LaneReadinessResponse](t, readiness)
	visualInitialized := false
	for _, lane := range initialized.Lanes {
		if string(lane.Lane) == laneVisualSearch {
			visualInitialized = lane.Initialized
		}
	}
	assertions.True(visualInitialized, "readiness sees the actually installed visual runtime")
	assertions.Equal(vf.PolicyFingerprint, output.Policy.CurrentPolicyFingerprint)
	assertions.NotContains(string(wire), cfg.Vector.Multimodal.CapabilitiesFile)
	assertions.NotContains(string(wire), "synthetic-key")
	args := map[string]any{"expected_generation_id": vf.Generation.ID, "expected_generation_fingerprint": vf.Generation.Fingerprint, "expected_policy_fingerprint": vf.PolicyFingerprint}
	approvalCount := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyVisual}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, vf.PolicyFingerprint)
		assertions.Contains(request.Params.Message, `"max_owners_per_pass":2`)
		assertions.Contains(request.Params.Message, `"retention_posture":"unspecified"`)
		approvalCount++
		if approvalCount == 1 {
			cfg.Vector.Multimodal.MaxContextChars++
			requirements.NoError(cfg.Save())
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_visual_index", Arguments: args})
	requirements.NoError(err)
	requirements.True(called.IsError)
	wire, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(wire), "visual_policy_changed")
	generation, err := st.GetVisualGeneration(t.Context(), vf.Generation.ID)
	requirements.NoError(err)
	assertions.False(generation.Consented, "approval against changed operator authority cannot grant fresh consent")
	rows, err := st.ListRuns(t.Context(), operations.Query{Kinds: []operations.Kind{operations.KindVisualEmbedding}, Limit: 25})
	requirements.NoError(err)
	assertions.Empty(rows.Runs, "refusal must precede even durable pass admission")
	cfg.Vector.Multimodal.MaxContextChars--
	requirements.NoError(cfg.Save())
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_visual_index", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError)
	generation, err = st.GetVisualGeneration(t.Context(), vf.Generation.ID)
	requirements.NoError(err)
	assertions.True(generation.Consented)
	assertions.Equal(vf.PolicyFingerprint, generation.ConsentPolicyFingerprint)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "resume_visual_index", Arguments: args})
	requirements.NoError(err)
	assertions.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_visual_index_status", Arguments: map[string]any{"coverage": true}})
	requirements.NoError(err)
	assertions.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_visual_index_status", Arguments: map[string]any{"coverage": true}})
	requirements.NoError(err)
	assertions.True(called.IsError, "the native coverage rate limit must remain effective")
	retryArgs := map[string]any{"expected_generation_id": vf.Generation.ID, "expected_generation_fingerprint": vf.Generation.Fingerprint, "expected_policy_fingerprint": vf.PolicyFingerprint, "message_id": 1, "blob_hash": strings.Repeat("a", 64)}
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retry_visual_attachment", Arguments: retryArgs})
	requirements.NoError(err)
	assertions.True(called.IsError, "the owning reconciler must reject an absent message/blob target")
	args["generation_id"] = vf.Generation.ID
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retire_visual_generation", Arguments: args})
	requirements.NoError(err)
	assertions.False(called.IsError)
	generation, err = st.GetVisualGeneration(t.Context(), vf.Generation.ID)
	requirements.NoError(err)
	assertions.Equal(store.VisualGenerationRetired, generation.State)
	assertions.Zero(providerRequests.Load(), "policy/progress reads, refused writes and empty-archive passes must not contact the provider")
}

func TestMCPVisualDiscoveryRequiresAllProducerGuards(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Logger: slog.New(slog.DiscardHandler)}))
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "startVisualAttachmentBuild" {
			capabilities.Routes[i].RequestProperties = []string{"consent"}
		}
	}
	restricted := newDaemonMCPOperations(backend.client, capabilities)
	assertions.NotContains(restricted.capabilities(), "build_visual_index")
	assertions.Contains(restricted.capabilities(), "get_visual_index_status")
	capabilities.Delegated = true
	delegated := newDaemonMCPOperations(backend.client, capabilities)
	assertions.NotContains(delegated.capabilities(), "get_visual_index_status")
}

func TestMCPVisualAndDocumentVectorReadsValidateInputsAndExposeDisabledState(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: st, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"build_visual_index", "resume_visual_index", "retry_visual_attachment", "retire_visual_generation"} {
		for _, args := range []map[string]any{{}, {"expected_generation_id": 1}, {"expected_generation_id": nil}, {"path": "/health"}} {
			result, err := backend.ExecuteOperation(t.Context(), name, args)
			requirements.NoError(err)
			assertions.True(result.IsError)
		}
	}
	for _, args := range []map[string]any{{"coverage": nil}, {"coverage": 1}, {"path": "/health"}} {
		result, err := backend.ExecuteOperation(t.Context(), "get_visual_index_status", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	for _, args := range []map[string]any{{"generation_id": 0}, {"limit": 0}, {"limit": 1001}, {"limit": nil}, {"after_token": ""}, {"after_token": strings.Repeat("x", 32769)}, {"path": "/health"}} {
		result, err := backend.ExecuteOperation(t.Context(), "get_document_vector_status", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_document_vector_status", nil)
	requirements.NoError(err)
	status := operationOutput[generated.DocumentVectorOperationsResponse](t, result)
	assertions.False(status.Enabled)
	assertions.Nil(status.Status)
	enabled := config.NewDefaultConfig()
	enabled.Vector.Enabled = true
	enabled.Attachments.Documents.Index.Embeddings.Enabled = true
	unconfigured := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: enabled, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err = unconfigured.ExecuteOperation(t.Context(), "get_document_vector_status", nil)
	requirements.NoError(err)
	status = operationOutput[generated.DocumentVectorOperationsResponse](t, result)
	assertions.True(status.Enabled)
	assertions.False(status.Configured)
	assertions.Nil(status.Status)
	unavailable := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: enabled, Logger: slog.New(slog.DiscardHandler)}))
	result, err = unavailable.ExecuteOperation(t.Context(), "get_document_vector_status", nil)
	requirements.NoError(err)
	assertions.True(result.IsError)
	wire, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(wire), "document_vector_status_unavailable")
	session := operationMCPSession(t, backend, nil, nil)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_document_vector_status"})
	requirements.NoError(err)
	assertions.False(called.IsError)
	assertions.NotNil(called.StructuredContent)
}
