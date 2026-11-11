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
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	a, err := st.EnsureParticipant("a@example.test", "A", "example.test")
	require.NoError(err)
	b, err := st.EnsureParticipant("b@example.test", "B", "example.test")
	require.NoError(err)
	survivor, _, err := st.CreatePersonFromParticipant(a)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(b)
	require.NoError(err)
	for id, value := range map[int64]string{survivor.ID: "email", absorbed.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, Source: store.ProvenanceUser})
		require.NoError(err)
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-merge", Actor: "test"})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(ctx, "list_person_merges", map[string]any{"person_id": survivor.ID})
	require.NoError(err)
	list := operationOutput[generated.PersonMergesResponse](t, result)
	require.Len(list.Merges, 1)
	assert.Equal(merged.Merge.ID, list.Merges[0].Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(err)
	detail := operationOutput[mcpserver.MergeDetail](t, result)
	assert.Equal(merged.Merge.ID, detail.Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge_snapshot", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(err)
	snapshot := operationOutput[mcpserver.MergeSnapshot](t, result)
	assert.Equal(merged.Merge.SnapshotSHA256, snapshot.SHA256)
	assert.Positive(snapshot.RowCount)
	require.Len(snapshot.Persons, 2)
	assert.ElementsMatch([]int64{survivor.ID, absorbed.ID}, []int64{snapshot.Persons[0].ID, snapshot.Persons[1].ID})
	for _, person := range snapshot.Persons {
		assert.Positive(person.Revision)
		if person.ID == absorbed.ID {
			assert.Contains(person.ParticipantIDs, b)
		}
	}
	projected, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.NotContains(string(projected), "columns")
	etag := fmt.Sprintf(`"person-%d-r%d"`, merged.Person.ID, merged.Person.Revision)
	args := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	stale := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": fmt.Sprintf(`"person-%d-r1"`, survivor.ID)}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", stale)
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_revision_conflict", refusal.Code)
	missing := map[string]any{"person_id": survivor.ID, "candidate_id": int64(999999), "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", missing)
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_candidate_not_found", refusal.Code)
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
	require.NoError(err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(names, "get_person_merge_snapshot")
	assert.Contains(names, "decide_person_merge_candidate")
	sdkResult, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Equal(1, approvals)
	var shown struct {
		Lineage generated.PersonMergeReviewCandidate `json:"lineage"`
	}
	require.NoError(json.Unmarshal([]byte(disclosure), &shown))
	assert.Equal(merged.ReviewCandidates[0].ID, shown.Lineage.ID)
	assert.Contains(disclosure, "Keep the current value")
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Contains(disclosure, "daemon will replay it")
	encoded, err := json.Marshal(sdkResult.StructuredContent)
	require.NoError(err)
	var receipt mcpserver.MergeCandidateReceipt
	require.NoError(json.Unmarshal(encoded, &receipt))
	assert.Equal("rejected", receipt.Candidate.State)
	assert.NotEmpty(receipt.ETag)
	opposite := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "accept", "etag": receipt.ETag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", opposite)
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_candidate_state_changed", refusal.Code)
	staleSplit := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": etag, "idempotency_key": "stale-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: staleSplit})
	require.NoError(err)
	staleFailure := loopToolFailure(t, sdkResult)
	assert.Equal("person_merge_revision_conflict", staleFailure["error"])
	assert.Equal(false, staleFailure["operation_may_have_completed"])
	current, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": fmt.Sprintf(`"person-%d-r%d"`, current.ID, current.Revision), "idempotency_key": "synthetic-split"}
	confirm = false
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(err)
	assert.True(sdkResult.IsError)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(err)
	assert.Empty(operationOutput[mcpserver.MergeDetail](t, result).Splits)
	confirm = true
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(err)
	split := loopToolOutput[mcpserver.MergeSplitReceipt](t, sdkResult)
	assert.Equal(merged.Merge.ID, split.Split.MergeID)
	assert.True(split.ExactReversal)
	thirdParticipant, err := st.EnsureParticipant("third@example.test", "Third", "example.test")
	require.NoError(err)
	third, _, err := st.CreatePersonFromParticipant(thirdParticipant)
	require.NoError(err)
	current, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: third.ID, AbsorbedID: current.ID, ExpectedSurvivorRevision: third.Revision, ExpectedAbsorbedRevision: current.Revision, IdempotencyKey: "absorb-split-source", Actor: "test"})
	require.NoError(err)
	previousApprovals := approvals
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(err)
	replay := loopToolOutput[mcpserver.MergeSplitReceipt](t, sdkResult)
	assert.Greater(approvals, previousApprovals)
	assert.Equal(split.Split, replay.Split)
	assert.Equal(split.IdentityRevision, replay.IdentityRevision)
	assert.Equal(split.NewPerson, replay.NewPerson)
	assert.Equal(split.SourcePerson, replay.SourcePerson)
	assert.Equal(split.ExactReversal, replay.ExactReversal)
	assert.Equal(split.UIDAliasDisposition, replay.UIDAliasDisposition)
	assert.Equal(split.AmbiguousRowCount, replay.AmbiguousRowCount)
	assert.Equal(split.UnrestoredRowCount, replay.UnrestoredRowCount)
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
	require.NoError(json.Unmarshal([]byte(disclosure), &splitDisclosure))
	assert.Empty(splitDisclosure.Current)
	assert.Equal(merged.Merge.ID, splitDisclosure.Lineage.Merge.ID)
	assert.Contains(splitDisclosure.Lineage.Labels.ParticipantIds, b)
	require.Len(splitDisclosure.Lineage.Splits, 1)
	assert.Equal(split.Split.ID, splitDisclosure.Lineage.Splits[0].ID)
	assert.Equal(survivor.ID, splitDisclosure.Changes.PersonID)
	assert.Equal(merged.Merge.ID, splitDisclosure.Changes.MergeID)
	assert.Equal([]int64{b}, splitDisclosure.Changes.ParticipantIDs)
	assert.Equal(splitArgs["etag"], splitDisclosure.Changes.ETag)
	assert.Equal("synthetic-split", splitDisclosure.Changes.Key)
	assert.Contains(disclosure, "identical completed request returns its saved result")
	assert.Contains(disclosure, "Lineage labels are historical")
	differentKey := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": splitArgs["etag"], "idempotency_key": "different-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: differentKey})
	require.NoError(err)
	assert.Equal("person_profile_not_found", loopToolFailure(t, sdkResult)["error"])
	differentPayload := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{}, "etag": splitArgs["etag"], "idempotency_key": "synthetic-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: differentPayload})
	require.NoError(err)
	assert.Equal("person_split_idempotency_conflict", loopToolFailure(t, sdkResult)["error"])
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(err)
	detail = operationOutput[mcpserver.MergeDetail](t, result)
	require.Len(detail.Splits, 1)
	assert.Equal(split.Split.ID, detail.Splits[0].ID)
	c, err := st.EnsureParticipant("c@example.test", "C", "example.test")
	require.NoError(err)
	d, err := st.EnsureParticipant("d@example.test", "D", "example.test")
	require.NoError(err)
	left, _, err := st.CreatePersonFromParticipant(c)
	require.NoError(err)
	right, _, err := st.CreatePersonFromParticipant(d)
	require.NoError(err)
	for id, val := range map[int64]string{left.ID: "email", right.ID: "chat"} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &val}, Source: store.ProvenanceUser})
		require.NoError(err)
	}
	left, err = st.GetPersonContext(ctx, left.ID)
	require.NoError(err)
	right, err = st.GetPersonContext(ctx, right.ID)
	require.NoError(err)
	other, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: left.ID, AbsorbedID: right.ID, ExpectedSurvivorRevision: left.Revision, ExpectedAbsorbedRevision: right.Revision, IdempotencyKey: "second-merge", Actor: "test"})
	require.NoError(err)
	require.Len(other.ReviewCandidates, 1)
	acceptArgs := map[string]any{"person_id": left.ID, "merge_id": other.Merge.ID, "candidate_id": other.ReviewCandidates[0].ID, "decision": "accept", "etag": fmt.Sprintf(`"person-%d-r%d"`, other.Person.ID, other.Person.Revision)}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: acceptArgs})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Contains(disclosure, fmt.Sprintf("attribute definition %d", other.ReviewCandidates[0].DefinitionID))
	assert.Contains(disclosure, fmt.Sprintf("absorbed value %d", other.ReviewCandidates[0].AbsorbedValueID))
	assert.Contains(disclosure, "exact split becomes unavailable")
}

func TestMCPMergeRecoveryRejectsUnsupportedAndInvalid(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
			require.ErrorAs(err, &refusal)
			assert.Equal("invalid_arguments", refusal.Code)
			continue
		}
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(err)
		assertLoopRefusal(t, result, "invalid_arguments", false)
	}
}

// Synthetic daemon-version skew and large detail fixtures exercise adapter projection.
func TestMCPMergeRecoveryBoundsAndSafeErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge_snapshot", "get_person_merge"}}
	for _, id := range []int{1, 2} {
		result, err := backend.ExecuteOperation(t.Context(), "get_person_merge_snapshot", map[string]any{"merge_id": id})
		require.NoError(err)
		assertLoopRefusal(t, result, "invalid_operation_response", false)
	}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_merge_snapshot", map[string]any{"merge_id": 4})
	require.NoError(err)
	assertLoopRefusal(t, result, "person_merge_snapshot_corrupt", false)
	result, err = backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 3})
	require.NoError(err)
	detail := operationOutput[mcpserver.MergeDetail](t, result)
	assert.Equal(6001, detail.RowCount)
	encoded, err := json.Marshal(detail)
	require.NoError(err)
	assert.NotContains(string(encoded), `"rows"`)
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
