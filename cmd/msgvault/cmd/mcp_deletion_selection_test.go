package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// saturatedDeletionSearch controls the candidate-total boundary while retaining
// the native lexical resolver and its real search results.
type saturatedDeletionSearch struct {
	*storeAPIAdapter
}

func (s *saturatedDeletionSearch) SearchMessageIDsQueryContext(ctx context.Context, q *search.Query, limit int) ([]int64, int64, error) {
	ids, _, err := s.storeAPIAdapter.SearchMessageIDsQueryContext(ctx, q, limit)
	return ids, int64(len(ids)) + 1, err
}

type partialDeletionSaver struct {
	*storeAPIAdapter

	path string
}

func (s *partialDeletionSaver) SaveCLIDeletionManifest(_ context.Context, manifest *deletion.Manifest) error {
	s.path = filepath.Join(s.config.Data.DataDir, "deletions", "pending", manifest.ID+".json")
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(`{"id":`), 0o600); err != nil {
		return err
	}
	return errors.New("injected partial manifest save")
}

func TestMCPDeletionSelectionNativePreflightAndStage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Synthetic")
	require.NoError(t, err)
	longQuery := strings.Repeat("a", 42) + "é"
	for _, id := range []string{"first", "second"} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: id, MessageType: "email", Subject: sql.NullString{String: longQuery, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
		require.NoError(t, err)
	}
	other, err := st.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(t, err)
	otherConversation, err := st.EnsureConversation(t.Context(), other.ID, "other-thread", "Synthetic")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: otherConversation, SourceMessageID: "other", MessageType: "email", Subject: sql.NullString{String: "Synthetic", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	_, err = st.CreateCollection("selected", "", []int64{source.ID})
	require.NoError(t, err)
	_, err = st.BackfillFTSContext(t.Context(), nil)
	require.NoError(t, err)
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
	elicitations := 0
	var disclosure string
	session := loopOperationMCPSession(t, backend, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		elicitations++
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
	type exploredResponse struct {
		generated.ExploreHTTPResponse

		Predicate map[string]any `json:"predicate"`
	}
	explored := loopToolOutput[exploredResponse](t, call("explore_deletion_selection", map[string]any{"collection": "selected"}))
	predicate := explored.Predicate
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
	before := elicitations
	assert.Equal(t, "operation_token_invalid", loopToolFailure(t, call("stage_deletion", map[string]any{"selection": tampered, "operation_token": preflight.OperationToken}))["error"])
	assert.Equal(t, before, elicitations)
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, manifests)
	result := call("stage_deletion", args)
	require.False(t, result.IsError)
	assert.Contains(t, disclosure, "owner@example.test")
	assert.Contains(t, disclosure, "Matched: 1; staged: 1; skipped: 0")
	assert.NotContains(t, disclosure, "predicate")
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	assert.Equal(t, []string{"second"}, manifests[0].GmailIDs)
	require.NotNil(t, manifests[0].Source)
	assert.Equal(t, "owner@example.test", manifests[0].Source.Identifier)
	assert.Equal(t, "selection: all_matching", manifests[0].Description)
	before = elicitations
	assert.Equal(t, "operation_token_invalid", loopToolFailure(t, call("stage_deletion", args))["error"])
	assert.Equal(t, before, elicitations)
	searched := loopToolOutput[exploredResponse](t, call("explore_deletion_selection", map[string]any{"collection": "selected", "query": longQuery, "search_mode": "full_text"}))
	require.NotEmpty(t, searched.Rows)
	explicit := map[string]any{"mode": "explicit", "predicate": searched.Predicate, "cache_revision": searched.CacheRevision, "search_provenance": searched.SearchProvenance, "row_keys": []string{searched.Rows[0].Key}}
	explicitPreflight := loopToolOutput[generated.ExplorePreflightResponse](t, call("preflight_deletion_selection", map[string]any{"selection": explicit}))
	explicitResult := loopToolOutput[struct {
		BatchID string `json:"batch_id"`
	}](t, call("stage_deletion", map[string]any{"selection": explicit, "operation_token": explicitPreflight.OperationToken}))
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	explicitFound := false
	for _, manifest := range manifests {
		if manifest.ID == explicitResult.BatchID {
			explicitFound = true
			assert.Equal(t, "selection: explicit", manifest.Description)
			assert.Equal(t, []string{strings.TrimPrefix(searched.Rows[0].Key, fmt.Sprintf("source:%d:message:", source.ID))}, manifest.GmailIDs)
		}
	}
	assert.True(t, explicitFound)
	querySelection := map[string]any{"mode": "all_matching", "predicate": searched.Predicate, "cache_revision": searched.CacheRevision, "search_provenance": searched.SearchProvenance}
	queryPreflight := loopToolOutput[generated.ExplorePreflightResponse](t, call("preflight_deletion_selection", map[string]any{"selection": querySelection}))
	queryResult := loopToolOutput[struct {
		BatchID string `json:"batch_id"`
	}](t, call("stage_deletion", map[string]any{"selection": querySelection, "operation_token": queryPreflight.OperationToken}))
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	var queryDescription string
	for _, manifest := range manifests {
		if manifest.ID == queryResult.BatchID {
			queryDescription = manifest.Description
		}
	}
	assert.Equal(t, "query: "+strings.Repeat("a", 42), queryDescription)
	assert.True(t, utf8.ValidString(queryDescription))
	assert.LessOrEqual(t, len(queryDescription), 50)
	preflight = loopToolOutput[generated.ExplorePreflightResponse](t, call("preflight_deletion_selection", map[string]any{"selection": selection}))
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "third", MessageType: "email", Subject: sql.NullString{String: "Synthetic", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, false)
	require.NoError(t, err)
	failure := loopToolFailure(t, call("stage_deletion", map[string]any{"selection": selection, "operation_token": preflight.OperationToken}))
	assert.Equal(t, "archive_revision_changed", failure["error"])
	assert.Equal(t, false, failure["operation_may_have_completed"])
	manifests, err = adapter.ListDeletionManifests(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, manifests, 3)
	invalid := loopToolFailure(t, call("explore_deletion_selection", map[string]any{"filters": []any{map[string]any{"dimension": "source", "values": []string{"bad"}}}}))
	assert.Equal(t, "invalid_filter", invalid["error"])
	assert.Equal(t, false, invalid["operation_may_have_completed"])

	saturated := &saturatedDeletionSearch{storeAPIAdapter: adapter}
	saturatedBackend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: saturated, Engine: engine, Logger: slog.New(slog.DiscardHandler)}))
	saturatedSession := loopOperationMCPSession(t, saturatedBackend, nil, nil)
	saturatedCall := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := saturatedSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	saturatedExplore := loopToolOutput[exploredResponse](t, saturatedCall("explore_deletion_selection", map[string]any{"collection": "selected", "query": longQuery, "search_mode": "full_text"}))
	require.NotEmpty(t, saturatedExplore.Rows)
	require.NotNil(t, saturatedExplore.CandidatePoolSaturated)
	assert.True(t, *saturatedExplore.CandidatePoolSaturated)
	saturatedSelection := map[string]any{"mode": "all_matching", "predicate": saturatedExplore.Predicate, "cache_revision": saturatedExplore.CacheRevision, "search_provenance": saturatedExplore.SearchProvenance}
	saturatedFailure := loopToolFailure(t, saturatedCall("preflight_deletion_selection", map[string]any{"selection": saturatedSelection}))
	assert.Equal(t, "candidate_pool_saturated", saturatedFailure["error"])
	assert.Equal(t, false, saturatedFailure["operation_may_have_completed"])

	partial := &partialDeletionSaver{storeAPIAdapter: adapter}
	partialBackend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: partial, Engine: engine, Logger: slog.New(slog.DiscardHandler)}))
	partialSession := loopOperationMCPSession(t, partialBackend, nil, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	partialCall := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := partialSession.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		return result
	}
	partialExplore := loopToolOutput[exploredResponse](t, partialCall("explore_deletion_selection", map[string]any{"collection": "selected"}))
	partialSelection := map[string]any{"mode": "all_matching", "predicate": partialExplore.Predicate, "cache_revision": partialExplore.CacheRevision, "search_provenance": partialExplore.SearchProvenance}
	partialPreflight := loopToolOutput[generated.ExplorePreflightResponse](t, partialCall("preflight_deletion_selection", map[string]any{"selection": partialSelection}))
	partialFailure := loopToolFailure(t, partialCall("stage_deletion", map[string]any{"selection": partialSelection, "operation_token": partialPreflight.OperationToken}))
	require.NotEmpty(t, partial.path)
	assert.FileExists(t, partial.path)
	assert.Equal(t, "operation_refused", partialFailure["error"])
	assert.Equal(t, true, partialFailure["operation_may_have_completed"])
}

// These native failures and an oversized response are supplied by a transport
// fixture because expiry is not controllable through the daemon's public interface.
func TestMCPDeletionSelectionRefusalsAndResponseBound(t *testing.T) {
	code := "search_revision_changed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code == "oversized" {
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		if code == "uncoded" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		status := http.StatusConflict
		if code == "stage_deletion_failed" {
			status = http.StatusInternalServerError
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend := &daemonMCPOperations{client: client, supported: []string{"preflight_deletion_selection", "stage_deletion_selection"}}
	args := map[string]any{"selection": map[string]any{"mode": "all_matching", "predicate": map[string]any{}, "cache_revision": "revision", "search_provenance": map[string]any{}}}
	for _, nativeCode := range []string{"search_revision_changed", "candidate_pool_saturated", "candidate_snapshot_expired"} {
		code = nativeCode
		result, err := backend.ExecuteOperation(t.Context(), "preflight_deletion_selection", args)
		require.NoError(t, err)
		assertLoopRefusal(t, result, nativeCode, false)
	}
	args["operation_token"] = "token"
	for _, nativeCode := range []string{"selection_changed", "stage_deletion_failed", "uncoded"} {
		code = nativeCode
		result, err := backend.ExecuteOperation(t.Context(), "stage_deletion_selection", args)
		require.NoError(t, err)
		if nativeCode == "uncoded" || nativeCode == "stage_deletion_failed" {
			assertLoopRefusal(t, result, "operation_refused", true)
		} else {
			assertLoopRefusal(t, result, nativeCode, false)
		}
	}
	code = "oversized"
	delete(args, "operation_token")
	result, err := backend.ExecuteOperation(t.Context(), "preflight_deletion_selection", args)
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
