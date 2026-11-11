package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPMergeRecoveryRealStore(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	a, err := st.EnsureParticipant("a@example.test", "A", "example.test")
	require.NoError(t, err)
	b, err := st.EnsureParticipant("b@example.test", "B", "example.test")
	require.NoError(t, err)
	survivor, _, err := st.CreatePersonFromParticipant(a)
	require.NoError(t, err)
	absorbed, _, err := st.CreatePersonFromParticipant(b)
	require.NoError(t, err)
	for id, value := range map[int64]string{survivor.ID: "email", absorbed.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, Source: store.ProvenanceUser})
		require.NoError(t, err)
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(t, err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(t, err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-merge", Actor: "test"})
	require.NoError(t, err)
	require.Len(t, merged.ReviewCandidates, 1)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(ctx, "list_person_merges", map[string]any{"person_id": survivor.ID})
	require.NoError(t, err)
	list := operationOutput[generated.PersonMergesResponse](t, result)
	require.Len(t, list.Merges, 1)
	assert.Equal(t, merged.Merge.ID, list.Merges[0].Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(t, err)
	detail := operationOutput[mcpserver.MergeDetail](t, result)
	assert.Equal(t, merged.Merge.ID, detail.Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge_snapshot", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(t, err)
	snapshot := operationOutput[mcpserver.MergeSnapshot](t, result)
	assert.Equal(t, merged.Merge.SnapshotSHA256, snapshot.SHA256)
	assert.Positive(t, snapshot.RowCount)
	require.Len(t, snapshot.Persons, 2)
	assert.ElementsMatch(t, []int64{survivor.ID, absorbed.ID}, []int64{snapshot.Persons[0].ID, snapshot.Persons[1].ID})
	for _, person := range snapshot.Persons {
		assert.Positive(t, person.Revision)
		if person.ID == absorbed.ID {
			assert.Contains(t, person.ParticipantIDs, b)
		}
	}
	projected, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(projected), "columns")
	etag := fmt.Sprintf(`"person-%d-r%d"`, merged.Person.ID, merged.Person.Revision)
	args := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	stale := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": fmt.Sprintf(`"person-%d-r1"`, survivor.ID)}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", stale)
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "person_merge_revision_conflict", refusal.Code)
	missing := map[string]any{"person_id": survivor.ID, "candidate_id": int64(999999), "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", missing)
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "person_merge_candidate_not_found", refusal.Code)
	var disclosure string
	approvals := 0
	confirm := true
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		disclosure = request.Params.Message
		if !confirm {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "get_person_merge_snapshot")
	assert.Contains(t, names, "decide_person_merge_candidate")
	sdkResult, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(t, err)
	require.False(t, sdkResult.IsError)
	assert.Equal(t, 1, approvals)
	var shown struct {
		Lineage generated.PersonMergeReviewCandidate `json:"lineage"`
	}
	require.NoError(t, json.Unmarshal([]byte(disclosure), &shown))
	assert.Equal(t, merged.ReviewCandidates[0].ID, shown.Lineage.ID)
	assert.Contains(t, disclosure, "Keep the current value")
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(t, err)
	require.False(t, sdkResult.IsError)
	assert.Contains(t, disclosure, "daemon will replay it")
	encoded, err := json.Marshal(sdkResult.StructuredContent)
	require.NoError(t, err)
	var receipt mcpserver.MergeCandidateReceipt
	require.NoError(t, json.Unmarshal(encoded, &receipt))
	assert.Equal(t, "rejected", receipt.Candidate.State)
	assert.NotEmpty(t, receipt.ETag)
	opposite := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "accept", "etag": receipt.ETag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", opposite)
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "person_merge_candidate_state_changed", refusal.Code)
	staleSplit := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": etag, "idempotency_key": "stale-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: staleSplit})
	require.NoError(t, err)
	staleFailure := loopToolFailure(t, sdkResult)
	assert.Equal(t, "person_merge_revision_conflict", staleFailure["error"])
	assert.Equal(t, false, staleFailure["operation_may_have_completed"])
	current, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(t, err)
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": fmt.Sprintf(`"person-%d-r%d"`, current.ID, current.Revision), "idempotency_key": "synthetic-split"}
	confirm = false
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(t, err)
	assert.True(t, sdkResult.IsError)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(t, err)
	assert.Empty(t, operationOutput[mcpserver.MergeDetail](t, result).Splits)
	confirm = true
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(t, err)
	split := loopToolOutput[generated.PersonSplitResult](t, sdkResult)
	assert.Equal(t, merged.Merge.ID, split.Split.MergeID)
	assert.True(t, split.ExactReversal)
	thirdParticipant, err := st.EnsureParticipant("third@example.test", "Third", "example.test")
	require.NoError(t, err)
	third, _, err := st.CreatePersonFromParticipant(thirdParticipant)
	require.NoError(t, err)
	current, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(t, err)
	_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: third.ID, AbsorbedID: current.ID, ExpectedSurvivorRevision: third.Revision, ExpectedAbsorbedRevision: current.Revision, IdempotencyKey: "absorb-split-source", Actor: "test"})
	require.NoError(t, err)
	previousApprovals := approvals
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(t, err)
	replay := loopToolOutput[generated.PersonSplitResult](t, sdkResult)
	assert.Greater(t, approvals, previousApprovals)
	assert.Equal(t, split.Split, replay.Split)
	assert.Equal(t, split.IdentityRevision, replay.IdentityRevision)
	assert.Equal(t, split.NewPerson, replay.NewPerson)
	assert.Equal(t, split.SourcePerson, replay.SourcePerson)
	assert.Equal(t, split.ExactReversal, replay.ExactReversal)
	assert.Equal(t, split.UIDAliasDisposition, replay.UIDAliasDisposition)
	assert.Equal(t, split.AmbiguousRows, replay.AmbiguousRows)
	assert.Equal(t, split.UnrestoredRows, replay.UnrestoredRows)
	var splitDisclosure struct {
		Current any                   `json:"current"`
		Lineage mcpserver.MergeDetail `json:"lineage"`
		Changes struct {
			PersonID       int64   `json:"person_id"`
			MergeID        int64   `json:"merge_id"`
			ParticipantIDs []int64 `json:"participant_ids"`
			ETag           string  `json:"etag"`
			Key            string  `json:"idempotency_key"`
		} `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(disclosure), &splitDisclosure))
	assert.Empty(t, splitDisclosure.Current)
	assert.Equal(t, merged.Merge.ID, splitDisclosure.Lineage.Merge.ID)
	assert.Contains(t, splitDisclosure.Lineage.Labels.ParticipantIds, b)
	require.Len(t, splitDisclosure.Lineage.Splits, 1)
	assert.Equal(t, split.Split.ID, splitDisclosure.Lineage.Splits[0].ID)
	assert.Equal(t, survivor.ID, splitDisclosure.Changes.PersonID)
	assert.Equal(t, merged.Merge.ID, splitDisclosure.Changes.MergeID)
	assert.Equal(t, []int64{b}, splitDisclosure.Changes.ParticipantIDs)
	assert.Equal(t, splitArgs["etag"], splitDisclosure.Changes.ETag)
	assert.Equal(t, "synthetic-split", splitDisclosure.Changes.Key)
	assert.Contains(t, disclosure, "identical completed request returns its saved result")
	assert.Contains(t, disclosure, "Lineage labels are historical")
	differentKey := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": splitArgs["etag"], "idempotency_key": "different-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: differentKey})
	require.NoError(t, err)
	assert.Equal(t, "person_profile_not_found", loopToolFailure(t, sdkResult)["error"])
	differentPayload := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{}, "etag": splitArgs["etag"], "idempotency_key": "synthetic-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: differentPayload})
	require.NoError(t, err)
	assert.Equal(t, "person_split_idempotency_conflict", loopToolFailure(t, sdkResult)["error"])
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(t, err)
	detail = operationOutput[mcpserver.MergeDetail](t, result)
	require.Len(t, detail.Splits, 1)
	assert.Equal(t, split.Split.ID, detail.Splits[0].ID)
	c, err := st.EnsureParticipant("c@example.test", "C", "example.test")
	require.NoError(t, err)
	d, err := st.EnsureParticipant("d@example.test", "D", "example.test")
	require.NoError(t, err)
	left, _, err := st.CreatePersonFromParticipant(c)
	require.NoError(t, err)
	right, _, err := st.CreatePersonFromParticipant(d)
	require.NoError(t, err)
	for id, val := range map[int64]string{left.ID: "email", right.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &val}, Source: store.ProvenanceUser})
		require.NoError(t, err)
	}
	left, err = st.GetPersonContext(ctx, left.ID)
	require.NoError(t, err)
	right, err = st.GetPersonContext(ctx, right.ID)
	require.NoError(t, err)
	other, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: left.ID, AbsorbedID: right.ID, ExpectedSurvivorRevision: left.Revision, ExpectedAbsorbedRevision: right.Revision, IdempotencyKey: "second-merge", Actor: "test"})
	require.NoError(t, err)
	require.Len(t, other.ReviewCandidates, 1)
	acceptArgs := map[string]any{"person_id": left.ID, "merge_id": other.Merge.ID, "candidate_id": other.ReviewCandidates[0].ID, "decision": "accept", "etag": fmt.Sprintf(`"person-%d-r%d"`, other.Person.ID, other.Person.Revision)}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: acceptArgs})
	require.NoError(t, err)
	require.False(t, sdkResult.IsError)
	assert.Contains(t, disclosure, fmt.Sprintf("attribute definition %d", other.ReviewCandidates[0].DefinitionID))
	assert.Contains(t, disclosure, fmt.Sprintf("absorbed value %d", other.ReviewCandidates[0].AbsorbedValueID))
	assert.Contains(t, disclosure, "exact split becomes unavailable")

}

func TestMCPMergeRecoveryRejectsUnsupportedAndInvalid(t *testing.T) {
	backend := &daemonMCPOperations{supported: []string{"list_person_merges", "get_person_merge", "decide_person_merge_candidate", "split_person_merge"}}
	for _, tc := range []struct {
		name       string
		args       map[string]any
		disclosure bool
	}{
		{"list_person_merges", map[string]any{"person_id": 1, "limit": 501}, false},
		{"list_person_merges", map[string]any{"person_id": 1, "offset": -1}, false},
		{"get_person_merge", map[string]any{"merge_id": 0}, false},
		{"split_person_merge", map[string]any{"person_id": 1, "merge_id": 1, "participant_ids": []int64{1, 1}, "etag": "x", "idempotency_key": "key"}, true},
		{"split_person_merge", map[string]any{"person_id": 1, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": ""}, true},
		{"split_person_merge", map[string]any{"person_id": 1, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": strings.Repeat("x", 129)}, true},
		{"decide_person_merge_candidate", map[string]any{"person_id": 1, "merge_id": 1, "candidate_id": 1, "decision": "reject", "etag": ""}, true},
	} {
		if tc.disclosure {
			_, err := backend.OperationDisclosure(t.Context(), tc.name, tc.args)
			var refusal *mcpserver.OperationRefusalError
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, "invalid_arguments", refusal.Code)
			continue
		}
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(t, err)
		assertLoopRefusal(t, result, "invalid_arguments", false)
	}
}

// Synthetic daemon-version skew and large detail fixtures exercise adapter projection.
func TestMCPMergeRecoveryBoundsAndSafeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/person-merges/1/snapshot":
			_, _ = w.Write([]byte(`{"version":2,"sha256":"x","snapshot":{"persons":[],"rows":[]}}`))
		case "/api/v1/person-merges/2/snapshot":
			_, _ = w.Write([]byte(`{"version":1,"sha256":"x","snapshot":{"persons":[{},{},{}],"rows":[]}}`))
		case "/api/v1/person-merges/4/snapshot":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"person_merge_snapshot_corrupt"}`))
		default:
			_, _ = w.Write([]byte(`{"rows":[` + strings.Repeat(`{"original_row_key":"`+strings.Repeat("x", 200)+`"},`, 6000) + `{}]}`))
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge_snapshot", "get_person_merge"}}
	for _, id := range []int{1, 2} {
		result, err := backend.ExecuteOperation(t.Context(), "get_person_merge_snapshot", map[string]any{"merge_id": id})
		require.NoError(t, err)
		assertLoopRefusal(t, result, "invalid_operation_response", false)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_merge_snapshot", map[string]any{"merge_id": 4})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "person_merge_snapshot_corrupt", false)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 3})
	require.NoError(t, err)
	detail := operationOutput[mcpserver.MergeDetail](t, result)
	assert.Equal(t, 6001, detail.RowCount)
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"rows"`)
}

func TestMCPMergeRecoveryCapabilityRequiresEachContract(t *testing.T) {
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, fixture)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct{ operation, missing, name string }{
		{"getPersonMerge", "route", "decide_person_merge_candidate"}, {"getPersonMerge", "route", "split_person_merge"},
		{"getPersonProfile", "route", "decide_person_merge_candidate"}, {"getPersonProfile", "route", "split_person_merge"},
		{"decidePersonMergeCandidate", "route", "decide_person_merge_candidate"}, {"decidePersonMergeCandidate", "decision", "decide_person_merge_candidate"}, {"decidePersonMergeCandidate", "person_id", "decide_person_merge_candidate"},
		{"splitPersonMerge", "merge_id", "split_person_merge"}, {"splitPersonMerge", "participant_ids", "split_person_merge"},
		{"listPersonMerges", "limit", "list_person_merges"}, {"listPersonMerges", "offset", "list_person_merges"},
	} {
		copyCaps := *caps
		copyCaps.Routes = slices.Clone(caps.Routes)
		for i := range copyCaps.Routes {
			route := &copyCaps.Routes[i]
			if route.OperationID != tc.operation {
				continue
			}
			if tc.missing == "route" {
				route.Method = http.MethodDelete
			} else if tc.operation == "listPersonMerges" {
				route.QueryParameters = slices.DeleteFunc(slices.Clone(route.QueryParameters), func(v string) bool { return v == tc.missing })
			} else {
				route.RequestProperties = slices.DeleteFunc(slices.Clone(route.RequestProperties), func(v string) bool { return v == tc.missing })
			}
		}
		assert.NotContains(t, newDaemonMCPOperations(backend.client, &copyCaps).capabilities(), tc.name)
	}
}
