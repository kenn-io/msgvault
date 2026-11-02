package mcp

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMessageRecordsLocalAutoKindProvenance(t *testing.T) {
	for _, handler := range []string{"store", "query"} {
		t.Run(handler, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, _, err := testutil.CreateIndexedSourceMessage(st, "records@example.test", "seed", "Fixture", "Fixture")
			require.NoError(t, err)
			var engine query.Engine
			if handler == "query" {
				engine = query.NewEngine(st.DB(), st.IsPostgreSQL())
			}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
			server := httptest.NewServer(srv.Router())
			t.Cleanup(server.Close)
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = owner.Close() })
			grant, err := owner.IssueAgentToken(t.Context(), "records", []string{"message.read"}, []int64{source.ID}, nil, time.Time{})
			require.NoError(t, err)
			agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = agent.Close() })
			session := task5ConnectClient(t, agentServeOptions(t, agent), true)
			embedded := NewEmbeddedReader(st, "")
			t.Cleanup(embedded.Close)
			cases := []struct {
				name, labelType, body, html, want, source, trust string
				provider, conflict                               bool
			}{
				{name: "local auto", labelType: "auto", body: "Paid for purchase", want: "receipt", source: "decision_model", trust: "model_judgment"},
				{name: "user lookalike", labelType: "user", body: "Paid for purchase", want: "none"},
				{name: "provider lookalike", labelType: "auto", provider: true, body: "Paid for purchase", want: "none"},
				{name: "conflicting auto", labelType: "auto", conflict: true, body: "Paid for purchase", want: "none"},
				{name: "deterministic wins", labelType: "auto", body: "Order B-42", html: `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`, want: "order", source: "json_ld", trust: "claimed_by_sender"},
				{name: "credentials excluded", labelType: "auto", body: "Your sign-in verification code is 654321", want: "verification", source: "rule", trust: "claimed_by_sender"},
				{name: "oversized body", labelType: "auto", body: strings.Repeat("x", (1<<20)+1), want: "none"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					_, id, err := testutil.CreateIndexedSourceMessage(st, "records@example.test", tc.name, "Purchase notice", tc.body)
					require.NoError(t, err)
					require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: tc.body, Valid: true}, sql.NullString{String: tc.html, Valid: tc.html != ""}))
					var labelID int64
					var labelSource any
					if tc.provider {
						labelSource = source.ID
					}
					require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO labels (source_id,name,label_type) VALUES (?, 'mv:kind/receipt', ?) RETURNING id`), labelSource, tc.labelType).Scan(&labelID))
					labels := []int64{labelID}
					if tc.conflict {
						require.NoError(t, st.DB().QueryRowContext(t.Context(), `INSERT INTO labels (source_id,name,label_type) VALUES (NULL, 'mv:kind/invoice', 'auto') RETURNING id`).Scan(&labelID))
						labels = append(labels, labelID)
					}
					require.NoError(t, st.ReplaceMessageLabels(id, labels))
					remote, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: ToolGetMessageRecords, Arguments: map[string]any{"message_id": id}})
					require.NoError(t, err)
					local, structured, err := embedded.Call(t.Context(), []int64{source.ID}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, map[string]any{"message_id": float64(id)})
					require.NoError(t, err)
					local.StructuredContent = structured
					for _, result := range []*sdkmcp.CallToolResult{remote, local} {
						require.False(t, result.IsError, task5SDKResultText(result))
						value := task5StructuredAs[messageRecordsResponse](t, result)
						assert.Equal(t, tc.want, value.Kind)
						assert.Equal(t, tc.source, value.KindSource)
						assert.Equal(t, tc.trust, value.KindTrust)
						if tc.source == "decision_model" {
							assert.Empty(t, value.Records)
							assert.Contains(t, strings.Join(value.Warnings, " "), "advisory model judgment")
						}
						assert.NotContains(t, task5SDKResultText(result), "654321")
					}
					denied, _, err := embedded.Call(t.Context(), []int64{source.ID + 100000}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetMessageRecords}}, map[string]any{"message_id": float64(id)})
					require.NoError(t, err)
					assert.True(t, denied.IsError)
				})
			}
		})
	}
}
