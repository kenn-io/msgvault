package api

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmailFamilyHTTPReadsAndErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	source, err := st.GetOrCreateSource("imap", "family@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	for _, tc := range []struct {
		path   string
		status int
	}{{fmt.Sprintf("/api/v1/messages/%d/family", id), 200}, {"/api/v1/messages/999999/family", 404}, {"/api/v1/messages/invalid/family", 400}, {fmt.Sprintf("/api/v1/messages/%d/family?source_id=99999", id), 404}, {fmt.Sprintf("/api/v1/messages/%d/family?source_id=bad", id), 400}} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		assert.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == 200 {
			assert.Contains(w.Body.String(), "anchor_message_id")
			assert.Equal("no-store", w.Header().Get("Cache-Control"))
		}
	}
}

func TestEmailFamilyHTTPRequiresOwner(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	_, secret, _, err := reg.Issue("family-read", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(t, err)
	for _, tc := range []struct {
		header, value string
		status        int
	}{{"", "", 401}, {apiprotocol.AgentTokenHeader, secret, 401}, {"Authorization", "Bearer owner-key", 503}} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1/family", nil)
		request.RemoteAddr = "192.0.2.2:4242"
		if tc.header != "" {
			request.Header.Set(tc.header, tc.value)
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assert.Equal(t, tc.status, response.Code, response.Body.String())
	}
}
