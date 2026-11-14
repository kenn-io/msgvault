package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
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
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "", "example.test"))
	require.NoError(err)
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
		require.NoError(err)
		return result
	}
	args := map[string]any{"person_id": person.ID, "enrolled": true, "track": true}
	assert.True(call("set_person_brief_enrollment", args).IsError)
	state, err := f.Store.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(err)
	assert.False(state.Enrolled)
	approved = true
	disclosureCount := len(disclosures)
	missing, err := backend.ExecuteOperation(t.Context(), "set_person_brief_enrollment", map[string]any{"person_id": person.ID})
	require.NoError(err)
	assertLoopRefusal(t, missing, "invalid_arguments", false)
	invalid := call("set_person_brief_enrollment", map[string]any{"person_id": person.ID, "enrolled": true, "track": false, "extra": 1})
	assert.True(invalid.IsError)
	assert.Len(disclosures, disclosureCount)
	noTrack := call("set_person_brief_enrollment", map[string]any{"person_id": person.ID, "enrolled": true})
	failure := loopToolFailure(t, noTrack)
	assert.Equal("person_brief_not_tracked", failure["error"])
	assert.Equal(false, failure["operation_may_have_completed"])
	result := loopToolOutput[generated.PersonBriefEnrollment](t, call("set_person_brief_enrollment", args))
	assert.True(result.Enrolled)
	assert.Contains(disclosures[len(disclosures)-1], fmt.Sprintf(`"person_id":%d`, person.ID))
	assert.Contains(disclosures[len(disclosures)-1], `"track":true`)
	assert.Contains(disclosures[len(disclosures)-1], "enrollment lets scheduled sweeps generate briefs under existing consent and provider budget")
	state, err = f.Store.GetPersonBriefEnrollmentContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(state.Enrolled)
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	read := loopToolOutput[mcpserver.PersonBriefVersions](t, call("list_person_brief_versions", map[string]any{"person_id": person.ID}))
	assert.True(read.Enrollment.Enrolled)
	assert.Empty(read.Versions)
	assert.Nil(read.NextBeforeVersion)
	args = map[string]any{"person_id": person.ID, "enrolled": false}
	result = loopToolOutput[generated.PersonBriefEnrollment](t, call("set_person_brief_enrollment", args))
	assert.False(result.Enrolled)
	tracking, err = f.Store.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	busy := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	busyResult, err := busy.ExecuteOperation(t.Context(), "set_person_brief_enrollment", args)
	require.NoError(err)
	assertLoopRefusal(t, busyResult, "server_busy", false)
}

func TestMCPPersonBriefDiscoveryRequiresExactRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)}))
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(err)
	names := []string{"list_person_brief_versions", "set_person_brief_enrollment"}
	for _, name := range names {
		assert.Contains(backend.capabilities(), name)
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
					caps.Routes[i].QueryParameters = []string{"limit"}
				}
			}
		}},
		{"getPersonBriefEnrollment", "list_person_brief_versions", func() {
			for i := range caps.Routes {
				if caps.Routes[i].OperationID == "getPersonBriefEnrollment" {
					caps.Routes[i].Path = "/missing"
				}
			}
		}},
	} {
		original := slices.Clone(caps.Routes)
		tc.edit()
		narrowed := newDaemonMCPOperations(backend.client, caps)
		for _, name := range names {
			if name == tc.missing {
				assert.NotContains(narrowed.capabilities(), name)
			} else {
				assert.Contains(narrowed.capabilities(), name)
			}
		}
		caps.Routes = original
	}
}

func TestMCPBriefVersionsThroughNativeStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("jordan@example.test", "", "example.test"))
	require.NoError(err)
	bind := "?"
	db := os.Getenv("MSGVAULT_TEST_DB")
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		bind = "?::jsonb"
	}
	output := peoplesweep.BriefOutput{Highlights: []peoplesweep.BriefHighlight{{Text: "synthetic update", Speaker: peoplesweep.BriefSpeakerPerson, EvidenceIDs: []string{"evidence-1"}, ConfidenceBasisPoints: 800}}, FollowUps: []peoplesweep.BriefFollowUp{}, Appreciations: []peoplesweep.BriefAppreciation{}, Uncertainties: []peoplesweep.BriefUncertainty{}, PossibleAttributes: []peoplesweep.ExtractedClaim{}}
	structured, err := json.Marshal(output)
	require.NoError(err)
	rendered, err := peoplesweep.RenderBrief(peoplesweep.ParsedBrief{Output: output}, peoplesweep.BriefWindow{})
	require.NoError(err)
	boundary, err := json.Marshal(peoplesweep.BriefBoundary{Lanes: []string{"conversation_text"}, ItemCount: 1, InputBytes: 100, PacketSHA256: strings.Repeat("c", 64)})
	require.NoError(err)
	var generation int64
	err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO person_fact_generations (person_id, generation_key, source_cursors_json, program_id, program_version, program_fingerprint, catalog_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, resolved_at) VALUES (?, 'fixture', `+bind+`, 'fixture', '1', ?, 'fixture', 'fixture', '1', 'fixture', '1', 'fixture', CURRENT_TIMESTAMP) RETURNING id`), person.ID, "[]", strings.Repeat("a", 64)).Scan(&generation)
	require.NoError(err)
	for version := 1; version <= 12; version++ {
		row := struct {
			version int
			status  string
		}{version, "superseded"}
		if version == 12 {
			row.status = "current"
		}
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_briefs (person_id, version, generation_id, status, program_id, program_version, program_fingerprint, provider, provider_version, model, model_version, provider_policy_fingerprint, boundary_json, structured_json, rendered_text, renderer_policy, generated_at, superseded_at) VALUES (?, ?, ?, ?, 'fixture', '1', ?, 'fixture', '1', 'fixture', '1', 'fixture', `+bind+`, `+bind+`, ?, ?, CURRENT_TIMESTAMP, CASE WHEN ? = 'superseded' THEN CURRENT_TIMESTAMP ELSE NULL END)`), person.ID, row.version, generation, row.status, strings.Repeat("a", 64), string(boundary), string(structured), rendered.Text, peoplesweep.BriefRendererPolicyV1, row.status)
		require.NoError(err)
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_briefs SET rejected_reason = ?, status = 'rejected', rejected_at = CURRENT_TIMESTAMP, superseded_at = NULL WHERE person_id = ?`), strings.Repeat("r", 4800), person.ID)
	require.NoError(err)
	for i := range 20 {
		var evidenceID int64
		err = f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO person_fact_evidence (person_id, evidence_key, source_class, directness, authority, event_time, recorded_time, identity_score) VALUES (?, ?, 'archive', 'direct-other', 'ordinary', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 1000) RETURNING id`), person.ID, fmt.Sprintf("evidence-%d", i)).Scan(&evidenceID)
		require.NoError(err)
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO person_brief_evidence (brief_id, evidence_id, ordinal) SELECT id, ?, ? FROM person_briefs WHERE person_id = ?`), evidenceID, i, person.ID)
		require.NoError(err)
	}
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, server)
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyBriefs}, nil)
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(err)
		return result
	}
	args := map[string]any{"person_id": person.ID}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_briefs SET rejected_reason = ? WHERE person_id = ? AND version = 2`), strings.Repeat("x", (1<<20)-1024), person.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_briefs SET rejected_reason = ?, status = 'rejected', rejected_at = CURRENT_TIMESTAMP WHERE person_id = ? AND version = 12`), "wrong\u001b[31m detail", person.ID)
	require.NoError(err)
	firstPage := call("list_person_brief_versions", map[string]any{"person_id": person.ID, "limit": 10})
	require.False(firstPage.IsError)
	encoded, err := json.Marshal(firstPage.StructuredContent)
	require.NoError(err)
	assert.Contains(string(encoded), `"rejected_reason":"wrong detail"`)
	assert.NotContains(string(encoded), `\u001b`)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_briefs SET rejected_reason = ? WHERE person_id = ? AND version = 2`), strings.Repeat("r", 4800), person.ID)
	require.NoError(err)
	largePage := loopToolOutput[mcpserver.PersonBriefVersions](t, firstPage)
	require.Len(largePage.Versions, 10)
	for i, version := range []int{12, 11, 10, 9, 8, 7, 6, 5, 4, 3} {
		assert.Equal(version, largePage.Versions[i].Brief.Version)
	}
	require.NotNil(largePage.NextBeforeVersion)
	assert.Equal(int64(3), *largePage.NextBeforeVersion)
	exactArgs := map[string]any{"person_id": person.ID, "limit": 4}
	for pageIndex, want := range [][]int{{12, 11, 10, 9}, {8, 7, 6, 5}, {4, 3, 2, 1}} {
		page := loopToolOutput[mcpserver.PersonBriefVersions](t, call("list_person_brief_versions", exactArgs))
		require.Len(page.Versions, len(want))
		for i, version := range want {
			assert.Equal(version, page.Versions[i].Brief.Version)
		}
		if pageIndex < 2 {
			require.NotNil(page.NextBeforeVersion)
			exactArgs["before_version"] = *page.NextBeforeVersion
		} else {
			assert.Nil(page.NextBeforeVersion)
		}
	}
	result := call("list_person_brief_versions", args)
	require.False(result.IsError)
	history := loopToolOutput[mcpserver.PersonBriefVersions](t, result)
	require.Len(history.Versions, 5)
	for i, version := range []int{12, 11, 10, 9, 8} {
		assert.Equal(version, history.Versions[i].Brief.Version)
	}
	require.NotNil(history.NextBeforeVersion)
	assert.Equal(int64(8), *history.NextBeforeVersion)
}
