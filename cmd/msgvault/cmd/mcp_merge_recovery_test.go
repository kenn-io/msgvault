package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// mergeRecoveryFixture merges a person whose slug value is "email" with one whose value is "chat".
func mergeRecoveryFixture(t *testing.T, st *store.Store, slug string) (int64, *store.PersonMergeResult) {
	t.Helper()
	require := require.New(t)
	people := make([]*store.Person, 2)
	var participant int64
	for i, value := range []string{"email", "chat"} {
		var err error
		participant, err = st.EnsureParticipant(value+"-"+slug+"@example.test", value, "example.test")
		require.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(err)
		_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: &value}, Source: store.ProvenanceUser})
		require.NoError(err)
		people[i], err = st.GetPersonContext(t.Context(), person.ID)
		require.NoError(err)
	}
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: people[0].ID, AbsorbedID: people[1].ID, ExpectedSurvivorRevision: people[0].Revision, ExpectedAbsorbedRevision: people[1].Revision, IdempotencyKey: "merge-" + slug, Actor: "test"})
	require.NoError(err)
	require.Len(merged.ReviewCandidates, 1)
	return participant, merged
}

func TestMCPMergeRecoveryRealStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	b, merged := mergeRecoveryFixture(t, st, store.AttributeSlugPrimaryChannel)
	survivor := merged.Person
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	editETag := func(id int64) string {
		r, err := backend.ExecuteOperation(ctx, "get_person_edit_context", map[string]any{"person_id": id})
		require.NoError(err)
		require.False(r.IsError)
		require.NotEmpty(r.ETag)
		return r.ETag
	}
	result, err := backend.ExecuteOperation(ctx, "list_person_merges", map[string]any{"person_id": survivor.ID})
	require.NoError(err)
	list := operationOutput[generated.PersonMergesResponse](t, result)
	require.Len(list.Merges, 1)
	assert.Equal(merged.Merge.ID, list.Merges[0].Merge.ID)
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
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: survivor.ID, DefinitionSlug: store.AttributeSlugPrimaryChannel, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("phone")}, Source: store.ProvenanceUser})
	require.NoError(err)
	args["etag"] = editETag(survivor.ID)
	reject, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", args)
	require.NoError(err)
	assert.Contains(reject, "Keep the current value")
	assert.Contains(reject, "Primary channel")
	acceptArgs := map[string]any{"person_id": survivor.ID, "candidate_id": merged.ReviewCandidates[0].ID, "merge_id": merged.Merge.ID, "decision": "accept", "etag": args["etag"]}
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", acceptArgs)
	if assert.ErrorAs(err, &refusal) {
		assert.Equal("person_merge_candidate_state_changed", refusal.Code)
	}
	var values struct {
		CurrentValue  generated.AttributeValue `json:"current_value"`
		SurvivorValue generated.AttributeValue `json:"survivor_value"`
		AbsorbedValue generated.AttributeValue `json:"absorbed_value"`
	}
	require.NoError(json.Unmarshal([]byte(reject), &values))
	assert.Equal(new("phone"), values.CurrentValue.Text)
	assert.Equal(new("email"), values.SurvivorValue.Text)
	assert.Equal(new("chat"), values.AbsorbedValue.Text)
	var disclosure string
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyMergeRecovery}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		disclosure = request.Params.Message
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	sdkDetail, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_person_merge", Arguments: map[string]any{"merge_id": merged.Merge.ID}})
	require.NoError(err)
	detail := loopToolOutput[mcpserver.MergeDetail](t, sdkDetail)
	assert.Equal(merged.Merge.ID, detail.Merge.ID)
	assert.NotEmpty(detail.Rows)
	sdkResult, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Equal(1, approvals)
	assert.Equal(reject, disclosure)
	replay, err := backend.OperationDisclosure(ctx, "decide_person_merge_candidate", args)
	require.NoError(err)
	assert.Contains(replay, "daemon will replay it")
	assert.Contains(replay, "Primary channel")
	assert.Contains(replay, "email")
	assert.Contains(replay, "chat")
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	receipt := loopToolOutput[mcpserver.MergeCandidateReceipt](t, sdkResult)
	assert.Equal("rejected", receipt.Candidate.State)
	assert.NotEmpty(receipt.ETag)
	_, err = backend.OperationDisclosure(ctx, "decide_person_merge_candidate", acceptArgs)
	if assert.ErrorAs(err, &refusal) {
		assert.Equal("person_merge_candidate_state_changed", refusal.Code)
	}
	splitArgs := map[string]any{"person_id": survivor.ID, "merge_id": merged.Merge.ID, "participant_ids": []int64{b}, "etag": editETag(survivor.ID), "idempotency_key": "synthetic-split"}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "split_person_merge", Arguments: splitArgs})
	require.NoError(err)
	split := loopToolOutput[mcpserver.MergeSplitReceipt](t, sdkResult)
	assert.Equal(merged.Merge.ID, split.Split.MergeID)
	assert.True(split.ExactReversal)
	assert.Equal(editETag(split.SourcePerson.ID), split.ETag)
	assert.Equal(editETag(split.NewPerson.ID), split.NewPersonETag)
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: args})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Contains(disclosure, "daemon will replay it")
	assert.NotContains(disclosure, `"attribute"`)
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
	definition, err := st.CreateAttributeDefinitionContext(ctx, store.AttributeDefinitionInput{UniversalID: "test-preferred-medium", ObjectType: store.AttributeObjectPerson, Slug: "preferred_medium", Label: "Preferred medium", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser, UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true, IsDeletable: true})
	require.NoError(err)
	_, retired := mergeRecoveryFixture(t, st, definition.Slug)
	_, err = st.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, store.AttributeDefinitionUpdate{IsActive: new(false)})
	require.NoError(err)
	_, err = st.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: retired.Person.ID, DefinitionSlug: definition.Slug})
	require.NoError(err)
	retiredArgs := map[string]any{"person_id": retired.Person.ID, "candidate_id": retired.ReviewCandidates[0].ID, "merge_id": retired.Merge.ID, "decision": "reject", "etag": editETag(retired.Person.ID)}
	sdkResult, err = session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "decide_person_merge_candidate", Arguments: retiredArgs})
	require.NoError(err)
	require.False(sdkResult.IsError)
	assert.Contains(disclosure, "Preferred medium")
	assert.Contains(disclosure, `"current_value":null`)
}

// A fake daemon returns enough rows to exercise paging.
func TestMCPMergeRecoveryPaging(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ambiguousLong bool
	}{
		{"ambiguous rows continue", true},
		{"unrestored rows continue", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			long := strings.Repeat(`{"original_row_key":"row"},`, 500) + `{"original_row_key":"last"}`
			short := `{"original_row_key":"only"}`
			ambiguous, unrestored := short, long
			if tc.ambiguousLong {
				ambiguous, unrestored = long, short
			}
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/people/7/split" {
					w.Header().Set("ETag", "source")
					w.Header().Set("X-New-Person-Etag", "new")
					_, _ = w.Write([]byte(`{"ambiguous_rows":[` + ambiguous + `],"unrestored_rows":[` + unrestored + `]}`))
				} else {
					_, _ = w.Write([]byte(`{"rows":[` + long + `]}`))
				}
			})
			backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
			for _, offset := range []int{0, 500} {
				result, err := backend.ExecuteOperation(t.Context(), "get_person_merge", map[string]any{"merge_id": 1, "row_offset": offset})
				require.NoError(err)
				detail := operationOutput[mcpserver.MergeDetail](t, result)
				result, err = backend.ExecuteOperation(t.Context(), "split_person_merge", map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{8}, "etag": "x", "idempotency_key": "same", "row_offset": offset})
				require.NoError(err)
				split := operationOutput[mcpserver.MergeSplitReceipt](t, result)
				longPage, shortPage := split.UnrestoredRows, split.AmbiguousRows
				longCount, shortCount := split.UnrestoredRowCount, split.AmbiguousRowCount
				if tc.ambiguousLong {
					longPage, shortPage = split.AmbiguousRows, split.UnrestoredRows
					longCount, shortCount = split.AmbiguousRowCount, split.UnrestoredRowCount
				}
				assert.Equal(501, detail.RowCount)
				assert.Equal(501, longCount)
				assert.Equal(1, shortCount)
				if offset == 0 {
					assert.Len(detail.Rows, 500)
					assert.Len(longPage, 500)
					require.Len(shortPage, 1)
					assert.Equal("only", shortPage[0].OriginalRowKey)
					assert.Equal(new(500), detail.NextRowOffset)
					assert.Equal(new(500), split.NextRowOffset)
				} else {
					assert.Len(detail.Rows, 1)
					require.Len(longPage, 1)
					assert.Equal("last", longPage[0].OriginalRowKey)
					assert.Empty(shortPage)
					assert.Nil(detail.NextRowOffset)
					assert.Nil(split.NextRowOffset)
				}
			}
		})
	}
}

func TestMCPMergeRecoveryCapabilityRequiresDetailAndAttributeHistory(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, fixture)
	caps, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(err)
	assert.Contains(backend.capabilities(), "decide_person_merge_candidate")
	assert.Contains(backend.capabilities(), "split_person_merge")
	for _, tc := range []struct{ operation, missing, name string }{
		{"getAttributeDefinition", "route", "decide_person_merge_candidate"}, {"listPersonAttributes", "slug", "decide_person_merge_candidate"}, {"listPersonAttributes", "route", "decide_person_merge_candidate"}, {"listPersonAttributes", "history", "decide_person_merge_candidate"}, {"getPersonMerge", "route", "split_person_merge"},
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
		assert.NotContains(newDaemonMCPOperations(backend.client, &copyCaps).capabilities(), tc.name)
	}
}

func TestMCPMergeRecoveryLargeConfirmation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows":[{"original_row_key":"` + strings.Repeat("x", 16<<20) + `"}]}`))
	})
	backend := &daemonMCPOperations{client: client, supported: []string{"get_person_merge", "split_person_merge"}}
	args := map[string]any{"person_id": 7, "merge_id": 1, "participant_ids": []int64{8}, "etag": "x", "idempotency_key": "key"}
	disclosure, err := backend.OperationDisclosure(t.Context(), "split_person_merge", args)
	require.NoError(err)
	assert.Less(len(disclosure), 10000)
}

func TestMCPMergeRecoveryReadsOnlyCandidateHistory(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/person-merges/1":
			_, _ = w.Write([]byte(`{"review_candidates":[{"id":2,"person_id":7,"definition_id":3,"survivor_value_id":4,"absorbed_value_id":5,"state":"pending"}]}`))
		case "/api/v1/attribute-definitions/3":
			_, _ = w.Write([]byte(`{"id":3,"slug":"primary_channel","label":"Primary channel"}`))
		case "/api/v1/people/7/attributes":
			if r.URL.Query().Get("history") != "true" || r.URL.Query().Get("slug") != "primary_channel" {
				_, _ = w.Write([]byte(strings.Repeat("x", 1<<20+1)))
				return
			}
			_, _ = w.Write([]byte(`{"attributes":[{"definition":{"id":3,"slug":"primary_channel","label":"Primary channel"},"current":[{"id":4,"value":{"type":"text","text":"email"}}],"history":[{"id":5,"source_ref":"` + strings.Repeat("x", 1<<20) + `","value":{"type":"text","text":"chat"}}]}]}`))
		default:
			assert.Fail("unexpected path", r.URL.Path)
		}
	})
	backend := &daemonMCPOperations{client: client, supported: []string{"decide_person_merge_candidate"}}
	disclosure, err := backend.OperationDisclosure(t.Context(), "decide_person_merge_candidate", map[string]any{"person_id": 7, "candidate_id": 2, "merge_id": 1, "decision": "accept", "etag": "x"})
	require.NoError(err)
	assert.Contains(disclosure, "email")
	assert.Contains(disclosure, "chat")
}
