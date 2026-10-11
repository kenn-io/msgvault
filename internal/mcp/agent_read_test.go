package mcp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentReadsThroughMCPSDK(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	sources := make([]*store.Source, 2)
	ids := make([]int64, 2)
	for i, identifier := range []string{"reader@example.test", "outside@example.test"} {
		var err error
		sources[i], ids[i], err = testutil.CreateIndexedSourceMessage(st, identifier, identifier, "glacier", "glacier body")
		requirements.NoError(err)
	}
	src, other := sources[0], sources[1]
	conv, err := st.EnsureConversation(src.ID, "not-indexed", "Synthetic")
	requirements.NoError(err)
	unindexedID, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "not-indexed", MessageType: "email", Subject: sql.NullString{String: "metadata glacier", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(unindexedID, sql.NullString{String: "glacier unindexed body", Valid: true}, sql.NullString{}))
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	var requests atomic.Uint64
	router := srv.Router()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requests.Add(1))
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"search.read", "stats.read", "message.read"}, []int64{src.ID}, nil, time.Time{})
	requirements.NoError(err)
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = agent.Close() })
	session := task5ConnectClient(t, agentServeOptions(t, agent), true)
	tools, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	templates, err := session.ListResourceTemplates(t.Context(), nil)
	requirements.NoError(err)
	assertions.Empty(templates.ResourceTemplates)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assertions.NotContains(names, ToolStageDeletion)
	assertions.NotContains(names, ToolExportEML)
	assertions.NotContains(names, ToolExportAttachment)
	assertions.NotContains(names, ToolQuerySQL)
	assertions.Contains(names, ToolGetMessage)
	assertions.Contains(names, ToolGetStats)
	assertions.NotContains(names, ToolGetAttachment, "the grant lacks attachment.read")
	for _, tc := range []struct {
		name   string
		args   map[string]any
		denied bool
	}{
		{ToolGetMessage, map[string]any{"id": ids[0]}, false},
		{ToolGetMessage, map[string]any{"id": ids[1]}, true},
		{ToolGetStats, map[string]any{}, false},
		{ToolListMessages, map[string]any{}, false},
		{ToolSearchByDomains, map[string]any{"domains": "example.test"}, false},
		{ToolSearchMessageBodies, map[string]any{"query": "glacier"}, st.IsPostgreSQL()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.Equal(tc.denied, result.IsError, task5SDKResultText(result))
			if !tc.denied {
				assertions.NotContains(task5SDKResultText(result), other.Identifier)
			}
			if tc.name == ToolSearchMessageBodies {
				if st.IsPostgreSQL() {
					assertions.Contains(task5SDKResultText(result), "rebuild-fts")
				} else {
					assertions.Contains(task5SDKResultText(result), `"index_state":"checking"`)
				}
				requirements.Eventually(func() bool {
					result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMetadata, Arguments: map[string]any{"query": "glacier"}})
					return err == nil && strings.Contains(task5SDKResultText(result), "awaiting_owner")
				}, 10*time.Second, time.Millisecond)
				assertions.False(result.IsError, task5SDKResultText(result))
				assertions.NotContains(task5SDKResultText(result), other.Identifier)
				_, err = owner.RebuildCLIFTS(t.Context(), nil)
				requirements.NoError(err)
			}
		})
	}
	denied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolListThread, Arguments: map[string]any{"thread_id": "thread", "account": other.Identifier}})
	requirements.NoError(err)
	assertions.True(denied.IsError)
	assertions.Contains(task5SDKResultText(denied), "permission_denied")
	assertions.Contains(task5SDKResultText(denied), "message.read", "the agent must learn which permission to request")
	multi, err := owner.IssueAgentToken(t.Context(), "multiple accounts", []string{"search.read", "message.read"}, []int64{src.ID, other.ID}, nil, time.Time{})
	requirements.NoError(err)
	multiAgent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: multi.Secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = multiAgent.Close() })
	multiSession := task5ConnectClient(t, agentServeOptions(t, multiAgent), true)
	result, err := multiSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"query": "glacier"}})
	requirements.NoError(err)
	assertions.True(result.IsError, task5SDKResultText(result))
	for _, source := range []*store.Source{src, other} {
		result, err = multiSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolSearchMessageBodies, Arguments: map[string]any{"query": "glacier", "account": source.Identifier}})
		requirements.NoError(err)
		assertions.False(result.IsError, task5SDKResultText(result))
		assertions.Contains(task5SDKResultText(result), "glacier")
		assertions.Contains(task5SDKResultText(result), source.Identifier)
	}
}

// agentServeOptions mirrors the delegated stdio setup: it reads the grant from the daemon.
func agentServeOptions(t *testing.T, agent *daemonclient.Client) ServeOptions {
	t.Helper()
	grant, err := agent.AgentTokenSelf(t.Context())
	require.NoError(t, err)
	return ServeOptions{Engine: daemonclient.NewEngineAdapter(agent), DelegatedOnly: true, GrantPermissions: grant.Permissions}
}

func TestAgentToolsFollowGrantPermissions(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	readTools := []string{ToolSearchMessages, ToolSearchMetadata, ToolSearchMessageBodies, ToolListMessages, ToolAggregate, ToolSearchByDomains, ToolGetMessage, ToolListThread, ToolSearchInMessage, ToolGetAttachment, ToolGetStats}
	for _, tc := range []struct {
		permission string
		want       []string
	}{
		{"draft.create", nil},
		{"search.read", []string{ToolSearchMessages, ToolSearchMetadata, ToolSearchMessageBodies, ToolListMessages, ToolAggregate, ToolSearchByDomains}},
		{"message.read", []string{ToolGetMessage, ToolListThread, ToolSearchInMessage}},
		{"attachment.read", []string{ToolGetAttachment}},
		{"stats.read", []string{ToolGetStats}},
	} {
		t.Run(tc.permission, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			grant, err := owner.IssueAgentToken(t.Context(), tc.permission, []string{tc.permission}, []int64{source.ID}, nil, time.Time{})
			requirements.NoError(err)
			agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = agent.Close() })
			opts := agentServeOptions(t, agent)
			opts.AttachmentReader = agent
			tools, err := task5ConnectClient(t, opts, true).ListTools(t.Context(), nil)
			requirements.NoError(err)
			var listed []string
			for _, tool := range tools.Tools {
				if slices.Contains(readTools, tool.Name) {
					listed = append(listed, tool.Name)
				}
			}
			assertions.ElementsMatch(tc.want, listed)
		})
	}
}

func TestAgentAttachmentChunksReauthorize(t *testing.T) {
	for _, action := range []string{"revoke", "replacement", "disappearance"} {
		t.Run(action, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
			requirements.NoError(err)
			data := []byte("synthetic attachment chunks")
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			path, err := msgexport.StoragePath(cfg.AttachmentsDir(), digest)
			requirements.NoError(err)
			requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
			requirements.NoError(os.WriteFile(path, data, 0600))
			requirements.NoError(st.UpsertAttachment(id, "synthetic.bin", "application/octet-stream", path, digest, len(data)))
			engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
			attachments, err := engine.GetAttachmentsByHash(t.Context(), digest)
			requirements.NoError(err)
			requirements.Len(attachments, 1)
			attachmentID := attachments[0].ID
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
			server := httptest.NewServer(srv.Router())
			t.Cleanup(server.Close)
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = owner.Close() })
			grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"attachment.read"}, []int64{source.ID}, nil, time.Time{})
			requirements.NoError(err)
			agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = agent.Close() })
			opts := agentServeOptions(t, agent)
			opts.AttachmentReader = agent
			session := task5ConnectClient(t, opts, true)
			call := func(offset int) *sdkmcp.CallToolResult {
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetAttachment, Arguments: map[string]any{"attachment_id": attachmentID, "offset": offset, "length": 4, "sha256": digest}})
				requirements.NoError(err)
				return result
			}
			assertions.False(call(0).IsError)
			assertions.False(call(4).IsError, "cached continuation before authority changes")
			_, err = st.DB().Exec(st.Rebind("UPDATE attachments SET filename=?, mime_type=?, size=? WHERE id=?"), "updated.bin", "text/plain", len(data)+7, attachmentID)
			requirements.NoError(err)
			updated := call(8)
			assertions.False(updated.IsError, task5SDKResultText(updated))
			assertions.Contains(task5SDKResultText(updated), "synthetic.bin")
			assertions.Contains(task5SDKResultText(updated), "application/octet-stream")
			assertions.NotContains(task5SDKResultText(updated), "updated.bin")
			switch action {
			case "revoke":
				requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
			case "replacement":
				data = []byte("replacement attachment")
				newHash := fmt.Sprintf("%x", sha256.Sum256(data))
				newPath, err := msgexport.StoragePath(cfg.AttachmentsDir(), newHash)
				requirements.NoError(err)
				requirements.NoError(os.MkdirAll(filepath.Dir(newPath), 0700))
				requirements.NoError(os.WriteFile(newPath, data, 0600))
				_, err = st.DB().Exec(st.Rebind("UPDATE attachments SET content_hash=?, storage_path=?, size=? WHERE id=?"), newHash, newPath, len(data), attachmentID)
				requirements.NoError(err)
			case "disappearance":
				_, err = st.DB().Exec(st.Rebind("DELETE FROM attachments WHERE id=?"), attachmentID)
				requirements.NoError(err)
			}
			result := call(12)
			assertions.True(result.IsError, task5SDKResultText(result))
			assertions.NotContains(task5SDKResultText(result), "data_base64")
			if action == "replacement" {
				restarted, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetAttachment, Arguments: map[string]any{"attachment_id": attachmentID, "offset": 0, "length": 4}})
				requirements.NoError(err)
				assertions.False(restarted.IsError, task5SDKResultText(restarted))
				assertions.Contains(task5SDKResultText(restarted), base64.StdEncoding.EncodeToString(data[:4]))
			}
		})
	}
}
