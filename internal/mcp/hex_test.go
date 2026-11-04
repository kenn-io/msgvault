package mcp

import (
	"log/slog"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHexMeetingContextThroughMCP(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := hex.NewImporter(st)
	source := meetingimport.Source{Identifier: "hex-fixture", AccountEmail: "owner@example.com"}
	_, err := imp.ImportRemote(t.Context(), hex.RemoteRequest{Action: "register", Source: source})
	require.NoError(t, err)
	result, err := imp.ImportRemote(t.Context(), hex.RemoteRequest{Action: "upsert", Source: source, Meeting: &hex.Meeting{Manifest: hex.Manifest{SchemaVersion: 1, ID: "mcp-fixture", Title: "Hex MCP fixture", Status: "interrupted", CreatedAtMS: 1790000000123, Calendar: &hex.Calendar{Title: "Planning", Attendees: []string{"Synthetic Guest", "guest@example.com"}}}, TranscriptState: "rolling", Entries: []hex.Entry{{Source: "system", StartMS: 1500, EndMS: 3000, Text: "MCP fixture transcript"}}}})
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServer(&config.Config{}, st, nil, slog.New(slog.DiscardHandler)).Router())
	defer server.Close()
	backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	defer func() { _ = backend.Close() }()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, Meetings: backend}, false).Connect(t.Context(), serverTransport, nil)
	require.NoError(t, err)
	defer func() { _ = serverSession.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "hex-fixture", Version: "1.0.0"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	defer func() { _ = session.Close() }()
	packet, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMeetingContext, Arguments: map[string]any{"message_ids": []any{float64(result.MessageID)}, "format": "markdown", "include_transcript": true}})
	require.NoError(t, err)
	require.False(t, packet.IsError)
	structured := meetingStructuredContent(t, packet)
	assert.Contains(t, structured["content"], "MCP fixture transcript")
	assert.Contains(t, structured["content"], "Synthetic Guest")
	assert.Contains(t, structured["content"], "Them")
}
