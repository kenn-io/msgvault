package cmd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailFamilyEndpointServesThroughProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-owner@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "family-thread", "Family fixture")
	require.NoError(err)
	messageID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "family-seed", MessageType: "email"},
		RawMIME: []byte("Message-ID: <family-seed@example.test>\r\n\r\n"),
	})
	require.NoError(err)

	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{},
		Store:  &storeAPIAdapter{store: st},
		Engine: engine,
		Logger: slog.New(slog.DiscardHandler),
	})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)

	resp, err := http.Get(httpSrv.URL + fmt.Sprintf("/api/v1/messages/%d/family", messageID))
	require.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(http.StatusOK, resp.StatusCode, "the daemon adapter must expose the email-family capability")

	var family store.EmailFamily
	require.NoError(json.NewDecoder(resp.Body).Decode(&family))
	assert.Equal(messageID, family.AnchorMessageID)
	require.Len(family.Members, 1)
	assert.Equal(messageID, family.Members[0].MessageID)

	post := func(path string, body any) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(err)
		resp, err := http.Post(httpSrv.URL+path, "application/json", strings.NewReader(string(encoded)))
		require.NoError(err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	input := store.EmailFamilyDecisionInput{
		ActorKind:  "owner",
		Kind:       "handled",
		MessageIDs: []int64{messageID},
		RequestKey: strings.Repeat("a", 32),
	}
	previewResponse := post("/api/v1/messages/family/decisions/preview", input)
	require.Equal(http.StatusOK, previewResponse.StatusCode, "the daemon adapter must expose decision preview")
	var preview store.EmailFamilyDecisionPreview
	require.NoError(json.NewDecoder(previewResponse.Body).Decode(&preview))
	require.NotEmpty(preview.BeforeRevision)

	input.ExpectedFamilyRevision = preview.BeforeRevision
	applyResponse := post("/api/v1/messages/family/decisions", input)
	require.Equal(http.StatusOK, applyResponse.StatusCode, "the daemon adapter must expose decision apply")
	var applied store.EmailFamilyDecisionResult
	require.NoError(json.NewDecoder(applyResponse.Body).Decode(&applied))
	require.Equal("active", applied.Decision.State)

	getResponse, err := http.Get(httpSrv.URL + fmt.Sprintf("/api/v1/messages/family/decisions/%d", applied.Decision.ID))
	require.NoError(err)
	t.Cleanup(func() { _ = getResponse.Body.Close() })
	require.Equal(http.StatusOK, getResponse.StatusCode, "the daemon adapter must expose decision reads")
	var read store.EmailFamilyDecisionResult
	require.NoError(json.NewDecoder(getResponse.Body).Decode(&read))
	assert.Equal(applied.Decision.ID, read.Decision.ID)

	listResponse, err := http.Get(httpSrv.URL + fmt.Sprintf("/api/v1/messages/family/decisions?message_id=%d", messageID))
	require.NoError(err)
	t.Cleanup(func() { _ = listResponse.Body.Close() })
	require.Equal(http.StatusOK, listResponse.StatusCode, "the daemon adapter must expose decision history")
	var history []store.EmailFamilyDecision
	require.NoError(json.NewDecoder(listResponse.Body).Decode(&history))
	require.Len(history, 1)
	assert.Equal(applied.Decision.ID, history[0].ID)

	undoResponse := post(fmt.Sprintf("/api/v1/messages/family/decisions/%d/undo", applied.Decision.ID), api.EmailFamilyDecisionUndo{
		ExpectedRevision:       applied.Decision.Revision,
		ExpectedFamilyRevision: applied.Family.Revision,
	})
	require.Equal(http.StatusOK, undoResponse.StatusCode, "the daemon adapter must expose decision undo")
	var undone store.EmailFamilyDecisionResult
	require.NoError(json.NewDecoder(undoResponse.Body).Decode(&undone))
	assert.Equal("undone", undone.Decision.State)
}
