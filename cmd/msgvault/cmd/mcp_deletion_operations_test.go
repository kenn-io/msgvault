package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPDeletionExplicitIDsUseNativeEligibilityDuringCacheInitialization(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	var ids []int64
	for _, item := range []struct{ kind, typ, provider string }{{"gmail", "email", "synthetic-mail"}, {"telegram", "chat", "synthetic-chat"}, {"imap", "email", "synthetic-imap"}, {"gmail", "email", ""}} {
		source, err := st.GetOrCreateSource(item.kind, item.kind+"@example.test")
		requirements.NoError(err)
		conv, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic thread")
		requirements.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: item.provider, MessageType: item.typ, Subject: sql.NullString{String: "synthetic deletion evidence", Valid: true}})
		requirements.NoError(err)
		ids = append(ids, id)
	}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Engine: query.NewSQLiteEngine(st.DB()), AnalyticsMode: api.AnalyticsModeInitializing, Logger: slog.New(slog.DiscardHandler)}))
	assertions.Contains(backend.capabilities(), "preview_deletion_selection")
	assertions.Contains(backend.capabilities(), "stage_deletion")
	approvals := 0
	session := operationMCPSession(t, backend, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, r *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assertions.Contains(r.Params.Message, "remote deletion")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: map[string]any{"message_ids": append(ids, int64(99999)), "dry_run": true}})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	data, ok := result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, data["dry_run"])
	assertions.InDelta(1, data["message_count"], 0)
	assertions.NotContains(data, "matched_count")
	assertions.NotContains(data, "skipped_count")
	assertions.NotContains(data, "id")
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	requirements.NoError(err)
	manifests, err := manager.ListPending()
	requirements.NoError(err)
	assertions.Empty(manifests)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: map[string]any{"message_ids": ids}})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	data, ok = result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.InDelta(1, data["message_count"], 0)
	assertions.NotEmpty(data["id"])
	manifests, err = manager.ListPending()
	requirements.NoError(err)
	requirements.Len(manifests, 1)
	assertions.Equal([]string{"synthetic-mail"}, manifests[0].GmailIDs)
	before := approvals
	for _, args := range []map[string]any{{"message_ids": ids, "selection": map[string]any{}}, {"message_ids": []int64{}}, {"message_ids": []int64{ids[0], ids[0]}}, {"message_ids": ids, "query": "synthetic"}} {
		result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: args})
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	assertions.Equal(before, approvals)
}

func TestMCPDeletionReviewedSelectionUsesNativeExploreAndOneShotAuthority(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	var sourceIDs []int64
	for _, item := range []struct{ kind, account, provider, typ, subject string }{{"gmail", "one@example.test", "synthetic-mail", "email", "synthetic mixed"}, {"telegram", "synthetic-chat", "synthetic-chat", "chat", "synthetic mixed"}, {"gmail", "two@example.test", "synthetic-other", "email", "synthetic other"}} {
		source, err := st.GetOrCreateSource(item.kind, item.account)
		requirements.NoError(err)
		sourceIDs = append(sourceIDs, source.ID)
		conv, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic thread")
		requirements.NoError(err)
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: item.provider, MessageType: item.typ, Subject: sql.NullString{String: item.subject, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Valid: true}}, BodyText: sql.NullString{String: item.subject, Valid: true}})
		requirements.NoError(err)
	}
	_, err = st.CreateCollection("one-source", "Synthetic scope", sourceIDs[:1])
	requirements.NoError(err)
	_, err = st.CreateCollection("mixed-collection", "Synthetic mixed scope", sourceIDs[:2])
	requirements.NoError(err)
	_, err = st.CreateCollection("empty-collection", "Synthetic empty scope", sourceIDs[:1])
	requirements.NoError(err)
	requirements.NoError(st.RemoveSourcesFromCollection("empty-collection", sourceIDs[:1]))
	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(cfg.AnalyticsDir(), cfg.DatabaseDSN(), nil)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(daemon.Shutdown(context.Background())) })
	backend := sourceOperationFixture(t, daemon)
	requirements.Contains(backend.capabilities(), "preview_deletion_selection")
	// Wait only for the real daemon's observable index-ready transition. The
	// background native completeness check is unchanged and has no poll-count SLA.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		probe, err := backend.client.GetCLISearch(t.Context(), daemonclient.CLISearchRequest{Query: "mixed", Limit: 1})
		requirements.NoError(err)
		if probe.IndexState == "" {
			break
		}
		select {
		case <-deadline.C:
			requirements.FailNow("native FTS completeness did not settle")
		case <-time.After(10 * time.Millisecond):
		}
	}
	approvals := 0
	session := operationMCPSession(t, backend, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, r *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assertions.Contains(r.Params.Message, "Current native eligibility: 1")
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_deletion_selection", Arguments: map[string]any{"query": "mixed"}})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var preview mcpserver.DeletionSelectionPreview
	requirements.NoError(json.Unmarshal(data, &preview))
	assertions.Equal(int64(2), preview.Preflight.Count)
	assertions.Equal(int64(1), preview.Preflight.DeletableCount)
	assertions.NotEmpty(preview.Preflight.OperationToken)
	assertions.NotEmpty(preview.Selection.CacheRevision)
	assertions.Equal(generated.ExploreHTTPRequestSearchModeFullText, *preview.Selection.Predicate.SearchMode)
	// Execute the real search and alter only its readiness receipt. This is
	// the stable daemon wire contract; no search, rows, or preflight is stubbed.
	for _, state := range []string{"checking", "building"} {
		t.Run("index-"+state, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			var analyticalCalls atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/explore" || r.URL.Path == "/api/v1/explore/preflight" {
					analyticalCalls.Add(1)
				}
				recorded := httptest.NewRecorder()
				daemon.Router().ServeHTTP(recorded, r)
				body := recorded.Body.Bytes()
				if r.URL.Path == "/api/v1/cli/search" && recorded.Code == http.StatusOK {
					var receipt generated.CliSearchResponse
					if err := json.Unmarshal(body, &receipt); !assert.NoError(t, err) {
						http.Error(w, "synthetic readiness receipt decode failed", http.StatusInternalServerError)
						return
					}
					receipt.IndexState = &state
					var err error
					body, err = json.Marshal(receipt)
					if !assert.NoError(t, err) {
						http.Error(w, "synthetic readiness receipt encode failed", http.StatusInternalServerError)
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
				_, err := w.Write(body)
				assert.NoError(t, err)
			}))
			t.Cleanup(proxy.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: proxy.URL, AllowInsecure: true})
			requirements.NoError(err)
			capabilities, err := client.MCPCapabilities(t.Context())
			requirements.NoError(err)
			guarded := operationMCPSession(t, newDaemonMCPOperations(client, capabilities), nil, nil)
			for _, input := range []map[string]any{{"query": "mixed"}, {"selection": preview.Selection}} {
				refused, err := guarded.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_deletion_selection", Arguments: input})
				requirements.NoError(err)
				assertions.True(refused.IsError)
				refusalData, ok := refused.StructuredContent.(map[string]any)
				requirements.True(ok)
				assertions.Equal("search_index_"+state, refusalData["error"])
			}
			assertions.Equal(int64(0), analyticalCalls.Load(), "index guard precedes Explore and preflight")
		})
	}
	beforeReview := approvals
	for _, kind := range []string{"archive", "search"} {
		changed := preview.Selection
		expected := "archive_revision_changed"
		if kind == "archive" {
			changed.CacheRevision = "synthetic-stale-cache"
		} else {
			revision := "fts5:synthetic-stale-search"
			changed.SearchProvenance.LexicalIndexRevision = &revision
			expected = "search_revision_changed"
		}
		refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: map[string]any{"selection": changed, "operation_token": preview.Preflight.OperationToken}})
		requirements.NoError(err)
		assertions.True(refused.IsError)
		refusalData, ok := refused.StructuredContent.(map[string]any)
		requirements.True(ok)
		assertions.Equal(expected, refusalData["error"])
	}
	assertions.Equal(beforeReview, approvals)
	var args map[string]any
	raw, err := json.Marshal(generated.StageDeletionBody{Selection: &preview.Selection, OperationToken: &preview.Preflight.OperationToken, DryRun: func() *bool { v := true; return &v }()})
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(raw, &args))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	staged, ok := result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.InDelta(2, staged["matched_count"], 0)
	assertions.InDelta(1, staged["message_count"], 0)
	assertions.InDelta(1, staged["skipped_count"], 0)
	assertions.NotContains(staged, "id")
	manager, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	requirements.NoError(err)
	manifests, err := manager.ListPending()
	requirements.NoError(err)
	assertions.Empty(manifests)
	args["dry_run"] = false
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: args})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	manifests, err = manager.ListPending()
	requirements.NoError(err)
	requirements.Len(manifests, 1)
	assertions.Equal([]string{"synthetic-mail"}, manifests[0].GmailIDs)
	before := approvals
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: args})
	requirements.NoError(err)
	assertions.True(result.IsError)
	refusalData, ok := result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.Equal("operation_token_invalid", refusalData["error"])
	assertions.Equal(before, approvals)
	// A newly reviewed broad query contains two deletable mailboxes. The daemon
	// refuses the split before user approval, without an additional manifest.
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "preview_deletion_selection", Arguments: map[string]any{"query": "synthetic"}})
	requirements.NoError(err)
	requirements.False(result.IsError, "%v", result.Content)
	data, err = json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(data, &preview))
	raw, err = json.Marshal(generated.StageDeletionBody{Selection: &preview.Selection, OperationToken: &preview.Preflight.OperationToken})
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(raw, &args))
	delete(args, "dry_run")
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "stage_deletion", Arguments: args})
	requirements.NoError(err)
	assertions.True(result.IsError)
	refusalData, ok = result.StructuredContent.(map[string]any)
	requirements.True(ok)
	assertions.Equal("multi_account_selection", refusalData["error"])
	assertions.Equal(before, approvals)
	manifests, err = manager.ListPending()
	requirements.NoError(err)
	assertions.Len(manifests, 1)
	for _, scope := range []map[string]any{{"source_id": sourceIDs[0]}, {"account": "one@example.test"}, {"collection": "one-source"}} {
		scope["query"] = "synthetic"
		narrowed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpDeletionPreview, Arguments: scope})
		requirements.NoError(err)
		requirements.False(narrowed.IsError, "%v", narrowed.Content)
		data, err = json.Marshal(narrowed.StructuredContent)
		requirements.NoError(err)
		requirements.NoError(json.Unmarshal(data, &preview))
		assertions.Equal(int64(1), preview.Preflight.Count)
		assertions.Equal(int64(1), preview.Preflight.DeletableCount)
		if _, exact := scope["source_id"]; exact {
			narrowed, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpDeletionStage, Arguments: map[string]any{"selection": preview.Selection, "operation_token": preview.Preflight.OperationToken, "dry_run": true}})
			requirements.NoError(err)
			assertions.False(narrowed.IsError, "%v", narrowed.Content)
		}
	}
	for _, scope := range []map[string]any{{"account": "unknown@example.test"}, {"account": "one@example.test", "source_id": sourceIDs[0]}, {"collection": "All"}, {"collection": "missing-collection"}, {"collection": "mixed-collection"}, {"collection": "empty-collection"}, {"collection": "one-source", "account": ""}} {
		scope["query"] = "synthetic"
		refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpDeletionPreview, Arguments: scope})
		requirements.NoError(err)
		assertions.True(refused.IsError)
	}
	manifests, err = manager.ListPending()
	requirements.NoError(err)
	assertions.Len(manifests, 1)
}

// This fixture exercises the real HTTP client and fixed refusal parser. The
// native API suite separately proves bounded candidate saturation; a wire
// refusal does not need a second archive with more than ten thousand messages.
func TestMCPDeletionPreservesNativeCandidateRefusalWithoutPrivateDiagnostic(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/explore/preflight", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		err := json.MarshalWrite(w, api.ErrorResponse{Error: "candidate_pool_saturated", Message: "synthetic private daemon diagnostic"})
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	result, err := readMCPOperationJSON[generated.ExplorePreflightResponse](t.Context(), client, http.MethodPost, "/api/v1/explore/preflight", nil, http.StatusOK)
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	var refusal struct {
		Error     string `json:"error"`
		Uncertain bool   `json:"operation_may_have_completed"`
	}
	requirements.NoError(json.Unmarshal(data, &refusal))
	assertions.Equal("candidate_pool_saturated", refusal.Error)
	assertions.False(refusal.Uncertain)
	assertions.NotContains(string(data), "synthetic private daemon diagnostic")
}
