package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestAppreciationHTTPProjectsDismissalAndPreservesHistory(t *testing.T) {
	f := newPersonBriefAPIFixture(t)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "test-model",
			Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
			RetentionPosture: "zero_retention", TrainingPosture: "no_training", AllowSensitive: true,
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, RequestTimeout: time.Second,
		}}}
	config.ApplyDefaults()
	f.server.cfg.People.Sweep = config
	var participantID int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id = ?`), f.personID).Scan(&participantID))
	source, err := f.store.GetOrCreateSourceContext(t.Context(), "slack", "api-appreciation")
	require.NoError(t, err)
	conversationID, err := f.store.EnsureConversationWithTypeContext(t.Context(), source.ID, "room", "direct_chat", "Synthetic")
	require.NoError(t, err)
	messageID, err := f.store.UpsertMessageContext(t.Context(), &store.Message{SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "peer-help", MessageType: "chat", SenderID: sql.NullInt64{Int64: participantID, Valid: true},
		SentAt: sql.NullTime{Time: personBriefAPINow.Add(-time.Hour), Valid: true}})
	require.NoError(t, err)
	const body = "I carried the boxes upstairs."
	require.NoError(t, f.store.UpsertMessageBodyContext(t.Context(), messageID, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	key, err := peoplesweep.AppreciationContributionKey("slack", "api-appreciation", "peer-help")
	require.NoError(t, err)
	hash := sha256.Sum256([]byte(body))
	peerItems, err := f.store.HydratePersonSweepMessages(t.Context(), f.personID, []int64{messageID}, 0)
	require.NoError(t, err)
	require.Len(t, peerItems, 1)
	suggestion := peoplesweep.BriefThanksSuggestion{Text: "carrying the boxes", AnchorID: personBriefAPIEvidenceID,
		Quote: "carried the boxes", Key: key, State: "suggested", PriorThanks: "unknown"}
	output := personBriefAPIStructure()
	output.ThanksSuggestions = []peoplesweep.BriefThanksSuggestion{suggestion}
	seed := personBriefAPISeedFor(t, output, personBriefAPINow, []int64{f.evidence[0].ID})
	rendered, err := peoplesweep.RenderBrief(peoplesweep.ParsedBrief{Output: output}, peoplesweep.BriefWindow{})
	require.NoError(t, err)
	seed.rendered = rendered.Text
	boundary, err := json.Marshal(peoplesweep.BriefBoundary{AppreciationMemory: []peoplesweep.BriefAppreciationMemory{{
		Suggestion: suggestion, PeerRef: peerItems[0].Ref,
		PeerRawSHA256: hex.EncodeToString(hash[:]), ConversationID: conversationID,
	}}})
	require.NoError(t, err)
	seed.boundary = string(boundary)
	f.seedBrief(t, seed)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_briefs SET program_version = ?, renderer_policy = ? WHERE person_id = ?`), "v3", peoplesweep.BriefRendererPolicyV3, f.personID)
	require.NoError(t, err)
	base := fmt.Sprintf("/api/v1/people/%d/appreciations", f.personID)
	response := doRequest(t, f.server.Router(), http.MethodGet, base, nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), key)
	response = doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, ""), nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "carrying the boxes")
	f.server.cfg.People.Sweep.Enabled = false
	response = doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, ""), nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "carrying the boxes")
	assert.NotContains(t, response.Body.String(), key)
	assert.Contains(t, response.Body.String(), "they were preparing for a role change")
	assert.NotContains(t, response.Body.String(), "carried the boxes")
	response = doRequest(t, f.server.Router(), http.MethodGet, base, nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	f.server.cfg.People.Sweep.Enabled = true
	response = doRequest(t, f.server.Router(), http.MethodPost, base+"/"+key+"/dismiss", nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, ""), nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "carrying the boxes")
	assert.NotContains(t, response.Body.String(), key)
	response = doRequest(t, f.server.Router(), http.MethodGet, personBriefPath(f.personID, "/versions"), nil, nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "carrying the boxes")
	assert.Contains(t, response.Body.String(), `"historical_snapshot":true`)
	assert.Contains(t, response.Body.String(), `"appreciation_dismissal_keys":["`+key+`"]`)
	assert.NotContains(t, response.Body.String(), hex.EncodeToString(hash[:]))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_briefs SET program_version = ? WHERE person_id = ?`), "v4", f.personID)
	require.NoError(t, err)
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, base}, {http.MethodPost, base + "/" + key + "/dismiss"},
	} {
		response = doRequest(t, f.server.Router(), request.method, request.path, nil, nil)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "appreciations_unavailable")
	}
}
