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
		{"missing version", `{"routes":[],"commands":[]}`, http.StatusOK},
		{"unknown version", `{"version":2,"routes":[],"commands":[]}`, http.StatusOK},
		{"malformed", `{"version":1`, http.StatusOK},
		{"multiple values", `{"version":1} {"version":1}`, http.StatusOK},
		{"oversized", strings.Repeat(" ", 1<<20) + `{"version":1}`, http.StatusOK},
		{"borrowed owner projection", `{"version":1,"delegated":false,"routes":[],"commands":[]}`, http.StatusOK},
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
			client, err := New(Config{URL: server.URL, AgentToken: "synthetic-token", AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
}

func TestMCPCapabilitiesRetainsActualModeAndParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Msgvault-Agent-Token")
		_, _ = io.WriteString(w, `{"version":1,"delegated":true,"routes":[],"commands":[{"name":"draft-compose","flags":["source-id","to","body","json"],"delegated":true}]}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AgentToken: "synthetic-token", AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(context.Background())
	requirements.NoError(err)
	requirements.Len(capabilities.Commands, 1)
	assertions.Equal("synthetic-token", <-seen)
	assertions.True(capabilities.Delegated)
	assertions.Equal([]string{"source-id", "to", "body", "json"}, capabilities.Commands[0].Flags)
}

func TestMCPCapabilitiesRejectsMalformedDescriptors(t *testing.T) {
	for name, body := range map[string]string{
		"owner routes":             `{"version":1,"delegated":true,"routes":[{"operation_id":"triggerSync","method":"POST","path":"/api/v1/sync/{account}"}],"commands":[]}`,
		"owner command":            `{"version":1,"delegated":true,"routes":[],"commands":[{"name":"draft-send-as","delegated":false}]}`,
		"missing command identity": `{"version":1,"delegated":true,"routes":[],"commands":[{"delegated":true}]}`,
		"duplicate command":        `{"version":1,"delegated":true,"routes":[],"commands":[{"name":"draft-compose","delegated":true},{"name":"draft-compose","delegated":true}]}`,
		"empty flag":               `{"version":1,"delegated":true,"routes":[],"commands":[{"name":"draft-compose","flags":[""],"delegated":true}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AgentToken: "synthetic-token", AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(context.Background())
			requirements.Error(err)
			assertions.Nil(capabilities)
		})
	}
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
