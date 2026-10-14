package daemonclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCapabilitiesFailClosed(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
	}{
		{"old daemon", `{"error":"not_found"}`, http.StatusNotFound},
		{"unauthorized", `{"error":"unauthorized"}`, http.StatusUnauthorized},
		{"missing version", `{"routes":[]}`, http.StatusOK},
		{"unknown version", `{"version":2,"routes":[]}`, http.StatusOK},
		{"malformed", `{"version":1`, http.StatusOK},
		{"multiple values", `{"version":1} {"version":1}`, http.StatusOK},
		{"oversized", strings.Repeat(" ", 1<<20) + `{"version":1}`, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
}

func TestMCPCapabilitiesRetainsRouteParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, `{"version":1,"routes":[{"operation_id":"listSourceStatus","method":"GET","path":"/api/v1/sources/status","query_parameters":["source_type"]}]}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	requirements.Len(capabilities.Routes, 1)
	assertions.Equal("synthetic-key", <-seen)
	assertions.Equal([]string{"source_type"}, capabilities.Routes[0].QueryParameters)
}

func TestMCPCapabilitiesOwnerRouteIdentities(t *testing.T) {
	for name, body := range map[string]string{
		"missing operation":   `{"version":1,"routes":[{"method":"GET","path":"/api/v1/sources"}]}`,
		"missing method":      `{"version":1,"routes":[{"operation_id":"getSources","path":"/api/v1/sources"}]}`,
		"unsafe path":         `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"https://example.com"}]}`,
		"duplicate operation": `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"/api/v1/sources"},{"operation_id":"getSources","method":"POST","path":"/api/v1/sources"}]}`,
		"duplicate parameter": `{"version":1,"routes":[{"operation_id":"getSources","method":"GET","path":"/api/v1/sources","query_parameters":["limit","limit"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
}
