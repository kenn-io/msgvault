package cmd

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPCollectionScopesUseNativeCatalogAndExactMembership(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	var sources []int64
	var messages []int64
	for i, input := range []struct{ kind, identifier, messageType string }{{"gmail", "shared@example.test", "email"}, {"telegram", "synthetic-chat", "chat"}, {"imap", "shared@example.test", "email"}} {
		source, err := st.GetOrCreateSource(input.kind, input.identifier)
		requirements.NoError(err)
		conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
		requirements.NoError(err)
		message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "synthetic-message", MessageType: input.messageType, Subject: sql.NullString{String: "synthetic scope evidence", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, time.September, 1+i, 0, 0, 0, 0, time.UTC), Valid: true}, SizeEstimate: 100})
		requirements.NoError(err)
		sources = append(sources, source.ID)
		messages = append(messages, message)
	}
	_, err := st.CreateCollection("shared@example.test", "Synthetic mixed scope", sources[:2])
	requirements.NoError(err)
	_, err = st.CreateCollection("empty", "Synthetic empty scope", sources[:1])
	requirements.NoError(err)
	requirements.NoError(st.RemoveSourcesFromCollection("empty", sources[:1]))
	native := query.NewSQLiteEngine(st.DB())
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Engine: native, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, daemon)
	session := operationMCPSession(t, backend, nil, nil)
	catalog := callRecordTool[struct {
		Collections []struct {
			Name      string  `json:"name"`
			SourceIDs []int64 `json:"source_ids"`
		} `json:"collections"`
	}](t, session, "list_message_collections", map[string]any{})
	requirements.Len(catalog.Collections, 2)
	for _, collection := range catalog.Collections {
		assertions.NotEqual(store.DefaultCollectionName, collection.Name)
		if collection.Name == "empty" {
			assertions.NotNil(collection.SourceIDs)
			assertions.Empty(collection.SourceIDs)
		} else {
			assertions.ElementsMatch(sources[:2], collection.SourceIDs)
		}
	}
	for _, tool := range []string{"list_messages", "search_metadata"} {
		args := map[string]any{"collection": "shared@example.test"}
		if tool == "search_metadata" {
			args["query"] = "synthetic"
		}
		page := callRecordTool[struct {
			Data []query.MessageSummary `json:"data"`
		}](t, session, tool, args)
		ids := make([]int64, 0, len(page.Data))
		for _, row := range page.Data {
			ids = append(ids, row.ID)
		}
		assertions.ElementsMatch(messages[:2], ids, "collection name must not be resolved as the colliding account identifier")
		args["collection"] = "empty"
		empty := callRecordTool[struct {
			Data []query.MessageSummary `json:"data"`
		}](t, session, tool, args)
		assertions.Empty(empty.Data)
	}
	stats := callRecordTool[struct {
		Stats    query.TotalStats    `json:"stats"`
		Accounts []query.AccountInfo `json:"accounts"`
	}](t, session, "get_stats", map[string]any{"collection": "shared@example.test"})
	expected, err := native.GetTotalStats(t.Context(), query.StatsOptions{SourceIDs: sources[:2]})
	requirements.NoError(err)
	assertions.Equal(expected.MessageCount, stats.Stats.MessageCount)
	accountIDs := make([]int64, 0, len(stats.Accounts))
	for _, account := range stats.Accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	assertions.ElementsMatch(sources[:2], accountIDs)
	aggregated := callRecordTool[struct {
		Data []query.AggregateRow `json:"data"`
	}](t, session, "aggregate", map[string]any{"collection": "shared@example.test", "group_by": "time"})
	var count int64
	for _, row := range aggregated.Data {
		count += row.Count
	}
	assertions.Equal(expected.MessageCount, count, "native aggregate email defaults stay intact within the exact collection")
	for _, args := range []map[string]any{{"collection": "unknown"}, {"collection": "All"}, {"collection": ""}, {"collection": "shared@example.test", "account": "shared@example.test"}} {
		called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_messages", Arguments: args})
		assertions.True(err != nil || called != nil && called.IsError)
	}
	for _, tool := range []string{"search_message_bodies", "stage_deletion"} {
		called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: map[string]any{"collection": "shared@example.test", "query": "synthetic"}})
		assertions.True(err != nil || called != nil && called.IsError, "unsupported multi-source scope must refuse")
	}
	requirements.NoError(st.RemoveSourcesFromCollection("shared@example.test", sources[1:2]))
	changed := callRecordTool[struct {
		Data []query.MessageSummary `json:"data"`
	}](t, session, "list_messages", map[string]any{"collection": "shared@example.test"})
	requirements.Len(changed.Data, 1)
	assertions.Equal(messages[0], changed.Data[0].ID, "resolve current membership once per invocation")
	// Execute the real owning read, then remove only its source-scope receipt.
	// This models the older external wire contract without replacing queries
	// or manufacturing archive rows. No unconfirmed rows may reach the caller.
	missingEcho := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		daemon.Router().ServeHTTP(recorded, r)
		data := recorded.Body.Bytes()
		if recorded.Code == http.StatusOK && slices.Contains([]string{"/api/v1/messages/filter", "/api/v1/search/fast", "/api/v1/aggregates", "/api/v1/stats/total"}, r.URL.Path) {
			var response map[string]jsontext.Value
			if err := json.Unmarshal(data, &response); !assert.NoError(t, err) {
				http.Error(w, "synthetic scope receipt decode failed", http.StatusInternalServerError)
				return
			}
			delete(response, "applied_source_ids")
			var err error
			data, err = json.Marshal(response)
			if !assert.NoError(t, err) {
				http.Error(w, "synthetic scope receipt encode failed", http.StatusInternalServerError)
				return
			}
		}
		for key, values := range recorded.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(recorded.Code)
		_, err := w.Write(data)
		assert.NoError(t, err)
	}))
	t.Cleanup(missingEcho.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: missingEcho.URL, AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	oldSession := operationMCPSession(t, newDaemonMCPOperations(client, capabilities), nil, nil)
	for _, tool := range []string{"list_messages", "search_metadata", "aggregate", "get_stats"} {
		args := map[string]any{"collection": "shared@example.test"}
		if tool == "search_metadata" {
			args["query"] = "synthetic"
		}
		if tool == "aggregate" {
			args["group_by"] = "time"
		}
		called, err := oldSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
		requirements.NoError(err)
		assertions.True(called.IsError, "%s must not return rows without the daemon's applied scope", tool)
		assertions.Contains(settingsMCPDiagnostic(called), "source_scope_unconfirmed")
	}
}
