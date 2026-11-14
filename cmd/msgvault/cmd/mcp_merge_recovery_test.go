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
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	editETag := func(id int64) string {
		r, err := backend.ExecuteOperation(ctx, "get_person_edit_context", map[string]any{"person_id": id})
		require.NoError(err)
		require.False(r.IsError)
		require.NotEmpty(r.ETag)
		return r.ETag
	}
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-merge", Actor: "test"})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	result, err := backend.ExecuteOperation(ctx, "list_person_merges", map[string]any{"person_id": survivor.ID})
	require.NoError(err)
	list := operationOutput[generated.PersonMergesResponse](t, result)
	require.Len(list.Merges, 1)
	assert.Equal(merged.Merge.ID, list.Merges[0].Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(err)
	detail := operationOutput[mcpserver.MergeDetail](t, result)
	assert.Equal(merged.Merge.ID, detail.Merge.ID)
	assert.NotEmpty(detail.Rows)
	etag := editETag(merged.Person.ID)
	args := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	var refusal *mcpserver.OperationRefusalError
	missing := map[string]any{"person_id": survivor.ID, "candidate_id": int64(999999), "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", missing)
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_candidate_not_found", refusal.Code)
	mismatch := map[string]any{"person_id": absorbed.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", mismatch)
	require.ErrorAs(err, &refusal)
	assert.Equal("person_profile_not_found", refusal.Code)
	_, err = backend.OperationDisclosure(ctx, "split_person_merge", map[string]any{"person_id": survivor.ID, "merge_id": int64(999999), "participant_ids": []int64{b}, "etag": etag, "idempotency_key": "missing-split"})
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_not_found", refusal.Code)
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
	assert.Contains(names, "decide_person_merge_candidate")
	sdkDetail, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_person_merge", Arguments: map[string]any{"merge_id": merged.Merge.ID}})
	require.NoError(err)
	assert.NotEmpty(loopToolOutput[mcpserver.MergeDetail](t, sdkDetail).Rows)
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
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": editETag(survivor.ID), "idempotency_key": "synthetic-split"}
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
	assert.Equal(editETag(split.SourcePerson.ID), split.ETag)
	assert.Equal(editETag(split.NewPerson.ID), split.NewPersonETag)
	thirdParticipant, err := st.EnsureParticipant("third@example.test", "Third", "example.test")
	require.NoError(err)
	third, _, err := st.CreatePersonFromParticipant(thirdParticipant)
	require.NoError(err)
	current, err := st.GetPersonContext(ctx, survivor.ID)
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
	acceptArgs := map[string]any{"person_id": left.ID, "merge_id": other.Merge.ID, "candidate_id": other.ReviewCandidates[0].ID, "decision": "accept", "etag": editETag(left.ID)}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: acceptArgs})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Contains(disclosure, "Primary channel")
	assert.Contains(disclosure, "email")
	assert.Contains(disclosure, "chat")
	assert.Contains(disclosure, "exact split becomes unavailable")
	accepted := loopToolOutput[mcpserver.MergeCandidateReceipt](t, sdkResult)
	require.NotEmpty(accepted.ETag)
	reviewedSplit := map[string]any{"person_id": left.ID, "merge_id": other.Merge.ID, "participant_ids": []int64{d}, "etag": accepted.ETag, "idempotency_key": "reviewed-split"}
	previousApprovals = approvals
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: reviewedSplit})
	require.NoError(err)
	assert.Greater(approvals, previousApprovals)
	reviewedFailure := loopToolFailure(t, sdkResult)
	assert.Equal("person_split_reviewed_candidates", reviewedFailure["error"])
	reviewedUncertain, ok := reviewedFailure["operation_may_have_completed"].(bool)
	require.True(ok)
	assert.False(reviewedUncertain)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge", map[string]any{"merge_id": other.Merge.ID})
	require.NoError(err)
	assert.Empty(operationOutput[mcpserver.MergeDetail](t, result).Splits)
}

func TestMCPMergeRecoveryRejectsInvalidArguments(t *testing.T) {
	require := require.New(t)
	backend := &daemonMCPOperations{supported: []string{"list_person_merges", "get_person_merge", "decide_person_merge_candidate", "split_person_merge"}}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_person_merges", map[string]any{"person_id": "1"}},
		{"get_person_merge", map[string]any{"merge_id": 1, "extra": true}},
		{"decide_person_merge_candidate", map[string]any{"person_id": "x"}},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		require.NoError(err)
		assertLoopRefusal(t, result, "invalid_arguments", false)
	}
}

// A fake daemon returns enough rows to exercise paging and idempotent replay.
func TestMCPMergeRecoveryPaging(t *testing.T) {
	require := require.New(t)
	rows := strings.Repeat(`{"original_row_key":"row"},`, 500) + `{"original_row_key":"last"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/people/7/split" {
			w.Header().Set("ETag", "source")
			w.Header().Set("X-New-Person-ETag", "new")
			_, _ = w.Write([]byte(`{"ambiguous_rows":[` + rows + `],"unrestored_rows":[` + rows + `]}`))
		} else {
			_, _ = w.Write([]byte(`{"rows":[` + rows + `]}`))
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
	for _, offset := range []int{0, 500} {
		result, err := backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 1, "row_offset": offset})
		require.NoError(err)
		detail := operationOutput[mcpserver.MergeDetail](t, result)
		assert.Equal(t, 501, detail.RowCount)
		result, err = backend.ExecuteOperation(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": "same", "row_offset": offset})
		require.NoError(err)
		split := operationOutput[mcpserver.MergeSplitReceipt](t, result)
		assert.Equal(t, 501, split.UnrestoredRowCount)
		assert.Equal(t, 501, split.AmbiguousRowCount)
		if offset == 0 {
			assert.Len(t, detail.Rows, 500)
			assert.Len(t, split.UnrestoredRows, 500)
			assert.Len(t, split.AmbiguousRows, 500)
			require.NotNil(detail.NextRowOffset)
			require.NotNil(split.NextRowOffset)
			assert.Equal(t, 500, *detail.NextRowOffset)
			assert.Equal(t, 500, *split.NextRowOffset)
		} else {
			assert.Len(t, detail.Rows, 1)
			assert.Len(t, split.UnrestoredRows, 1)
			assert.Len(t, split.AmbiguousRows, 1)
			assert.Nil(t, detail.NextRowOffset)
			assert.Nil(t, split.NextRowOffset)
		}
	}
}

func TestMCPMergeRecoveryCapabilityRequiresEachContract(t *testing.T) {
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, fixture)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct{ operation, missing, name string }{
		{"listPersonAttributes", "route", "decide_person_merge_candidate"}, {"listPersonAttributes", "history", "decide_person_merge_candidate"}, {"getPersonMerge", "route", "decide_person_merge_candidate"}, {"getPersonMerge", "route", "split_person_merge"},
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
			} else if tc.operation == "listPersonMerges" || tc.operation == "listPersonAttributes" {
				route.QueryParameters = slices.DeleteFunc(slices.Clone(route.QueryParameters), func(v string) bool { return v == tc.missing })
			} else {
				route.RequestProperties = slices.DeleteFunc(slices.Clone(route.RequestProperties), func(v string) bool { return v == tc.missing })
			}
		}
		assert.NotContains(t, newDaemonMCPOperations(backend.client, &copyCaps).capabilities(), tc.name)
	}
}

func TestMCPMergeRecoveryLargeConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"original_row_key":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", 16<<20)))
		_, _ = w.Write([]byte(`"}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": "key"})
	require.NoError(t, err)
	assert.Contains(t, disclosure, "row_count")
	assert.Less(t, len(disclosure), 10000)
}

func TestMCPMergeRecoveryWriteRefusals(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		uncertain  bool
	}{
		{"server_busy", "server_busy", false}, {"invalid_if_match", "invalid_if_match", false},
		{"person_merge_already_split", "person_merge_already_split", false}, {"profile_value_not_found", "profile_value_not_found", false},
		{"person_merge_failed", "operation_refused", true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"error":%q}`, tc.code)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			_, _, failure, err := readMCPOperationBytes(t.Context(), client, http.MethodPost, "/api/v1/people/{id}/split", &generated.SplitPersonMergeRequestOptions{PathParams: &generated.SplitPersonMergePath{ID: 7}}, mcpserver.MaxDownloadBytes, nil, http.StatusOK)
			require.NoError(t, err)
			assertLoopRefusal(t, failure, tc.want, tc.uncertain)
		})
	}
}

func TestMCPMergeRecoveryResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for range 257 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 1})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "operation_response_limit_exceeded", false)
	result, err = backend.ExecuteOperation(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": "key"})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "operation_response_limit_exceeded", true)
}
