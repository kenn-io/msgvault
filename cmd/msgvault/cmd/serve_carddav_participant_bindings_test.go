package cmd

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVParticipantBindingDaemonAdapter(t *testing.T) {
	st := testutil.NewTestStore(t)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	server := api.NewServer(cfg, &storeAPIAdapter{store: st}, nil, slog.New(slog.DiscardHandler))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/carddav/participant-bindings", bytes.NewBufferString(`{"dry_run":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"cards_scanned":0`)
}
