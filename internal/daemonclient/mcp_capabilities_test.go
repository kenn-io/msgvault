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
		name, body, schemaVersion string
		status                    int
		wantDiscovery             bool
	}{
		{"old daemon", `{"error":"not_found"}`, "2.27.0", http.StatusNotFound, false},
		{"unauthorized", `{"error":"unauthorized"}`, "3.15.0", http.StatusUnauthorized, true},
		{"missing version", `{"routes":[]}`, "3.15.0", http.StatusOK, true},
		{"unknown version", `{"version":2,"routes":[]}`, "3.15.0", http.StatusOK, true},
		{"malformed", `{"version":1`, "3.15.0", http.StatusOK, true},
		{"multiple values", `{"version":1} {"version":1}`, "3.15.0", http.StatusOK, true},
		{"oversized", strings.Repeat(" ", 1<<20) + `{"version":1}`, "3.15.0", http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			discoveryRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = io.WriteString(w, `{"api_schema_version":"`+tt.schemaVersion+`"}`)
				case "/api/v1/mcp/capabilities":
					discoveryRequests++
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, tt.body)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
			wantDiscoveryRequests := 0
			if tt.wantDiscovery {
				wantDiscoveryRequests = 1
			}
			assertions.Equal(wantDiscoveryRequests, discoveryRequests, "descriptor validation must reach the capability route")
		})
	}
}

func TestMCPCapabilitiesRetainsRouteParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = io.WriteString(w, `{"api_schema_version":"3.15.0"}`)
		case "/api/v1/mcp/capabilities":
			seen <- r.Header.Get("X-Api-Key")
			_, _ = io.WriteString(w, `{"version":1,"routes":[{"operation_id":"listSourceStatus","method":"GET","path":"/api/v1/sources/status","query_parameters":["source_type"]}]}`)
		default:
			http.NotFound(w, r)
		}
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
			discoveryRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = io.WriteString(w, `{"api_schema_version":"3.15.0"}`)
				case "/api/v1/mcp/capabilities":
					discoveryRequests++
					_, _ = io.WriteString(w, body)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
			assertions.Equal(1, discoveryRequests, "invalid route identity must be rejected after capability discovery")
		})
	}
}
