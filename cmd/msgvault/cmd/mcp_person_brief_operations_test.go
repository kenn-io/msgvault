package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPPersonBriefEnrollmentThroughNativeRouter(t *testing.T) {
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "", "example.test"))
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, server)
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
	disclosureCount := len(disclosures)
	invalid := call("set_person_brief_enrollment", map[string]any{"person_id": person.ID, "enrolled": true, "track": false, "extra": 1})
	assert.True(t, invalid.IsError)
	assert.Len(t, disclosures, disclosureCount)
	noTrack := call("set_person_brief_enrollment", map[string]any{"person_id": person.ID, "enrolled": true, "track": false})
	failure := loopToolFailure(t, noTrack)
	assert.Equal(t, "person_brief_not_tracked", failure["error"])
	assert.Equal(t, false, failure["operation_may_have_completed"])
	result := loopToolOutput[generated.PersonBriefEnrollment](t, call("set_person_brief_enrollment", args))
	assert.True(t, result.Enrolled)
	assert.Contains(t, disclosures[len(disclosures)-1], fmt.Sprintf(`"person_id":%d`, person.ID))
	assert.Contains(t, disclosures[len(disclosures)-1], `"track":true`)
	assert.Contains(t, disclosures[len(disclosures)-1], "enrollment lets scheduled sweeps generate briefs under existing consent and provider budget")
	state, err = f.Store.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.True(t, state.Enrolled)
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.True(t, tracking.Tracked)
	read := loopToolOutput[generated.PersonBriefEnrollment](t, call("get_person_brief_enrollment", map[string]any{"person_id": person.ID}))
	assert.True(t, read.Enrolled)
	failure = loopToolFailure(t, call("generate_person_brief", map[string]any{"person_id": person.ID}))
	assert.Equal(t, "brief_generation_unavailable", failure["error"])
	assert.Equal(t, false, failure["operation_may_have_completed"])
	assert.Contains(t, disclosures[len(disclosures)-1], "may extract and save profile facts and spend provider budget, even if brief generation fails")
	var generateErr error
	server.SetPersonBriefGenerator(func(_ context.Context, id int64) (api.PersonBriefRun, error) {
		assert.Equal(t, person.ID, id)
		return api.PersonBriefRun{RunID: "run-synthetic", AttemptID: "attempt-synthetic", BriefVersion: 2}, generateErr
	})
	receipt := loopToolOutput[generated.PersonBriefRun](t, call("generate_person_brief", map[string]any{"person_id": person.ID}))
	assert.Equal(t, "run-synthetic", receipt.RunID)
	assert.Equal(t, "attempt-synthetic", receipt.AttemptID)
	assert.EqualValues(t, 2, receipt.BriefVersion)
	for _, tc := range []struct {
		err       error
		code      string
		uncertain bool
	}{{peoplesweep.ErrPersonBriefBusy, "person_brief_busy", false}, {peoplesweep.ErrPersonBriefNotEnrolled, "person_brief_not_enrolled", false}, {errors.New("private failure"), "operation_refused", true}} {
		generateErr = tc.err
		failure = loopToolFailure(t, call("generate_person_brief", map[string]any{"person_id": person.ID}))
		assert.Equal(t, tc.code, failure["error"])
		assert.Equal(t, tc.uncertain, failure["operation_may_have_completed"])
	}
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	busy := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	busyResult, err := busy.ExecuteOperation(t.Context(), "set_person_brief_enrollment", args)
	require.NoError(t, err)
	assertLoopRefusal(t, busyResult, "server_busy", false)
	for _, code := range []string{"invalid_request", "server_busy"} {
		client := newMCPDaemonClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := fmt.Fprintf(w, `{"error":%q}`, code)
			assert.NoError(t, err)
		})
		response, err := readMCPOperationJSONWithClassifier[generated.PersonBriefRun](t.Context(), client, http.MethodPost, "/api/v1/people/1/brief/generate", nil, briefRefusal, http.StatusOK)
		require.NoError(t, err)
		assertLoopRefusal(t, response, code, false)
	}
}

func TestMCPPersonBriefDiscoveryRequiresExactRoutes(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	names := []string{"list_person_brief_versions", "get_person_brief_enrollment", "set_person_brief_enrollment", "generate_person_brief", "reject_person_brief"}
	for _, name := range names {
		assert.Contains(t, backend.capabilities(), name)
	}
	for _, tc := range []struct {
		id, missing string
		edit        func()
	}{
		{"setPersonBriefEnrollment", "set_person_brief_enrollment", func() {
			for i := range caps.Routes {
				if caps.Routes[i].OperationID == "setPersonBriefEnrollment" {
					caps.Routes[i].RequestProperties = []string{"enrolled"}
				}
			}
		}},
		{"listPersonBriefVersions", "list_person_brief_versions", func() {
			for i := range caps.Routes {
				if caps.Routes[i].OperationID == "listPersonBriefVersions" {
					caps.Routes[i].QueryParameters = nil
				}
			}
		}},
	} {
		original := slices.Clone(caps.Routes)
		tc.edit()
		narrowed := newDaemonMCPOperations(backend.client, caps)
		for _, name := range names {
			if name == tc.missing {
				assert.NotContains(t, narrowed.capabilities(), name)
			} else {
				assert.Contains(t, narrowed.capabilities(), name)
			}
		}
		caps.Routes = original
	}
	result, err := backend.ExecuteOperation(t.Context(), "set_person_brief_enrollment", map[string]any{"person_id": 1})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "invalid_arguments", false)
}

func TestMCPBriefVersionsAndRejectionThroughNativeStore(t *testing.T) {
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("jordan@example.test", "", "example.test"))
	require.NoError(t, err)
	bind := "?"
	db := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		bind = "?::jsonb"
	}
	output := peoplesweep.BriefOutput{Highlights: []peoplesweep.BriefHighlight{{Text: "synthetic update", Speaker: peoplesweep.BriefSpeakerPerson, EvidenceIDs: []string{"evidence-1"}, ConfidenceBasisPoints: 800}}, FollowUps: []peoplesweep.BriefFollowUp{}, Appreciations: []peoplesweep.BriefAppreciation{}, Uncertainties: []peoplesweep.BriefUncertainty{}, PossibleAttributes: []peoplesweep.ExtractedClaim{}}
	structured, err := json.Marshal(output)
	require.NoError(t, err)
	rendered, err := peoplesweep.RenderBrief(peoplesweep.ParsedBrief{Output: output}, peoplesweep.BriefWindow{})
	require.NoError(t, err)
	boundary, err := json.Marshal(peoplesweep.BriefBoundary{Lanes: []string{"conversation_text"}, ItemCount: 1, InputBytes: 100, PacketSHA256: strings.Repeat("c", 64)})
	require.NoError(t, err)
	var generation int64
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO person_fact_generations (person_id, generation_key, source_cursors_json, program_id, program_version, program_fingerprint, catalog_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, resolved_at) VALUES (?, 'fixture', `+bind+`, 'fixture', '1', ?, 'fixture', 'fixture', '1', 'fixture', '1', 'fixture', CURRENT_TIMESTAMP) RETURNING id`), person.ID, "[]", strings.Repeat("a", 64)).Scan(&generation)
	require.NoError(t, err)
	for _, row := range []struct {
		version int
		status  string
	}{{1, "superseded"}, {2, "current"}} {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_briefs (person_id, version, generation_id, status, program_id, program_version, program_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, boundary_json, structured_json, rendered_text, renderer_policy, generated_at, superseded_at) VALUES (?, ?, ?, ?, 'fixture', '1', ?, 'fixture', '1', 'fixture', '1', 'fixture', `+bind+`, `+bind+`, ?, ?, CURRENT_TIMESTAMP, CASE WHEN ? = 'superseded' THEN CURRENT_TIMESTAMP ELSE NULL END)`), person.ID, row.version, generation, row.status, strings.Repeat("a", 64), string(boundary), string(structured), rendered.Text, peoplesweep.BriefRendererPolicyV1, row.status)
		require.NoError(t, err)
	}
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}))
	var disclosures []string
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyBriefs}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, req *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosures = append(disclosures, req.Params.Message)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	args := map[string]any{"person_id": person.ID}
	versions := call("list_person_brief_versions", args)
	require.False(t, versions.IsError)
	data, err := json.Marshal(versions.StructuredContent)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"provider"`)
	assert.NotContains(t, string(data), `"model"`)
	var history struct {
		Versions []struct {
			Brief struct {
				UntrustedText struct {
					Sentences []struct {
						Text string `json:"text"`
					} `json:"sentences"`
					Items []struct {
						Text string `json:"text"`
					} `json:"items"`
				} `json:"untrusted_text"`
			} `json:"brief"`
			Status       string  `json:"status"`
			RejectedAt   *string `json:"rejected_at"`
			SupersededAt *string `json:"superseded_at"`
		} `json:"versions"`
	}
	require.NoError(t, json.Unmarshal(data, &history))
	require.Len(t, history.Versions, 2)
	require.Len(t, history.Versions[0].Brief.UntrustedText.Sentences, 1)
	assert.Equal(t, rendered.Text, history.Versions[0].Brief.UntrustedText.Sentences[0].Text)
	require.Len(t, history.Versions[0].Brief.UntrustedText.Items, 1)
	assert.Equal(t, "synthetic update", history.Versions[0].Brief.UntrustedText.Items[0].Text)
	assert.Equal(t, "current", history.Versions[0].Status)
	assert.Nil(t, history.Versions[0].RejectedAt)
	assert.Nil(t, history.Versions[0].SupersededAt)
	assert.Equal(t, "superseded", history.Versions[1].Status)
	assert.NotNil(t, history.Versions[1].SupersededAt)
	rejected := call("reject_person_brief", map[string]any{"person_id": person.ID, "reason": "wrong\u001b[31m detail"})
	require.False(t, rejected.IsError)
	assert.Contains(t, disclosures[len(disclosures)-1], "reject the version current at execution (no version precondition); the next eligible scheduled sweep may generate a replacement")
	data, err = json.Marshal(rejected.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"rejected_reason":"wrong detail"`)
	assert.NotContains(t, string(data), `\u001b`)
	var rejection struct {
		Status       string  `json:"status"`
		RejectedAt   *string `json:"rejected_at"`
		SupersededAt *string `json:"superseded_at"`
	}
	require.NoError(t, json.Unmarshal(data, &rejection))
	assert.Equal(t, "rejected", rejection.Status)
	assert.NotNil(t, rejection.RejectedAt)
	assert.Nil(t, rejection.SupersededAt)
	var status, reason string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT status,rejected_reason FROM person_briefs WHERE person_id = ? AND version = 2`), person.ID).Scan(&status, &reason))
	assert.Equal(t, "rejected", status)
	assert.Equal(t, "wrong\u001b[31m detail", reason)
	versions = call("list_person_brief_versions", args)
	require.False(t, versions.IsError)
	data, err = json.Marshal(versions.StructuredContent)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"rejected_reason":"wrong detail"`)
	require.NoError(t, json.Unmarshal(data, &history))
	assert.Equal(t, "rejected", history.Versions[0].Status)
	assert.NotNil(t, history.Versions[0].RejectedAt)
	failure := loopToolFailure(t, call("reject_person_brief", args))
	assert.Equal(t, "person_brief_not_found", failure["error"])
	assert.Equal(t, false, failure["operation_may_have_completed"])
}
