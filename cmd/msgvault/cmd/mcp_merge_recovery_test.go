package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
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
	detail := operationOutput[generated.PersonMergeDetail](t, result)
	assert.Equal(t, merged.Merge.ID, detail.Merge.ID)
	result, err = backend.ExecuteOperation(ctx, "get_person_merge_snapshot", map[string]any{"merge_id": merged.Merge.ID})
	require.NoError(t, err)
	snapshot := operationOutput[mcpserver.MergeSnapshot](t, result)
	assert.Equal(t, merged.Merge.SnapshotSHA256, snapshot.SHA256)
	assert.Positive(t, snapshot.RowCount)
	projected, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.NotContains(t, string(projected), "display_name")
	assert.NotContains(t, string(projected), "columns")
	etag := fmt.Sprintf(`"person-%d-r%d"`, merged.Person.ID, merged.Person.Revision)
	args := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "reject", "etag": etag}
	disclosure, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", args)
	require.NoError(t, err)
	assert.Contains(t, disclosure, "reject")
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, "reject")
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
	encoded, err := json.Marshal(sdkResult.StructuredContent)
	require.NoError(t, err)
	var receipt mcpserver.MergeCandidateReceipt
	require.NoError(t, json.Unmarshal(encoded, &receipt))
	assert.Equal(t, "rejected", receipt.Candidate.State)
	assert.NotEmpty(t, receipt.ETag)
	result, err = backend.ExecuteOperation(ctx, "split_person_merge", map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": etag, "idempotency_key": "synthetic-split"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	failure, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.Contains(t, string(failure), "person_merge_revision_conflict")
	assert.Contains(t, string(failure), `"operation_may_have_completed":false`)
	current, err := st.GetPersonContext(ctx, survivor.ID)
	require.NoError(t, err)
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": fmt.Sprintf(`"person-%d-r%d"`, current.ID, current.Revision), "idempotency_key": "synthetic-split"}
	result, err = backend.ExecuteOperation(ctx, "split_person_merge", splitArgs)
	require.NoError(t, err)
	split := operationOutput[generated.PersonSplitResult](t, result)
	assert.Equal(t, merged.Merge.ID, split.Split.MergeID)
	assert.True(t, split.ExactReversal)
	disclosure, err = backend.OperationDisclosure(ctx, "split_person_merge", splitArgs)
	require.NoError(t, err)
	assert.Contains(t, disclosure, "idempotent replay")
	result, err = backend.ExecuteOperation(ctx, "split_person_merge", splitArgs)
	require.NoError(t, err)
	replay := operationOutput[generated.PersonSplitResult](t, result)
	assert.Equal(t, split.Split.ID, replay.Split.ID)
}

func TestMCPMergeRecoveryRejectsUnsupportedAndInvalid(t *testing.T) {
	backend := newDaemonMCPOperations(nil, nil)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 1})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	backend.supported = []string{"list_person_merges"}
	result, err = backend.ExecuteOperation(t.Context(), "list_person_merges", map[string]any{"person_id": 1, "limit": 501})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// The real daemon cannot produce arbitrary raw errors or a deliberately oversized
// response on demand; this transport fixture checks the adapter boundary.
func TestMCPMergeRecoveryBoundsAndSafeErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/person-merges/1/snapshot" {
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"private host path"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge_snapshot", "get_person_merge"}}
	result, err := backend.ExecuteOperation(t.Context(), "get_person_merge_snapshot", map[string]any{"merge_id": 1})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "operation_response_limit_exceeded")
	result, err = backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 1})
	require.NoError(t, err)
	require.True(t, result.IsError)
	data, err = json.Marshal(result.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "operation_refused")
	assert.NotContains(t, string(data), "private host path")
}

func TestMCPMergeRecoveryCapabilityRequiresEachContract(t *testing.T) {
	caps := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{
		{OperationID: "getPersonMerge", Method: http.MethodGet, Path: "/api/v1/person-merges/{merge_id}"},
		{OperationID: "getPersonProfile", Method: http.MethodGet, Path: "/api/v1/people/{id}"},
		{OperationID: "splitPersonMerge", Method: http.MethodPost, Path: "/api/v1/people/{id}/split", RequestProperties: []string{"merge_id"}},
	}}
	backend := newDaemonMCPOperations(nil, caps)
	assert.Contains(t, backend.capabilities(), "get_person_merge")
	assert.NotContains(t, backend.capabilities(), "split_person_merge")
	assert.NotContains(t, backend.capabilities(), "get_person_merge_snapshot")
	caps.Routes[2].RequestProperties = append(caps.Routes[2].RequestProperties, "participant_ids")
	backend = newDaemonMCPOperations(nil, caps)
	assert.Contains(t, backend.capabilities(), "split_person_merge")
}
