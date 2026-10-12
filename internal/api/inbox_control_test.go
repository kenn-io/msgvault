package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestInboxHandlerOwnsAuthentication(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []struct {
		name      string
		operation Operation
		body      string
	}{
		{"discovery", Operation{ID: "getMCPCapabilities", Method: http.MethodGet, Path: "/api/v1/mcp/capabilities"}, ""},
		{"control", Operation{ID: "controlInbox", Method: http.MethodPost, Path: "/api/v1/inbox/control"}, inboxStateBody},
	} {
		for _, credential := range []struct {
			name   string
			header string
			value  string
		}{
			{"missing headers", "", ""},
			{"invalid agent header", apiprotocol.AgentTokenHeader, "invalid-agent-token"},
			{"invalid owner header", "Authorization", "Bearer invalid-owner-key"},
		} {
			for _, admitted := range []bool{true, false} {
				policy := "denied"
				if admitted {
					policy = "admitted"
				}
				t.Run(endpoint.name+"/"+credential.name+"/"+policy, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					backend := &inboxHTTPStore{}
					srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
					t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
					calls := 0
					handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, operation Operation) bool {
						calls++
						assertions.Equal(endpoint.operation, operation)
						if !admitted {
							w.WriteHeader(http.StatusForbidden)
						}
						return admitted
					})
					r := httptest.NewRequest(endpoint.operation.Method, endpoint.operation.Path, strings.NewReader(endpoint.body))
					r.Header.Set("Content-Type", "application/json")
					if credential.header != "" {
						r.Header.Set(credential.header, credential.value)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					assertions.Equal(1, calls, "the caller authorizes the registered operation")
					if !admitted {
						assertions.Equal(http.StatusForbidden, w.Code)
						assertions.Empty(w.Body.String())
						assertions.Zero(backend.calls, "caller denial precedes archive access")
						return
					}
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					if endpoint.name == "discovery" {
						var descriptor apiprotocol.MCPCapabilities
						requirements.NoError(json.Unmarshal(w.Body.Bytes(), &descriptor))
						assertions.False(descriptor.Delegated)
						assertions.True(descriptor.HasInboxContract())
						assertions.Contains(descriptor.InboxOperations, "get-state")
						assertions.Zero(backend.calls, "discovery never dispatches control")
					} else {
						assertions.Equal(1, backend.calls)
						assertions.Equal(inboxcontrol.Principal{ID: "owner", Owner: true}, backend.principal)
					}
				})
			}
		}
	}
}

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
	_, secret, grant, err := srv.agentGrants.Issue("state-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
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

func TestInboxHTTPRechecksRevokedGrant(t *testing.T) {
	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	id, secret, _, err := srv.agentGrants.Issue("state-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
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

// The backend is confined to this predicate test; daemon tests cover native
// Store/provider resolution and dispatch.
func TestInboxHTTPGrantRequiresSelectedSourceLifetime(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, oldSecret, oldGrant, err := srv.agentGrants.Issue("old-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
	requirements.NoError(err)
	body := strings.Replace(inboxStateBody, `"source_id":1`, `"source_id":2`, 1)
	assertions.Equal(http.StatusForbidden, inboxHTTPCall(srv, body, "", oldSecret).Code)
	_, freshSecret, _, err := srv.agentGrants.Issue("fresh-reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 2, Type: "gmail", Identifier: "owner@example.test"}}, time.Time{})
	requirements.NoError(err)
	assertions.Equal(http.StatusOK, inboxHTTPCall(srv, body, "", freshSecret).Code)
	assertions.Equal(http.StatusOK, inboxHTTPCall(srv, body, "synthetic-owner-key", "").Code)
	assertions.True(oldGrant.MatchesSource(agentgrant.SourceRef{ID: 2, Type: "gmail", Identifier: "owner@example.test"}), "general archive grants retain tuple semantics")
}
