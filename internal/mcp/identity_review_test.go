package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIdentityReviewMCPToolsRequireDaemonCapabilityAndWriteOptIn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	without := toolsByName(t, rawListTools(t,
		ServeOptions{Engine: &querytest.MockEngine{}}, true))
	assert.NotContains(without, ToolListIdentityMatches)
	assert.NotContains(without, ToolAcceptIdentityMatch)

	backend := newIdentityReviewMCPBackend(t)
	readOnly := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend,
	}, false))
	assert.Contains(readOnly, ToolListIdentityMatches)
	assert.Contains(readOnly, ToolGetIdentityMatch)
	assert.NotContains(readOnly, ToolAcceptIdentityMatch)
	assert.NotContains(readOnly, ToolRejectIdentityMatch)

	generalWriteOnly := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend,
	}, true))
	assert.NotContains(generalWriteOnly, ToolAcceptIdentityMatch)
	profileWrites := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowProfileWrites: true,
	}, true))
	assert.NotContains(profileWrites, ToolAcceptIdentityMatch)

	decisionWrites := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
	}, true))
	assert.Contains(decisionWrites, ToolAcceptIdentityMatch)
	assert.NotContains(decisionWrites, ToolMergePerson)

	fullyWritable := toolsByName(t, rawListTools(t, ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
	}, true))
	for _, name := range []string{ToolListIdentityMatches, ToolGetIdentityMatch,
		ToolAcceptIdentityMatch, ToolRejectIdentityMatch} {
		require.Contains(fullyWritable, name)
	}
	assert.Equal(true, toolReadOnlyHint(t, fullyWritable[ToolListIdentityMatches]))
	assert.Equal(false, toolReadOnlyHint(t, fullyWritable[ToolAcceptIdentityMatch]))
}

func TestIdentityMutationToolsAreDestructive(t *testing.T) {
	for _, definition := range []toolDefinition{
		acceptIdentityMatchDefinition(),
		rejectIdentityMatchDefinition(),
		mergePersonDefinition(),
		approveCardDAVPublicationDefinition(),
		syncCardDAVDefinition(),
	} {
		t.Run(definition.name, func(t *testing.T) {
			require.NotNil(t, definition.annotations.DestructiveHint)
			assert.True(t, *definition.annotations.DestructiveHint)
		})
	}
}

func newIdentityReviewMCPBackend(t *testing.T) *daemonclient.Client {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	left, err := st.EnsureParticipantByIdentifier("beeper", "mcp-review-left", "MCP Review Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "mcp-review-right", "MCP Review Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID,
		store.IdentityMatchEvidenceInput{EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
			Detail: new("Both identifiers appear in a synthetic export.")})
	require.NoError(err)
	router := api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Participant detail is an analytics API; keep this fixture focused on
		// the daemon transport while review decisions use the real store/API.
		if strings.HasPrefix(r.URL.Path, "/api/v1/participants/") {
			w.Header().Set("Content-Type", "application/json")
			name, id := "MCP Review Left", left
			if r.URL.Path == fmt.Sprintf("/api/v1/participants/%d", right) {
				name, id = "MCP Review Right", right
			}
			_, _ = fmt.Fprintf(w, `{"id":%d,"display_name":%q,"display_label":%q,"identifiers":[]}`, id, name, name)
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return client
}

func TestIdentityReviewConfirmationNamesEvidenceAndSubmittedNotes(t *testing.T) {
	for _, decision := range []struct{ tool, state string }{
		{ToolAcceptIdentityMatch, "accepted"},
		{ToolRejectIdentityMatch, "rejected"},
	} {
		t.Run(decision.tool, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := newIdentityReviewMCPBackend(t)
			candidate, err := backend.GetIdentityMatch(t.Context(), 1)
			require.NoError(err)
			opts := ServeOptions{
				Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
				PeopleBackend: daemonclient.NewPeopleBrowser(daemonclient.NewEngineAdapter(backend)),
			}
			const notes = "Checked the synthetic export with the account owner."
			inspected := false
			result := confirmedCallTool(t, opts, decision.tool, map[string]any{
				"candidate_id": float64(1), "review_token": *candidate.ReviewToken, "notes": notes,
			}, true, func(message string) {
				inspected = true
				assert.Contains(message, "MCP Review Left")
				assert.Contains(message, "MCP Review Right")
				assert.Contains(message, "email")
				assert.Contains(message, "Both identifiers appear in a synthetic export.")
				assert.Contains(message, notes)
			})
			require.True(inspected)
			require.NotEqual(true, result["isError"], "%v", result)
			readback, err := backend.GetIdentityMatch(t.Context(), 1)
			require.NoError(err)
			assert.Equal(decision.state, readback.State)
			require.NotNil(readback.Notes)
			assert.Equal(notes, *readback.Notes)
		})
	}
}

func TestIdentityReviewMCPReadsEvidenceAndDecidesWithToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
	}
	listed := rawCallTool(t, opts, ToolListIdentityMatches, map[string]any{
		"state": "candidate", "limit": float64(1), "offset": float64(0),
	})
	assert.NotEqual(true, listed["isError"])
	rows, ok := toolStructuredContent(t, listed)["candidates"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.NotEmpty(row["evidence"])
	token, ok := row["review_token"].(string)
	require.True(ok)
	require.NotEmpty(token)

	stale := confirmedCallTool(t, opts, ToolAcceptIdentityMatch, map[string]any{
		"candidate_id": float64(1), "review_token": "stale-token",
	}, true)
	assert.Equal(true, stale["isError"])
	assert.Equal("daemon request failed (409, identity_match_review_stale)", toolErrorTextFromResult(t, stale))

	accepted := confirmedCallTool(t, opts, ToolAcceptIdentityMatch, map[string]any{
		"candidate_id": float64(1), "review_token": token,
	}, true)
	assert.NotEqual(true, accepted["isError"])
	decision := toolStructuredContent(t, accepted)
	decidedCandidate, ok := decision["candidate"].(map[string]any)
	require.True(ok)
	assert.Equal("accepted", decidedCandidate["state"])
	assert.Equal(false, decidedCandidate["application_pending"])

	readback := rawCallTool(t, opts, ToolGetIdentityMatch, map[string]any{"candidate_id": float64(1)})
	assert.Equal("accepted", toolStructuredContent(t, readback)["state"])
}

func TestIdentityDecisionRequiresClientConfirmation(t *testing.T) {
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true}
	result := confirmedCallTool(t, opts, ToolRejectIdentityMatch, map[string]any{
		"candidate_id": float64(1), "review_token": "token-from-current-review",
	}, false)
	assert.Equal(t, true, result["isError"])
	assert.Contains(t, fmt.Sprint(result), "explicit client confirmation")
}

func TestIdentityReviewMCPUsesSafeDaemonErrors(t *testing.T) {
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{ToolListIdentityMatches, map[string]any{}},
		{ToolGetIdentityMatch, map[string]any{"candidate_id": float64(1)}},
		{ToolAcceptIdentityMatch, map[string]any{"candidate_id": float64(1), "review_token": "reviewed-token"}},
		{ToolRejectIdentityMatch, map[string]any{"candidate_id": float64(1), "review_token": "reviewed-token"}},
	} {
		t.Run(tool.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, err := w.Write([]byte(`{"error":"identity_match_failed","message":"Synthetic daemon failure detail"}`))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{
				URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
			})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			result := confirmedCallTool(t, ServeOptions{
				Engine: &querytest.MockEngine{}, IdentityReview: client, AllowIdentityDecisions: true,
			}, tool.name, tool.args, true)
			assert.Equal(t, "daemon request failed (500, identity_match_failed)", toolErrorTextFromResult(t, result))
		})
	}
}

func TestIdentityDecisionConfirmationWorksAcrossAuthenticatedStatelessHTTPRequests(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
	}
	listed := rawCallTool(t, opts, ToolListIdentityMatches, map[string]any{
		"state": "candidate", "limit": float64(1), "offset": float64(0),
	})
	rows, ok := toolStructuredContent(t, listed)["candidates"].([]any)
	requires.True(ok)
	requires.Len(rows, 1)
	candidate, ok := rows[0].(map[string]any)
	requires.True(ok)
	arguments := map[string]any{"candidate_id": candidate["id"], "review_token": candidate["review_token"]}
	httpOpts := HTTPOptions{APIKey: "fixture-api-key", AllowWrites: true}
	handler := newMCPHTTPServer(opts, httpOpts).Handler
	call := func(id int, extra map[string]any) task3RPCResponse {
		t.Helper()
		params := map[string]any{"name": ToolAcceptIdentityMatch, "arguments": arguments,
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": task3ModernProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
		maps.Copy(params, extra)
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params})
		requires.NoError(err)
		req := task3ModernRequest("tools/call", ToolAcceptIdentityMatch, string(body))
		req.Header.Set("Authorization", "Bearer fixture-api-key")
		returnRecorder, response := task3Serve(handler, req)
		task3RequireSuccess(t, returnRecorder, response)
		return response
	}
	first := call(1, nil)
	requires.NotEmpty(first.Result["requestState"])
	requires.NotEmpty(first.Result["inputRequests"])
	second := call(2, map[string]any{
		"requestState": first.Result["requestState"],
		"inputResponses": map[string]any{"confirm": map[string]any{
			"action": "accept", "content": map[string]any{"confirm": true},
		}},
	})
	asserts.NotEqual(true, second.Result["isError"], "authenticated HTTP confirmation must survive the stateless request boundary: %#v", second)
}

func TestIdentityDecisionConfirmationWorksAcrossNoKeyStatelessHTTPRequests(t *testing.T) {
	asserts := assert.New(t)
	requires := require.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{
		Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true,
	}
	listed := rawCallTool(t, opts, ToolListIdentityMatches, map[string]any{
		"state": "candidate", "limit": float64(1), "offset": float64(0),
	})
	rows, ok := toolStructuredContent(t, listed)["candidates"].([]any)
	requires.True(ok)
	requires.Len(rows, 1)
	candidate, ok := rows[0].(map[string]any)
	requires.True(ok)
	arguments := map[string]any{"candidate_id": candidate["id"], "review_token": candidate["review_token"]}
	handler := newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler
	call := func(id int, extra map[string]any) task3RPCResponse {
		t.Helper()
		params := map[string]any{"name": ToolAcceptIdentityMatch, "arguments": arguments,
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": task3ModernProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
		maps.Copy(params, extra)
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params})
		requires.NoError(err)
		req := task3ModernRequest("tools/call", ToolAcceptIdentityMatch, string(body))
		_, response := task3Serve(handler, req)
		return response
	}
	first := call(1, nil)
	requires.NotEmpty(first.Result["requestState"])
	requires.NotEmpty(first.Result["inputRequests"])
	second := call(2, map[string]any{
		"requestState": first.Result["requestState"],
		"inputResponses": map[string]any{"confirm": map[string]any{
			"action": "accept", "content": map[string]any{"confirm": true},
		}},
	})
	asserts.NotEqual(true, second.Result["isError"], "no-key HTTP confirmation must survive stateless requests: %#v", second)
	readback := rawCallTool(t, opts, ToolGetIdentityMatch, map[string]any{"candidate_id": candidate["id"]})
	asserts.Equal("accepted", toolStructuredContent(t, readback)["state"])
}
