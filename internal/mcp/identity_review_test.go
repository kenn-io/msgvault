package mcp

import (
	"fmt"
	"log/slog"
	"net/http/httptest"
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
		store.IdentityMatchEvidenceInput{EvidenceKind: "email", Source: store.ProvenanceArchiveObservation})
	require.NoError(err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil,
		slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return client
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

func TestIdentityDecisionRequiresFreshUserConfirmation(t *testing.T) {
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityReview: backend, AllowIdentityDecisions: true}
	result := confirmedCallTool(t, opts, ToolRejectIdentityMatch, map[string]any{
		"candidate_id": float64(1), "review_token": "token-from-current-review",
	}, false)
	assert.Equal(t, true, result["isError"])
	assert.Contains(t, fmt.Sprint(result), "explicit user confirmation")
}
