package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOwnerEventsDisabledAndInvalidSubscription(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	srv := NewServer(cfg, st, nil, testLogger())
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
	w = request(`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "-32602")
}
