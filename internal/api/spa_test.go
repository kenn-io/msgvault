package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	webapp "go.kenn.io/msgvault/internal/web"
)

func TestSPARoutePreservesRegisteredAPIPriorityAndServesNavigation(t *testing.T) {
	assert := assert.New(t)
	shell := []byte("<!doctype html><title>test shell</title>")
	spa := webapp.NewHandler(fstest.MapFS{
		"index.html": &fstest.MapFile{Data: shell},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No route matches "+r.Method+" "+r.URL.Path)
	}))
	srv := NewServerWithOptions(ServerOptions{
		Config:     &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger:     testLogger(),
		SPAHandler: spa,
	})

	health := httptest.NewRecorder()
	srv.Router().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(http.StatusOK, health.Code)
	assert.Contains(health.Header().Get("Content-Type"), "application/json")
	assert.NotContains(health.Body.String(), "test shell")

	navigation := httptest.NewRecorder()
	srv.Router().ServeHTTP(navigation, httptest.NewRequest(http.MethodGet, "/not/a/real/route", nil))
	assert.Equal(http.StatusOK, navigation.Code)
	assert.Equal("text/html; charset=utf-8", navigation.Header().Get("Content-Type"))
	assert.Equal(string(shell), navigation.Body.String())

	unknownAPI := httptest.NewRecorder()
	srv.Router().ServeHTTP(unknownAPI, httptest.NewRequest(http.MethodGet, "/api/v1/not-a-real-route", nil))
	assert.Equal(http.StatusNotFound, unknownAPI.Code)
	assert.Contains(unknownAPI.Header().Get("Content-Type"), "application/json")
	var response ErrorResponse
	require.NoError(t, json.NewDecoder(unknownAPI.Body).Decode(&response))
	assert.Equal("not_found", response.Error)
}
