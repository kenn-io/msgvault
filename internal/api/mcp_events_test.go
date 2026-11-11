package api

import (
	"context"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/testutil"
)

type countingEventsGate struct{ calls int }

func (g *countingEventsGate) BeginWork() (func(), bool) { g.calls++; return func() {}, true }
func (g *countingEventsGate) BeginWorkContext(context.Context) (func(), bool) {
	g.calls++
	return func() {}, true
}

func TestEventsMutationsUseOperationGate(t *testing.T) {
	gate := &countingEventsGate{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, MCPEvents: &mcpevents.Service{}, OperationGate: gate, Logger: testLogger()})
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	srv.handleMCPEventsList(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	assert.Zero(t, gate.calls)
	srv.handleMCPEventsSubscribe(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body)))
	srv.handleMCPEventsUnsubscribe(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body)))
	assert.Equal(t, 2, gate.calls)
}

func TestOwnerEventsDisabledAndInvalidSubscription(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	events := &mcpevents.Service{Store: st, Home: cfg.HomeDir}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, Logger: testLogger()})
	request := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	w := request(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	cfg.MCPEvents.Enabled = true
	events.Enabled = true
	w = request(`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "-32602")
}

func TestMCPEventsRejectsDelegatedGrant(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	t.Cleanup(reg.Close)
	_, secret, _, err := reg.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "agent@example.com"}}, time.Time{})
	require.NoError(t, err)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/mcp-events/list"}, {http.MethodPost, "/api/v1/mcp-events/subscribe"}, {http.MethodPost, "/api/v1/mcp-events/unsubscribe"}} {
		r := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(`{}`))
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code, route.path)
	}
}
