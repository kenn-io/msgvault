package mcp

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

type messageRecordsSenderAuthFixture struct {
	Kind       string              `json:"kind"`
	SenderAuth *senderauth.Verdict `json:"sender_auth"`
	Warnings   []string            `json:"warnings"`
}

func TestMessageRecordsSenderAuthentication(t *testing.T) {
	for _, handler := range []string{"store", "query"} {
		t.Run(handler, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st := testutil.NewTestStore(t)
			source, id, err := testutil.CreateIndexedSourceMessage(st, "records@example.test", "receipt", "Order", "Order B-42")
			require.NoError(err)
			_, outside, err := testutil.CreateIndexedSourceMessage(st, "outside@example.test", "outside", "Order", "Order B-42")
			require.NoError(err)
			participant, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (?,?,'from')`), id, participant)
			require.NoError(err)
			body := `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`
			require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Order B-42", Valid: true}, sql.NullString{String: body, Valid: true}))
			var engine query.Engine
			if handler == "query" {
				engine = query.NewEngine(st.DB(), st.IsPostgreSQL())
			}
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()},
				Server:     config.ServerConfig{APIKey: "owner", AgentAccess: true},
				SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
			server := httptest.NewServer(srv.Router())
			t.Cleanup(server.Close)
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(owner.Close()) })
			grant, err := owner.IssueAgentToken(t.Context(), "records", []string{"message.read"}, []int64{source.ID}, nil, time.Time{})
			require.NoError(err)
			agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(agent.Close()) })
			session := task5ConnectClient(t, agentServeOptions(t, agent), true)
			embedded := NewEmbeddedReader(st, "")
			t.Cleanup(embedded.Close)
			for _, tc := range []struct {
				name, header string
				verified     bool
				basis        senderauth.Basis
			}{
				{"verified", "mx.test; dmarc=pass header.from=example.test", true, senderauth.ProviderHeader},
				{"failed", "mx.test; dmarc=fail header.from=example.test", false, senderauth.ProviderHeader},
				{"untrusted", "untrusted.test; dmarc=pass header.from=example.test", false, senderauth.Untrusted},
				{"mismatched", "mx.test; dmarc=pass header.from=other.test", false, senderauth.FromMismatch},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assert := Assert.New(t)
					require := Require.New(t)
					raw := "From: sender@example.test\r\nAuthentication-Results: " + tc.header + "\r\n\r\nOrder B-42"
					require.NoError(st.UpsertMessageRaw(id, []byte(raw)))
					result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": id}})
					require.NoError(err)
					require.False(result.IsError, task5SDKResultText(result))
					value := task5StructuredAs[messageRecordsSenderAuthFixture](t, result)
					require.NotNil(value.SenderAuth)
					assert.Equal(tc.verified, value.SenderAuth.Verified)
					assert.Equal(tc.basis, value.SenderAuth.Basis)
					assert.Equal("order", value.Kind)
					assert.Contains(strings.Join(value.Warnings, " "), "verify before acting")
					local, structured, err := embedded.Call(t.Context(), []int64{source.ID}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, map[string]any{"message_id": float64(id)})
					require.NoError(err)
					local.StructuredContent = structured
					require.False(local.IsError, task5SDKResultText(local))
					localValue := task5StructuredAs[messageRecordsSenderAuthFixture](t, local)
					require.NotNil(localValue.SenderAuth)
					assert.False(localValue.SenderAuth.Verified, "the embedded reader has no configured trust pins")
				})
			}
			require.NoError(st.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nOrder B-42")))
			require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Your sign-in verification code is 654321", Valid: true}, sql.NullString{String: body, Valid: true}))
			credential, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": id}})
			require.NoError(err)
			require.False(credential.IsError, task5SDKResultText(credential))
			credentialValue := task5StructuredAs[messageRecordsSenderAuthFixture](t, credential)
			require.NotNil(credentialValue.SenderAuth)
			assert.True(credentialValue.SenderAuth.Verified)
			assert.Equal("verification", credentialValue.Kind)
			assert.NotContains(task5SDKResultText(credential), "654321")
			denied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": outside}})
			require.NoError(err)
			missing, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": outside + 100000}})
			require.NoError(err)
			assert.True(denied.IsError)
			assert.Equal(task5SDKResultText(missing), task5SDKResultText(denied))
		})
	}
}

// This real wire fixture represents a daemon version that omits the optional
// sender-auth field. The records tool must keep that absence as explicit null.
func TestMessageRecordsSenderAuthenticationOlderDaemon(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.MarshalWrite(w, map[string]any{
			"id": 42, "body": "Order B-42", "from": "sender@example.test", "to": []string{},
			"subject": "Order", "sent_at": "2026-01-01T00:00:00Z", "snippet": "", "labels": []string{},
			"has_attachments": false, "hidden_text_present": false, "size_bytes": 0, "attachments": []any{},
		}))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	session := task5ConnectClient(t, ServeOptions{Engine: daemonclient.NewEngineAdapter(client)}, true)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": 42}})
	require.NoError(err)
	require.False(result.IsError, task5SDKResultText(result))
	assert.Contains(task5SDKResultText(result), `"sender_auth":null`)
}
