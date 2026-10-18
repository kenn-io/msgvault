package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgmcp "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func tierMCP(t *testing.T, backend msgmcp.PersonTierBackend) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- msgmcp.ServeTransport(ctx, msgmcp.ServeOptions{
			Engine: &querytest.MockEngine{}, PersonTierBackend: backend,
		}, serverTransport)
	}()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-tier-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			assert.Fail(t, "MCP transport did not terminate after cancellation")
		}
	})
	return session
}

// A synthetic daemon exercises MCP -> daemonclient -> generated HTTP without
// replacing the MCP handler or duplicating its returned explanation logic.
func tierMCPBackend(t *testing.T, handler http.HandlerFunc) *daemonclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	return backend
}

func TestPersonTierMCPToolIsOptionalAndReadOnly(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "supported"}[enabled], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var backend msgmcp.PersonTierBackend
			if enabled {
				backend = tierMCPBackend(t, func(http.ResponseWriter, *http.Request) {})
			}
			session := tierMCP(t, backend)
			result, err := session.ListTools(t.Context(), nil)
			require.NoError(err)
			var tier *sdkmcp.Tool
			for _, tool := range result.Tools {
				assert.NotEqual("apply_person_tiers", tool.Name)
				if tool.Name == "get_person_tier" {
					tier = tool
				}
			}
			if !enabled {
				assert.Nil(tier)
				return
			}
			require.NotNil(tier)
			require.NotNil(tier.Annotations)
			assert.True(tier.Annotations.ReadOnlyHint)
			assert.Contains(tier.Description, "archived interaction patterns")
			assert.Contains(tier.Description, "judgement")
			assert.NotNil(tier.OutputSchema)
		})
	}
}

const tierMCPExplanation = `{"person_id":7,"name":"Synthetic Example","signals":{"outbound_short":8,"excluded_list_events":4},"prior":{"tier":3},"manual":{},"readiness":{"ready":true,"reasons":[]},"decision":{"tier":2,"computed_tier":2,"rule":"tier_2","thresholds":[{"name":"outbound_short","observed":8,"required":6,"enabled":true,"met":true}]},"tracking":{"person_id":7,"tracked":true,"provenance":"manual","actor":"synthetic-actor","tracked_at":"2026-10-17T12:00:00Z"},"briefs":null,"opt_outs":[],"linked_participants":1,"signal_note":"archived patterns","known_suppression":false,"suppression_note":"","as_of":"2026-10-17T12:00:00Z","enrichment_eligible":true,"enrichment_consent":true,"recent_events":[]}`

func TestPersonTierMCPReturnsExplanationThroughGeneratedHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	backend := tierMCPBackend(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/people/7/tier", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tierMCPExplanation))
	})
	result, err := tierMCP(t, backend).CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_tier", Arguments: map[string]any{"person_id": 7}})
	require.NoError(err)
	require.False(result.IsError)
	require.NotEmpty(result.Content)
	content, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	var explanation map[string]any
	require.NoError(json.Unmarshal([]byte(content.Text), &explanation))
	assert.InDelta(float64(7), explanation["person_id"], 0.001)
	assert.Equal(true, explanation["enrichment_eligible"])
	tracking, ok := explanation["tracking"].(map[string]any)
	require.True(ok)
	assert.Equal("manual", tracking["provenance"])
	assert.Equal("synthetic-actor", tracking["actor"])
	decision, ok := explanation["decision"].(map[string]any)
	require.True(ok)
	assert.NotEmpty(decision["thresholds"])
	assert.Equal(int32(1), requests.Load())
	structured, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	assert.JSONEq(content.Text, string(structured))
}

func TestPersonTierMCPRejectsInvalidIDWithoutHTTP(t *testing.T) {
	var requests atomic.Int32
	backend := tierMCPBackend(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	result, err := tierMCP(t, backend).CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_tier", Arguments: map[string]any{"person_id": 0}})
	if err == nil {
		require.NotNil(t, result)
		assert.True(t, result.IsError)
	}
	assert.Zero(t, requests.Load())
}
