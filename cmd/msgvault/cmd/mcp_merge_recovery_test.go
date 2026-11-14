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
	_, err = backend.OperationDisclosure(ctx, "split_person_merge", map[string]any{"person_id": survivor.ID, "merge_id": int64(999999), "participant_ids": []int64{b}, "etag": etag, "idempotency_key": "missing-split"})
	require.ErrorAs(err, &refusal)
	assert.Equal("person_merge_not_found", refusal.Code)
	accept, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "accept", "etag": etag})
	require.NoError(err)
	for _, want := range []string{"Primary channel", "email", "chat", "exact split becomes unavailable"} {
		assert.Contains(accept, want)
	}
	reject, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", args)
	require.NoError(err)
	assert.Contains(reject, "Keep the current value")
	var disclosure string
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		disclosure = request.Params.Message
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	sdkDetail, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_person_merge", Arguments: map[string]any{"merge_id": merged.Merge.ID}})
	require.NoError(err)
	assert.NotEmpty(loopToolOutput[mcpserver.MergeDetail](t, sdkDetail).Rows)
	sdkResult, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Equal(1, approvals)
	assert.Equal(reject, disclosure)
	replayDisclosure, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", args)
	require.NoError(err)
	assert.Contains(replayDisclosure, "daemon will replay it")
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	assert.Equal(replayDisclosure, disclosure)
	receipt := loopToolOutput[mcpserver.MergeCandidateReceipt](t, sdkResult)
	assert.Equal("rejected", receipt.Candidate.State)
	assert.NotEmpty(receipt.ETag)
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": editETag(survivor.ID), "idempotency_key": "synthetic-split"}
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
	assert.Greater(approvals, previousApprovals)
	require.False(sdkResult.IsError)
	var shown struct {
		Lineage mcpserver.MergeDetail `json:"lineage"`
	}
	require.NoError(json.Unmarshal([]byte(disclosure), &shown))
	assert.Equal(merged.Merge.ID, shown.Lineage.Merge.ID)
	require.Len(shown.Lineage.Splits, 1)
	assert.Contains(disclosure, "identical completed request returns its saved result")
}

// A fake daemon returns enough rows to exercise paging.
func TestMCPMergeRecoveryPaging(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	rows := strings.Repeat(`{"original_row_key":"row"},`, 500) + `{"original_row_key":"last"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/people/7/split" {
			w.Header().Set("ETag", "source")
			w.Header().Set("X-New-Person-Etag", "new")
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
		assert.Equal(501, detail.RowCount)
		result, err = backend.ExecuteOperation(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": "same", "row_offset": offset})
		require.NoError(err)
		split := operationOutput[mcpserver.MergeSplitReceipt](t, result)
		assert.Equal(501, split.UnrestoredRowCount)
		assert.Equal(501, split.AmbiguousRowCount)
		if offset == 0 {
			assert.Len(detail.Rows, 500)
			assert.Len(split.UnrestoredRows, 500)
			assert.Len(split.AmbiguousRows, 500)
			assert.Equal("row", split.UnrestoredRows[0].OriginalRowKey)
			assert.Equal("row", split.AmbiguousRows[0].OriginalRowKey)
			require.NotNil(detail.NextRowOffset)
			require.NotNil(split.NextRowOffset)
			assert.Equal(500, *detail.NextRowOffset)
			assert.Equal(500, *split.NextRowOffset)
		} else {
			assert.Len(detail.Rows, 1)
			assert.Len(split.UnrestoredRows, 1)
			assert.Len(split.AmbiguousRows, 1)
			assert.Equal("last", split.UnrestoredRows[0].OriginalRowKey)
			assert.Equal("last", split.AmbiguousRows[0].OriginalRowKey)
			assert.Nil(detail.NextRowOffset)
			assert.Nil(split.NextRowOffset)
		}
	}
}

func TestMCPMergeRecoveryCapabilityRequiresDetailAndAttributeHistory(t *testing.T) {
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, fixture)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	assert.Contains(t, backend.capabilities(), "decide_person_merge_candidate")
	assert.Contains(t, backend.capabilities(), "split_person_merge")
	for _, tc := range []struct{ operation, missing, name string }{
		{"listPersonAttributes", "route", "decide_person_merge_candidate"}, {"listPersonAttributes", "history", "decide_person_merge_candidate"}, {"getPersonMerge", "route", "split_person_merge"},
	} {
		copyCaps := *caps
		copyCaps.Routes = slices.Clone(caps.Routes)
		for i := range copyCaps.Routes {
			route := &copyCaps.Routes[i]
			if route.OperationID == tc.operation && tc.missing == "route" {
				route.Method = http.MethodDelete
			} else if route.OperationID == tc.operation {
				route.QueryParameters = slices.DeleteFunc(slices.Clone(route.QueryParameters), func(v string) bool { return v == tc.missing })
			}
		}
		assert.NotContains(t, newDaemonMCPOperations(backend.client, &copyCaps).capabilities(), tc.name)
	}
}

func TestMCPMergeRecoveryLargeConfirmation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"original_row_key":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", 16<<20)))
		_, _ = w.Write([]byte(`"}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{}, "etag": "x", "idempotency_key": "key"})
	require.NoError(err)
	assert.Contains(disclosure, "row_count")
	assert.Less(len(disclosure), 10000)
}

func TestMCPMergeRecoveryWriteRefusals(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		uncertain  bool
	}{
		{"person_merge_already_split", "person_merge_already_split", false}, {"profile_value_not_found", "profile_value_not_found", false},
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
