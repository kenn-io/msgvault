package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This backend exercises the HTTP authentication/authorization contract. Real
// Store, lease, receipt and dispatch behavior is covered by Service tests.
type inboxHTTPStore struct {
	mockStore

	calls               int
	principal           inboxcontrol.Principal
	beforeAuthorization func()
}

func (s *inboxHTTPStore) ControlInbox(ctx context.Context, request inboxcontrol.Request, principal inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, acquire func(context.Context) (func(), error)) (*inboxcontrol.Result, error) {
	s.calls++
	s.principal = principal
	if s.beforeAuthorization != nil {
		s.beforeAuthorization()
	}
	if err := authorize(ctx, principal, request); err != nil {
		return nil, err
	}
	return &inboxcontrol.Result{}, nil
}

const inboxStateBody = `{"operation":"get-state","target":{"source_id":1,"source_type":"gmail","source_identifier":"owner@example.test","account_id":"owner@example.test","scope":"message","item_id":7,"provider_id":"mail-7"}}`

func TestInboxControlOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	document := OpenAPIDocument()
	requirements.NotNil(document.Paths["/api/v1/inbox/control"])
	requirements.NotNil(document.Paths["/api/v1/inbox/control"].Post)
	assertions.Equal("controlInbox", document.Paths["/api/v1/inbox/control"].Post.OperationID)
}

func inboxHTTPCall(srv *Server, body, owner, agent string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/inbox/control", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if owner != "" {
		r.Header.Set("Authorization", "Bearer "+owner)
	}
	if agent != "" {
		r.Header.Set(apiprotocol.AgentTokenHeader, agent)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestInboxHTTPDelegationAndStrictJSON(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, grant, err := srv.agentGrants.Issue("state-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}})
	requirements.NoError(err)
	response := inboxHTTPCall(srv, inboxStateBody, "", secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(grant.ID, backend.principal.ID)
	assertions.False(backend.principal.Owner)
	for _, body := range []string{
		strings.Replace(inboxStateBody, `"operation":"get-state"`, `"operation":"archive","dry_run":true`, 1),
		strings.ReplaceAll(inboxStateBody, "owner@example.test", "other@example.test"),
	} {
		response := inboxHTTPCall(srv, body, "", secret)
		assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	}
	for _, body := range []string{inboxStateBody + inboxStateBody, strings.Replace(inboxStateBody, `"source_id":1`, `"source_id":null`, 1), strings.Replace(inboxStateBody, `"source_id":1`, `"source_id":1,"Source_ID":2`, 1), strings.Replace(inboxStateBody, `"source_id":1`, `"source_id":1,"unknown":true`, 1)} {
		calls := backend.calls
		response := inboxHTTPCall(srv, body, "synthetic-owner-key", "")
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
		assertions.Equal(calls, backend.calls)
	}
	response = inboxHTTPCall(srv, inboxStateBody, "synthetic-owner-key", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.True(backend.principal.Owner)
}

func TestInboxHTTPMoveNeedsDelegatedMovePermission(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("archive-agent", []agentgrant.Permission{
		agentgrant.PermissionInboxRead,
		agentgrant.PermissionInboxArchive,
	}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.test"}})
	requirements.NoError(err)
	body := `{"operation":"move","target":{"source_id":1,"source_type":"imap","source_identifier":"owner@example.test","account_id":"owner@example.test","scope":"message","item_id":7,"provider_id":"Archive|42","mailbox":"Archive","uidvalidity":9,"uid":42},"destination":{"id":"Trash","uidvalidity":17},"dry_run":true}`

	response := inboxHTTPCall(srv, body, "", secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	calls := backend.calls
	unarchiveBody := strings.Replace(body, `"operation":"move"`, `"operation":"unarchive"`, 1)
	response = inboxHTTPCall(srv, unarchiveBody, "", secret)
	assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assertions.Equal(calls, backend.calls, "invalid IMAP unarchive destinations must not reach the controller")
}

func TestInboxHTTPRechecksRevokedGrant(t *testing.T) {
	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	id, secret, _, err := srv.agentGrants.Issue("state-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}})
	require.NoError(t, err)
	backend.beforeAuthorization = func() { assert.True(t, srv.agentGrants.Revoke(id)) }
	response := inboxHTTPCall(srv, inboxStateBody, "", secret)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestInboxHTTPReadsAndPreviewsDoNotAcquireGate(t *testing.T) {
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "fixture sync")
	require.True(t, ok)
	defer release()
	backend := &inboxHTTPStore{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: backend, Logger: testLogger(), OperationGate: gate})
	for _, body := range []string{inboxStateBody, strings.Replace(inboxStateBody, `"operation":"get-state"`, `"operation":"archive","dry_run":true`, 1)} {
		response := inboxHTTPCall(srv, body, "", "")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
}

func TestLegacyInboxTagWriteCannotBypassPreview(t *testing.T) {
	backend := &tagsHTTPStore{}
	srv := NewServer(&config.Config{}, backend, nil, testLogger())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/messages/7/tags", strings.NewReader(`{"add":["Next"]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, 0, backend.calls)
}
