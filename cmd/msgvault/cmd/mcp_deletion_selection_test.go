package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPDeletionSelectionNativePreflightAndStage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Synthetic")
	require.NoError(t, err)
	for _, id := range []string{"first", "second"} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: id, MessageType: "email", Subject: sql.NullString{String: "Synthetic", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
		require.NoError(t, err)
	}
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", st.DB())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	adapter := &storeAPIAdapter{store: st, config: cfg, analyticsDir: analytics}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler)}))
	assert.Contains(t, backend.capabilities(), "stage_deletion_selection")
	approved := false
	var disclosure string
	session := loopOperationMCPSession(t, backend, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosure = request.Params.Message
		action := "decline"
		if approved {
			action = "accept"
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": approved}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	predicate := map[string]any{"filters": []any{map[string]any{"dimension": "source", "values": []string{fmt.Sprint(source.ID)}}}}
	explored := loopToolOutput[generated.ExploreHTTPResponse](t, call("explore_deletion_selection", predicate))
	selection := map[string]any{"mode": "all_matching", "predicate": predicate, "cache_revision": explored.CacheRevision, "search_provenance": explored.SearchProvenance, "exclusions": []string{fmt.Sprintf("source:%d:message:first", source.ID)}}
	preflight := loopToolOutput[generated.ExplorePreflightResponse](t, call("preflight_deletion_selection", map[string]any{"selection": selection}))
	assert.Equal(t, int64(1), preflight.Count)
	assert.Equal(t, int64(1), preflight.DeletableCount)
	args := map[string]any{"selection": selection, "operation_token": preflight.OperationToken}
	assert.True(t, call("stage_deletion", args).IsError)
	manifests, err := adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, manifests)
	approved = true
	tampered := map[string]any{"mode": "all_matching", "predicate": predicate, "cache_revision": explored.CacheRevision, "search_provenance": explored.SearchProvenance}
	assert.Equal(t, "operation_token_invalid", loopToolFailure(t, call("stage_deletion", map[string]any{"selection": tampered, "operation_token": preflight.OperationToken}))["error"])
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, manifests)
	result := call("stage_deletion", args)
	require.False(t, result.IsError)
	assert.Contains(t, disclosure, "Matched: 1; deletable: 1")
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	assert.Equal(t, []string{"second"}, manifests[0].GmailIDs)
	assert.Equal(t, "operation_token_invalid", loopToolFailure(t, call("stage_deletion", args))["error"])
	selection["cache_revision"] = "stale"
	assert.Equal(t, "archive_revision_changed", loopToolFailure(t, call("preflight_deletion_selection", map[string]any{"selection": selection}))["error"])
}

// These native failures and an oversized response are supplied by a transport
// fixture because expiry and a saturated candidate pool are not controllable
// through the daemon's public interface.
func TestMCPDeletionSelectionRefusalsAndResponseBound(t *testing.T) {
	code := "search_revision_changed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code == "oversized" {
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"preflight_deletion_selection", "stage_deletion_selection"}}
	args := map[string]any{"selection": map[string]any{"mode": "all_matching", "predicate": map[string]any{}, "cache_revision": "revision", "search_provenance": map[string]any{}}}
	for _, nativeCode := range []string{"search_revision_changed", "candidate_pool_saturated", "candidate_snapshot_expired", "operation_token_invalid"} {
		code = nativeCode
		result, err := backend.ExecuteOperation(t.Context(), "preflight_deletion_selection", args)
		require.NoError(t, err)
		assertLoopRefusal(t, result, nativeCode, false)
	}
	code = "oversized"
	result, err := backend.ExecuteOperation(t.Context(), "preflight_deletion_selection", args)
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
