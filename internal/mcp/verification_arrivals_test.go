package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestVerificationArrivalsAndSearchThroughMCP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("beeper", "synthetic-source")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "direct-chat", "direct_chat", "Synthetic chat")
	requirements.NoError(err)
	for i, body := range []string{"Your verification code is 482913", "Invoice number 482913", "Invoice number 482913", "Your verification code is 482913"} {
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: fmt.Sprintf("provider-%d", i), MessageType: "beeper", Subject: sql.NullString{String: "482913", Valid: true}, SentAt: sql.NullTime{Time: time.Now().Add(-time.Duration(i+1) * time.Minute), Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		requirements.NoError(st.UpsertFTS(id, "482913", body, "", "", ""))
	}
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "synthetic-owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	call := func(opts ServeOptions, tool string, args map[string]any) string {
		t.Helper()
		response := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": tool, "arguments": args})
		assertions.NotEqual(true, response.Result["isError"], "%v", response.Result)
		encoded, err := json.Marshal(response.Result)
		requirements.NoError(err)
		return string(encoded)
	}
	for _, reveal := range []bool{false, true} {
		permissions := []string{"search.read"}
		if reveal {
			permissions = append(permissions, "verification.read")
		}
		grant, err := owner.IssueAgentToken(t.Context(), "synthetic reader", permissions, []int64{src.ID}, nil, time.Time{})
		requirements.NoError(err)
		client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
		requirements.NoError(err)
		t.Cleanup(func() { requirements.NoError(client.Close()) })
		opts := agentServeOptions(t, client)
		opts.Verifications, opts.UnmaskVerification = client, true // delegated reads must ignore the owner flag
		arrivals := call(opts, ToolListVerifications, map[string]any{})
		assertions.Equal(reveal, strings.Contains(arrivals, "482913"))
		assertions.Contains(arrivals, "provider-0")
		for _, tool := range []string{ToolSearchMetadata, ToolSearchMessageBodies} {
			if st.IsPostgreSQL() && tool == ToolSearchMessageBodies {
				continue
			}
			first := call(opts, tool, map[string]any{"query": "482913", "limit": 2})
			if reveal {
				assertions.Contains(first, "provider-0")
				continue
			}
			assertions.NotContains(first, "provider-0")
			assertions.Contains(first, "provider-1")
			assertions.Contains(first, "provider-2")
			assertions.Contains(first, `\"masked_hits_dropped\":1`)
			assertions.Contains(first, `\"next_offset\":3`)
			assertions.Contains(first, `\"has_more\":true`)
			last := call(opts, tool, map[string]any{"query": "482913", "limit": 2, "offset": 3})
			assertions.NotContains(last, "provider-3")
			assertions.Contains(last, `\"masked_hits_dropped\":1`)
			assertions.Contains(last, `\"next_offset\":4`)
			assertions.Contains(last, `\"has_more\":false`)
		}
		ownerOpts := ServeOptions{Engine: daemonclient.NewEngineAdapter(owner), Verifications: owner, UnmaskVerification: reveal}
		arrivals = call(ownerOpts, ToolListVerifications, map[string]any{})
		assertions.Equal(reveal, strings.Contains(arrivals, "482913"))
	}
}
