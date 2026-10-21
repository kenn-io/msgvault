package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmailFamilyHTTPReadsAndErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	source, err := st.GetOrCreateSource("imap", "family@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"},
		RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n"),
	})
	require.NoError(err)
	for _, tc := range []struct {
		path   string
		status int
	}{
		{fmt.Sprintf("/api/v1/messages/%d/family", id), http.StatusOK},
		{"/api/v1/messages/999999/family", http.StatusNotFound},
		{"/api/v1/messages/invalid/family", http.StatusBadRequest},
		{"/api/v1/messages/0/family", http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		assert.Equal(tc.status, w.Code, tc.path+" "+w.Body.String())
		assert.Equal("no-store", w.Header().Get("Cache-Control"), tc.path)
	}
}

type failingEmailFamilyStore struct {
	*stubIdentityCacheStore
}

func (failingEmailFamilyStore) EmailFamilyContext(context.Context, int64) (*store.EmailFamily, error) {
	return nil, errors.New("database unavailable")
}

func TestEmailFamilyHTTPDatabaseFailureIsNotNotFound(t *testing.T) {
	srv, st := newIdentityLinkTestServer(t)
	srv.store = failingEmailFamilyStore{st}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/messages/1/family", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "email_family_unavailable")
}

func TestEmailFamilyHTTPRequiresOwner(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	_, secret, _, err := reg.Issue("family-read", []agentgrant.Permission{agentgrant.PermissionMessageRead},
		[]agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, header, value string
		status              int
	}{
		{"anonymous", "", "", http.StatusUnauthorized},
		{"delegated message.read grant", apiprotocol.AgentTokenHeader, secret, http.StatusUnauthorized},
		{"owner without a backend", "Authorization", "Bearer owner-key", http.StatusServiceUnavailable},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/messages/1/family", nil)
		request.RemoteAddr = "192.0.2.2:4242"
		if tc.header != "" {
			request.Header.Set(tc.header, tc.value)
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assert.Equal(t, tc.status, response.Code, tc.name+" "+response.Body.String())
	}
}
