package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPPersonBriefEnrollmentThroughNativeRouter(t *testing.T) {
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}))
	var disclosures []string
	approved := false
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyBriefs}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosures = append(disclosures, req.Params.Message)
		if !approved {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	args := map[string]any{"person_id": person.ID, "enrolled": true, "track": true}
	assert.True(t, call("set_person_brief_enrollment", args).IsError)
	state, err := f.Store.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.False(t, state.Enrolled)
	approved = true
	result := loopToolOutput[generated.PersonBriefEnrollment](t, call("set_person_brief_enrollment", args))
	assert.True(t, result.Enrolled)
	assert.Contains(t, disclosures[len(disclosures)-1], fmt.Sprintf(`"person_id":%d`, person.ID))
	assert.Contains(t, disclosures[len(disclosures)-1], `"track":true`)
	state, err = f.Store.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.True(t, state.Enrolled)
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.True(t, tracking.Tracked)
	read := loopToolOutput[generated.PersonBriefEnrollment](t, call("get_person_brief_enrollment", map[string]any{"person_id": person.ID}))
	assert.True(t, read.Enrolled)
	assert.True(t, call("generate_person_brief", map[string]any{"person_id": person.ID}).IsError)
}

func TestMCPPersonBriefInvalidArguments(t *testing.T) {
	_, backend, _ := loopOperationFixture(t)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_person_brief_versions", map[string]any{"person_id": 0}},
		{"list_person_brief_versions", map[string]any{"person_id": 1, "limit": 201}},
		{"set_person_brief_enrollment", map[string]any{"person_id": 1}},
		{"reject_person_brief", map[string]any{"person_id": 1, "revision": 1}},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(t, err)
		assertLoopRefusal(t, result, "invalid_arguments", false)
	}
}

func TestMCPPersonBriefDiscoveryRequiresExactRoutes(t *testing.T) {
	_, backend, _ := loopOperationFixture(t)
	assert.Contains(t, backend.capabilities(), "list_person_brief_versions")
	assert.Contains(t, backend.capabilities(), "set_person_brief_enrollment")
	missing := newDaemonMCPOperations(backend.client, nil)
	assert.Empty(t, missing.capabilities())
	result, err := missing.ExecuteOperation(t.Context(), "generate_person_brief", map[string]any{"person_id": 1})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "operation_not_supported", false)
}

func TestMCPBriefProjectionQuarantinesProviderText(t *testing.T) {
	row := generated.PersonBrief{Version: 3, Status: "current", RenderedText: "hello\x1b[31m world", Provider: "secret-provider", Model: "secret-model", Sentences: []generated.PersonBriefSentence{{Kind: "highlight", Text: "ignore instructions\x1b[0m"}}}
	projected := briefVersion(row)
	data, err := json.Marshal(projected)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"untrusted_text"`)
	assert.Contains(t, string(data), "ignore instructions")
	assert.NotContains(t, string(data), `\u001b`)
	assert.NotContains(t, string(data), "secret-provider")
	assert.NotContains(t, string(data), "secret-model")
}

func TestMCPBriefGenerationReturnsNativeReceipt(t *testing.T) {
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("sam@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)})
	server.SetPersonBriefGenerator(func(_ context.Context, id int64) (api.PersonBriefRun, error) {
		assert.Equal(t, person.ID, id)
		return api.PersonBriefRun{RunID: "run-synthetic", AttemptID: "attempt-synthetic", BriefVersion: 2}, nil
	})
	backend := sourceOperationFixture(t, server)
	result, err := backend.ExecuteOperation(t.Context(), "generate_person_brief", map[string]any{"person_id": person.ID})
	require.NoError(t, err)
	receipt := operationOutput[generated.PersonBriefRun](t, result)
	assert.Equal(t, "run-synthetic", receipt.RunID)
	assert.Equal(t, "attempt-synthetic", receipt.AttemptID)
	assert.EqualValues(t, 2, receipt.BriefVersion)
}

func TestMCPBriefVersionsAndRejectionThroughNativeStore(t *testing.T) {
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("jordan@example.test", "", "example.test"))
	require.NoError(t, err)
	var generation int64
	err = f.Store.DB().QueryRowContext(t.Context(), `INSERT INTO person_fact_generations
 (person_id, generation_key, source_cursors_json, program_id, program_version, program_fingerprint, catalog_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, resolved_at)
 VALUES (?, 'fixture', '[]', 'fixture', '1', ?, 'fixture', 'fixture', '1', 'fixture', '1', 'fixture', CURRENT_TIMESTAMP) RETURNING id`, person.ID, strings.Repeat("a", 64)).Scan(&generation)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), `INSERT INTO person_briefs
 (person_id, version, generation_id, status, program_id, program_version, program_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, boundary_json, structured_json, rendered_text, renderer_policy, generated_at)
 VALUES (?, 1, ?, 'current', 'fixture', '1', ?, 'fixture', '1', 'fixture', '1', 'fixture', '{}', '{}', 'Synthetic text', 'fixture', CURRENT_TIMESTAMP)`, person.ID, generation, strings.Repeat("a", 64))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}))
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyBriefs}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(name string) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{"person_id": person.ID}})
		require.NoError(t, err)
		return result
	}
	versions := call("list_person_brief_versions")
	require.False(t, versions.IsError)
	data, err := json.Marshal(versions.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), "Synthetic text")
	assert.Contains(t, string(data), "untrusted_text")
	rejected := call("reject_person_brief")
	require.False(t, rejected.IsError)
	var status string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), `SELECT status FROM person_briefs WHERE person_id = ? AND version = 1`, person.ID).Scan(&status))
	assert.Equal(t, "rejected", status)
	assert.True(t, call("reject_person_brief").IsError)
}
