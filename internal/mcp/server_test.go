package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// stubEmbedder is an EmbeddingClient placeholder for tests where the
// engine never reaches the embed step (e.g. ResolveActiveForFingerprint
// fails first). Calling Embed signals a test bug.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("stubEmbedder.Embed should not be called in this test")
}

// toolHandler is the function signature for MCP tool handler methods.
type toolHandler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)

// Response types for runTool generic calls.
type statsResponse struct {
	Stats        query.TotalStats    `json:"stats"`
	Accounts     []query.AccountInfo `json:"accounts"`
	VectorSearch *vector.StatsView   `json:"vector_search"`
}

type attachmentMeta struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// newTestHandlers creates a handlers instance with the given mock engine.
func newTestHandlers(eng *querytest.MockEngine) *handlers {
	return &handlers{engine: eng}
}

// callToolDirect invokes a handler directly with the given arguments and returns the raw result.
func callToolDirect(t *testing.T, name string, fn toolHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	result, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return result
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := r.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", r.Content[0])
	}
	return tc.Text
}

// runTool invokes a handler, asserts no error, and unmarshals the JSON result into T.
func runTool[T any](t *testing.T, name string, fn toolHandler, args map[string]any) T {
	t.Helper()
	r := callToolDirect(t, name, fn, args)
	if r.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, r))
	}
	var out T
	if err := json.Unmarshal([]byte(resultText(t, r)), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return out
}

// runToolExpectError invokes a handler and asserts it returns an error result.
func runToolExpectError(t *testing.T, name string, fn toolHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	r := callToolDirect(t, name, fn, args)
	if !r.IsError {
		t.Fatal("expected error result")
	}
	return r
}

func TestSearchMessages(t *testing.T) {
	eng := &querytest.MockEngine{
		SearchFastResults: []query.MessageSummary{
			testutil.NewMessageSummary(1).WithSubject("Hello").WithFromEmail("alice@example.com").WithSourceConversationID("thread-abc").Build(),
		},
	}
	h := newTestHandlers(eng)

	t.Run("valid query", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "search_messages", h.searchMessages, map[string]any{"query": "from:alice"})
		if len(msgs) != 1 || msgs[0].Subject != "Hello" {
			t.Fatalf("unexpected result: %v", msgs)
		}
		if msgs[0].SourceConversationID != "thread-abc" {
			t.Fatalf("expected SourceConversationID 'thread-abc', got %q", msgs[0].SourceConversationID)
		}
	})

	t.Run("missing query", func(t *testing.T) {
		runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{})
	})
}

func TestSearchFallbackToFTS(t *testing.T) {
	eng := &querytest.MockEngine{
		SearchFastResults: nil, // fast returns nothing
		SearchResults: []query.MessageSummary{
			testutil.NewMessageSummary(2).WithSubject("Body match").WithFromEmail("bob@example.com").Build(),
		},
	}
	h := newTestHandlers(eng)

	msgs := runTool[[]query.MessageSummary](t, "search_messages", h.searchMessages, map[string]any{"query": "important meeting notes"})
	if len(msgs) != 1 || msgs[0].ID != 2 {
		t.Fatalf("expected FTS fallback result, got: %v", msgs)
	}
}

func TestSearchMessages_HybridModeNotConfigured(t *testing.T) {
	// Handlers constructed without a hybridEngine must reject
	// mode=hybrid (and mode=vector) with a vector_not_enabled error.
	h := newTestHandlers(&querytest.MockEngine{})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "meeting notes",
		"mode":  "hybrid",
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "vector_not_enabled") {
		t.Fatalf("expected 'vector_not_enabled' error, got: %s", txt)
	}
}

// newHybridHandlersForErrorTest wires a real hybrid.Engine around the
// supplied backend so the search_messages handler exercises the engine's
// sentinel-error translation. mainDB is nil because the test query has
// no operators, so BuildFilter never touches it.
func newHybridHandlersForErrorTest(backend vector.Backend) *handlers {
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "nomic-embed:768",
		RRFK:                60,
		KPerSignal:          10,
	})
	return &handlers{
		engine:       &querytest.MockEngine{},
		hybridEngine: engine,
		backend:      backend,
	}
}

// TestSearchMessages_HybridErrIndexBuilding regression-guards the MCP
// handler's translation of vector.ErrIndexBuilding from the hybrid
// engine into an "index_building" tool error. The engine returns this
// when no active generation exists yet but a build is in progress.
func TestSearchMessages_HybridErrIndexBuilding(t *testing.T) {
	building := &vector.Generation{
		ID: 1, Model: "nomic-embed", Dimension: 768,
		Fingerprint: "nomic-embed:768", State: vector.GenerationBuilding,
	}
	// activeErr drives ResolveActiveForFingerprint to consult the
	// building generation; with one present the result is ErrIndexBuilding.
	h := newHybridHandlersForErrorTest(&fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
		building:  building,
	})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "anything",
		"mode":  "hybrid",
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "index_building") {
		t.Fatalf("expected 'index_building' error, got: %s", txt)
	}
}

// TestSearchMessages_HybridErrNotEnabled regression-guards the MCP
// handler's translation of vector.ErrNotEnabled from the hybrid engine
// into a "vector_not_enabled" tool error. The engine returns this when
// no generation exists at all (no active, no building).
func TestSearchMessages_HybridErrNotEnabled(t *testing.T) {
	// fakeBackend.activeErr=ErrNoActiveGeneration + building=nil
	// drives ResolveActiveForFingerprint into the ErrNotEnabled branch.
	h := newHybridHandlersForErrorTest(&fakeBackend{
		activeErr: vector.ErrNoActiveGeneration,
	})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "anything",
		"mode":  "hybrid",
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "vector_not_enabled") {
		t.Fatalf("expected 'vector_not_enabled' error, got: %s", txt)
	}
}

// realEmbedder returns a deterministic vector. Used for end-to-end
// MCP hybrid tests that exercise the engine's embed → backend.Search
// path; pickEmbedGeneration tests use stubEmbedder instead.
type realEmbedder struct {
	dim int
}

func (e realEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, e.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// TestSearchMessages_HybridFilterOnlyReturnsMissingFreeText
// regression-guards the wire-level contract that mode=vector|hybrid
// rejects filter-only queries (no free-text terms) with the
// "missing_free_text" tool error rather than passing an empty string
// into the embedder. Mirrors the API-side handler check so MCP and
// HTTP clients see the same boundary.
func TestSearchMessages_HybridFilterOnlyReturnsMissingFreeText(t *testing.T) {
	// A real engine wired to a backend with an active generation —
	// stubEmbedder keeps us safe if the handler ever forgets to
	// short-circuit (Embed will return an error, exposing the bug).
	backend := &fakeBackend{
		active: vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
	}
	engine := hybrid.NewEngine(backend, nil, stubEmbedder{}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	h := &handlers{
		engine:       &querytest.MockEngine{},
		hybridEngine: engine,
		backend:      backend,
	}

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "from:alice@example.com",
		"mode":  "vector",
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "missing_free_text") {
		t.Fatalf("expected 'missing_free_text' error, got: %s", txt)
	}
}

// TestSearchMessages_HybridPoolSaturatedAlwaysEmitted regression-guards
// the wire-level contract that pool_saturated is always present (and
// false on a successful, under-cap response). An `omitempty` slip
// would silently drop the field when false; clients that key off
// "saturated vs not" would break.
func TestSearchMessages_HybridPoolSaturatedAlwaysEmitted(t *testing.T) {
	backend := &fakeBackend{
		active: vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		// No hits → pool_saturated computes to false (len(hits) < limit).
		searchHits: nil,
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, hybrid.Config{
		ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10,
	})
	h := &handlers{
		engine:       &querytest.MockEngine{},
		hybridEngine: engine,
		backend:      backend,
	}

	r := callToolDirect(t, "search_messages", h.searchMessages, map[string]any{
		"query": "hello world",
		"mode":  "vector",
	})
	if r.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, r))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(resultText(t, r)), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	val, exists := raw["pool_saturated"]
	if !exists {
		t.Fatalf("pool_saturated key missing from successful response (raw=%s)", resultText(t, r))
	}
	if string(val) != "false" {
		t.Errorf("pool_saturated = %s, want false", string(val))
	}
}

func TestSearchMessages_HybridModePaginationUnsupported(t *testing.T) {
	// offset>0 must be rejected before any hybrid-engine lookup. The
	// pagination check runs first, so a missing hybridEngine does not
	// mask the pagination_unsupported error.
	h := newTestHandlers(&querytest.MockEngine{})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query":  "meeting notes",
		"mode":   "vector",
		"offset": float64(1),
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "pagination_unsupported") {
		t.Fatalf("expected 'pagination_unsupported' error, got: %s", txt)
	}
}

func TestSearchMessages_UnknownMode(t *testing.T) {
	h := newTestHandlers(&querytest.MockEngine{})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "meeting notes",
		"mode":  "bogus",
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "invalid mode") {
		t.Fatalf("expected 'invalid mode' error, got: %s", txt)
	}
}

func TestGetMessage(t *testing.T) {
	eng := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			42: testutil.NewMessageDetail(42).WithSubject("Test Message").WithBodyText("Hello world").WithSourceConversationID("thread-xyz").BuildPtr(),
		},
	}
	h := newTestHandlers(eng)

	t.Run("found", func(t *testing.T) {
		msg := runTool[query.MessageDetail](t, "get_message", h.getMessage, map[string]any{"id": float64(42)})
		if msg.Subject != "Test Message" {
			t.Fatalf("unexpected subject: %s", msg.Subject)
		}
		if msg.SourceConversationID != "thread-xyz" {
			t.Fatalf("expected SourceConversationID 'thread-xyz', got %q", msg.SourceConversationID)
		}
	})

	errorCases := []struct {
		name string
		args map[string]any
	}{
		{"not found", map[string]any{"id": float64(999)}},
		{"missing id", map[string]any{}},
		{"non-integer id", map[string]any{"id": float64(1.9)}},
		{"negative id", map[string]any{"id": float64(-1)}},
		{"overflow id", map[string]any{"id": float64(1e19)}},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			runToolExpectError(t, "get_message", h.getMessage, tt.args)
		})
	}
}

func TestGetStats_VectorDisabled(t *testing.T) {
	eng := &querytest.MockEngine{
		Stats: &query.TotalStats{
			MessageCount: 1000,
			TotalSize:    5000000,
			AccountCount: 2,
		},
		Accounts: []query.AccountInfo{
			{ID: 1, Identifier: "alice@gmail.com"},
			{ID: 2, Identifier: "bob@gmail.com"},
		},
	}
	// newTestHandlers leaves backend nil, mirroring a non-vector install.
	h := newTestHandlers(eng)

	resp := runTool[statsResponse](t, "get_stats", h.getStats, map[string]any{})

	if resp.Stats.MessageCount != 1000 {
		t.Fatalf("unexpected message count: %d", resp.Stats.MessageCount)
	}
	if len(resp.Accounts) != 2 {
		t.Fatalf("unexpected account count: %d", len(resp.Accounts))
	}
	if resp.VectorSearch != nil {
		t.Fatalf("expected VectorSearch to be nil when backend is disabled, got %+v",
			resp.VectorSearch)
	}

	// Also confirm the JSON payload omits the key entirely, so clients
	// that type-check the wire format see a clean absence rather than
	// a null value.
	r := callToolDirect(t, "get_stats", h.getStats, map[string]any{})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(resultText(t, r)), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["vector_search"]; ok {
		t.Errorf("expected 'vector_search' to be absent from JSON when backend is nil")
	}
}

func TestGetStats_VectorEnabled(t *testing.T) {
	eng := &querytest.MockEngine{
		Stats: &query.TotalStats{
			MessageCount: 100,
			AccountCount: 1,
		},
		Accounts: []query.AccountInfo{
			{ID: 1, Identifier: "alice@gmail.com"},
		},
	}
	activatedAt := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	fb := &fakeBackend{
		active: vector.Generation{
			ID:          5,
			Model:       "nomic-embed",
			Dimension:   768,
			Fingerprint: "nomic-embed:768",
			State:       vector.GenerationActive,
			ActivatedAt: &activatedAt,
		},
		stats: map[vector.GenerationID]vector.Stats{
			5: {EmbeddingCount: 100, PendingCount: 3},
		},
	}

	h := &handlers{engine: eng, backend: fb}

	resp := runTool[statsResponse](t, "get_stats", h.getStats, map[string]any{})

	if resp.VectorSearch == nil {
		t.Fatal("expected vector_search sub-object, got nil")
	}
	if !resp.VectorSearch.Enabled {
		t.Error("vector_search.enabled = false, want true")
	}
	if resp.VectorSearch.ActiveGeneration == nil {
		t.Fatal("expected vector_search.active_generation to be populated")
	}
	ag := resp.VectorSearch.ActiveGeneration
	if ag.ID != 5 {
		t.Errorf("active_generation.id = %d, want 5", ag.ID)
	}
	if ag.Model != "nomic-embed" {
		t.Errorf("active_generation.model = %q, want 'nomic-embed'", ag.Model)
	}
	if ag.MessageCount != 100 {
		t.Errorf("active_generation.message_count = %d, want 100", ag.MessageCount)
	}
	if resp.VectorSearch.PendingEmbeddingsTotal != 3 {
		t.Errorf("pending_embeddings_total = %d, want 3",
			resp.VectorSearch.PendingEmbeddingsTotal)
	}
	if resp.VectorSearch.BuildingGeneration != nil {
		t.Errorf("building_generation = %+v, want nil",
			resp.VectorSearch.BuildingGeneration)
	}
}

func TestAggregate(t *testing.T) {
	eng := &querytest.MockEngine{
		AggregateRows: []query.AggregateRow{
			{Key: "alice@example.com", Count: 100, TotalSize: 50000},
			{Key: "bob@example.com", Count: 50, TotalSize: 25000},
		},
	}
	h := newTestHandlers(eng)

	for _, groupBy := range []string{"sender", "recipient", "domain", "label", "time"} {
		t.Run(groupBy, func(t *testing.T) {
			rows := runTool[[]query.AggregateRow](t, "aggregate", h.aggregate, map[string]any{"group_by": groupBy})
			if len(rows) != 2 {
				t.Fatalf("expected 2 rows, got %d", len(rows))
			}
		})
	}

	errorCases := []struct {
		name string
		args map[string]any
	}{
		{"invalid group_by", map[string]any{"group_by": "invalid"}},
		{"missing group_by", map[string]any{}},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			runToolExpectError(t, "aggregate", h.aggregate, tt.args)
		})
	}
}

func TestListMessages(t *testing.T) {
	eng := &querytest.MockEngine{
		ListResults: []query.MessageSummary{
			testutil.NewMessageSummary(1).WithSubject("Test").WithFromEmail("alice@example.com").WithSourceConversationID("thread-list").Build(),
		},
	}
	h := newTestHandlers(eng)

	t.Run("valid filters", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "list_messages", h.listMessages, map[string]any{
			"from":  "alice@example.com",
			"after": "2024-01-01",
			"limit": float64(10),
		})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		if msgs[0].SourceConversationID != "thread-list" {
			t.Fatalf("expected SourceConversationID 'thread-list', got %q", msgs[0].SourceConversationID)
		}
	})

	errorCases := []struct {
		name string
		args map[string]any
	}{
		{"invalid after date", map[string]any{"after": "not-a-date"}},
		{"invalid before date", map[string]any{"before": "2024/01/01"}},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			runToolExpectError(t, "list_messages", h.listMessages, tt.args)
		})
	}
}

func TestAggregateInvalidDates(t *testing.T) {
	h := newTestHandlers(&querytest.MockEngine{})

	errorCases := []struct {
		name string
		args map[string]any
	}{
		{"invalid after", map[string]any{"group_by": "sender", "after": "bad"}},
		{"invalid before", map[string]any{"group_by": "sender", "before": "bad"}},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			runToolExpectError(t, "aggregate", h.aggregate, tt.args)
		})
	}
}

// createAttachmentFixture creates a content-addressed file under dir using the given hash.
func createAttachmentFixture(t *testing.T, dir string, hash string, content []byte) {
	t.Helper()
	hashDir := filepath.Join(dir, hash[:2])
	if err := os.MkdirAll(hashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hashDir, hash), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGetAttachment(t *testing.T) {
	tmpDir := t.TempDir()
	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	content := []byte("hello world PDF content")
	createAttachmentFixture(t, tmpDir, hash, content)

	eng := &querytest.MockEngine{
		Attachments: map[int64]*query.AttachmentInfo{
			10: {ID: 10, Filename: "report.pdf", MimeType: "application/pdf", Size: int64(len(content)), ContentHash: hash},
			11: {ID: 11, Filename: "no-hash.pdf", MimeType: "application/pdf", Size: 100, ContentHash: ""},
		},
	}
	h := &handlers{engine: eng, attachmentsDir: tmpDir}

	t.Run("valid", func(t *testing.T) {
		r := callToolDirect(t, "get_attachment", h.getAttachment, map[string]any{"attachment_id": float64(10)})
		if r.IsError {
			t.Fatalf("unexpected error: %s", resultText(t, r))
		}

		// Should have 2 content blocks: text metadata + embedded resource.
		if len(r.Content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(r.Content))
		}

		// First block: text with metadata JSON.
		tc, ok := r.Content[0].(mcp.TextContent)
		if !ok {
			t.Fatalf("expected TextContent, got %T", r.Content[0])
		}
		var meta attachmentMeta
		if err := json.Unmarshal([]byte(tc.Text), &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		if meta.Filename != "report.pdf" {
			t.Fatalf("unexpected filename: %s", meta.Filename)
		}
		if meta.MimeType != "application/pdf" {
			t.Fatalf("unexpected mime_type: %s", meta.MimeType)
		}
		if meta.Size != int64(len(content)) {
			t.Fatalf("unexpected size: %d", meta.Size)
		}

		// Second block: embedded resource with blob.
		er, ok := r.Content[1].(mcp.EmbeddedResource)
		if !ok {
			t.Fatalf("expected EmbeddedResource, got %T", r.Content[1])
		}
		blob, ok := er.Resource.(mcp.BlobResourceContents)
		if !ok {
			t.Fatalf("expected BlobResourceContents, got %T", er.Resource)
		}
		if blob.MIMEType != "application/pdf" {
			t.Fatalf("unexpected blob MIME type: %s", blob.MIMEType)
		}
		decoded, err := base64.StdEncoding.DecodeString(blob.Blob)
		if err != nil {
			t.Fatal(err)
		}
		if string(decoded) != string(content) {
			t.Fatalf("content mismatch: got %q", string(decoded))
		}
	})

	t.Run("empty mime type defaults to octet-stream", func(t *testing.T) {
		noMimeHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		noMimeContent := []byte("binary data")
		createAttachmentFixture(t, tmpDir, noMimeHash, noMimeContent)

		h2 := &handlers{
			engine: &querytest.MockEngine{
				Attachments: map[int64]*query.AttachmentInfo{
					50: {ID: 50, Filename: "data.bin", MimeType: "", Size: int64(len(noMimeContent)), ContentHash: noMimeHash},
				},
			},
			attachmentsDir: tmpDir,
		}
		r := callToolDirect(t, "get_attachment", h2.getAttachment, map[string]any{"attachment_id": float64(50)})
		if r.IsError {
			t.Fatalf("unexpected error: %s", resultText(t, r))
		}

		var meta attachmentMeta
		tc := r.Content[0].(mcp.TextContent)
		if err := json.Unmarshal([]byte(tc.Text), &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		if meta.MimeType != "application/octet-stream" {
			t.Fatalf("expected default mime_type, got %s", meta.MimeType)
		}

		er := r.Content[1].(mcp.EmbeddedResource)
		blob := er.Resource.(mcp.BlobResourceContents)
		if blob.MIMEType != "application/octet-stream" {
			t.Fatalf("expected default blob MIME type, got %s", blob.MIMEType)
		}
	})

	t.Run("filename with spaces and unicode", func(t *testing.T) {
		unicodeHash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		unicodeContent := []byte("unicode file")
		createAttachmentFixture(t, tmpDir, unicodeHash, unicodeContent)

		h2 := &handlers{
			engine: &querytest.MockEngine{
				Attachments: map[int64]*query.AttachmentInfo{
					51: {ID: 51, Filename: "report 2024✓.pdf", MimeType: "application/pdf", Size: int64(len(unicodeContent)), ContentHash: unicodeHash},
				},
			},
			attachmentsDir: tmpDir,
		}
		r := callToolDirect(t, "get_attachment", h2.getAttachment, map[string]any{"attachment_id": float64(51)})
		if r.IsError {
			t.Fatalf("unexpected error: %s", resultText(t, r))
		}

		// Metadata JSON must be valid and preserve the filename exactly.
		var meta attachmentMeta
		tc := r.Content[0].(mcp.TextContent)
		if err := json.Unmarshal([]byte(tc.Text), &meta); err != nil {
			t.Fatalf("metadata is not valid JSON: %v\nraw: %s", err, tc.Text)
		}
		if meta.Filename != "report 2024✓.pdf" {
			t.Fatalf("unexpected filename: %s", meta.Filename)
		}

		// URI must percent-encode spaces and non-ASCII characters.
		er := r.Content[1].(mcp.EmbeddedResource)
		blob := er.Resource.(mcp.BlobResourceContents)
		const wantURI = "attachment:///51/report%202024%E2%9C%93.pdf"
		if blob.URI != wantURI {
			t.Fatalf("unexpected URI:\n got: %s\nwant: %s", blob.URI, wantURI)
		}
	})

	// Error cases using the shared engine/handler.
	sharedErrorCases := []struct {
		name string
		args map[string]any
	}{
		{"missing attachment_id", map[string]any{}},
		{"non-integer id", map[string]any{"attachment_id": float64(1.5)}},
		{"not found", map[string]any{"attachment_id": float64(999)}},
		{"missing hash", map[string]any{"attachment_id": float64(11)}},
	}
	for _, tt := range sharedErrorCases {
		t.Run(tt.name, func(t *testing.T) {
			runToolExpectError(t, "get_attachment", h.getAttachment, tt.args)
		})
	}

	// Error cases requiring custom engine/handler configuration.
	customErrorCases := []struct {
		name        string
		attachments map[int64]*query.AttachmentInfo
		attDir      string
		args        map[string]any
		errContains string // if non-empty, assert error text contains this
	}{
		{
			name:        "invalid content hash (path traversal)",
			attachments: map[int64]*query.AttachmentInfo{30: {ID: 30, Filename: "evil.pdf", MimeType: "application/pdf", Size: 100, ContentHash: "../../etc/passwd"}},
			attDir:      tmpDir,
			args:        map[string]any{"attachment_id": float64(30)},
			errContains: "invalid content hash",
		},
		{
			name:        "non-hex content hash",
			attachments: map[int64]*query.AttachmentInfo{31: {ID: 31, Filename: "bad.pdf", MimeType: "application/pdf", Size: 100, ContentHash: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}},
			attDir:      tmpDir,
			args:        map[string]any{"attachment_id": float64(31)},
		},
		{
			name:        "attachmentsDir not configured",
			attachments: map[int64]*query.AttachmentInfo{10: {ID: 10, Filename: "report.pdf", MimeType: "application/pdf", Size: 100, ContentHash: hash}},
			attDir:      "",
			args:        map[string]any{"attachment_id": float64(10)},
		},
		{
			name:        "file not on disk",
			attachments: map[int64]*query.AttachmentInfo{20: {ID: 20, Filename: "gone.pdf", MimeType: "application/pdf", Size: 100, ContentHash: "deadbeef1234567890abcdef1234567890abcdef1234567890abcdef12345678"}},
			attDir:      tmpDir,
			args:        map[string]any{"attachment_id": float64(20)},
		},
	}
	for _, tt := range customErrorCases {
		t.Run(tt.name, func(t *testing.T) {
			h2 := &handlers{
				engine:         &querytest.MockEngine{Attachments: tt.attachments},
				attachmentsDir: tt.attDir,
			}
			r := runToolExpectError(t, "get_attachment", h2.getAttachment, tt.args)
			if tt.errContains != "" {
				if txt := resultText(t, r); !strings.Contains(txt, tt.errContains) {
					t.Fatalf("expected error containing %q, got: %s", tt.errContains, txt)
				}
			}
		})
	}

	t.Run("oversized attachment", func(t *testing.T) {
		bigHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		createAttachmentFixture(t, tmpDir, bigHash, nil)
		bigPath := filepath.Join(tmpDir, bigHash[:2], bigHash)
		if err := os.Truncate(bigPath, maxAttachmentSize+1); err != nil {
			t.Fatal(err)
		}

		h2 := &handlers{
			engine: &querytest.MockEngine{
				Attachments: map[int64]*query.AttachmentInfo{
					40: {ID: 40, Filename: "huge.bin", MimeType: "application/octet-stream", Size: maxAttachmentSize + 1, ContentHash: bigHash},
				},
			},
			attachmentsDir: tmpDir,
		}
		r := runToolExpectError(t, "get_attachment", h2.getAttachment, map[string]any{"attachment_id": float64(40)})
		if txt := resultText(t, r); !strings.Contains(txt, "too large") {
			t.Fatalf("expected 'too large' error, got: %s", txt)
		}
	})
}

type exportResponse struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func TestExportAttachment(t *testing.T) {
	srcDir := t.TempDir()
	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	content := []byte("hello world PDF content")
	createAttachmentFixture(t, srcDir, hash, content)

	eng := &querytest.MockEngine{
		Attachments: map[int64]*query.AttachmentInfo{
			10: {ID: 10, Filename: "report.pdf", MimeType: "application/pdf", Size: int64(len(content)), ContentHash: hash},
		},
	}
	h := &handlers{engine: eng, attachmentsDir: srcDir}

	t.Run("export to custom destination", func(t *testing.T) {
		destDir := t.TempDir()
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		if resp.Filename != "report.pdf" {
			t.Fatalf("unexpected filename: %s", resp.Filename)
		}
		if resp.Size != int64(len(content)) {
			t.Fatalf("unexpected size: %d", resp.Size)
		}
		wantPath := filepath.Join(destDir, "report.pdf")
		if resp.Path != wantPath {
			t.Fatalf("unexpected path: %s (want %s)", resp.Path, wantPath)
		}
		got, err := os.ReadFile(wantPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(content) {
			t.Fatalf("content mismatch")
		}
	})

	t.Run("filename collision appends suffix", func(t *testing.T) {
		destDir := t.TempDir()
		// Create existing file to force collision.
		if err := os.WriteFile(filepath.Join(destDir, "report.pdf"), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		if resp.Filename != "report_1.pdf" {
			t.Fatalf("expected report_1.pdf, got %s", resp.Filename)
		}
		// Original file should be untouched.
		old, _ := os.ReadFile(filepath.Join(destDir, "report.pdf"))
		if string(old) != "old" {
			t.Fatal("original file was overwritten")
		}
	})

	t.Run("directory collision appends suffix", func(t *testing.T) {
		destDir := t.TempDir()
		// Create a directory with the same name as the attachment.
		if err := os.Mkdir(filepath.Join(destDir, "report.pdf"), 0755); err != nil {
			t.Fatal(err)
		}
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		if resp.Filename != "report_1.pdf" {
			t.Fatalf("expected report_1.pdf, got %s", resp.Filename)
		}
	})

	t.Run("default destination is ~/Downloads", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		downloads := filepath.Join(home, "Downloads")
		if err := os.Mkdir(downloads, 0755); err != nil {
			t.Fatal(err)
		}

		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
		})
		if !strings.HasPrefix(resp.Path, downloads) {
			t.Fatalf("expected path under ~/Downloads, got %s", resp.Path)
		}
	})

	t.Run("invalid destination", func(t *testing.T) {
		runToolExpectError(t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   "/nonexistent/path/that/does/not/exist",
		})
	})

	t.Run("missing attachment_id", func(t *testing.T) {
		runToolExpectError(t, "export_attachment", h.exportAttachment, map[string]any{})
	})

	t.Run("attachment not found", func(t *testing.T) {
		runToolExpectError(t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(999),
		})
	})
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"report.pdf", "report.pdf"},
		{"file:name.pdf", "file_name.pdf"},
		{"path/to/file.txt", "path_to_file.txt"},
		{"back\\slash.doc", "back_slash.doc"},
		{"tab\there.txt", "tab_here.txt"},
		{"new\nline.txt", "new_line.txt"},
		{"pipe|star*.txt", "pipe_star_.txt"},
		{"quotes\"angle<>.txt", "quotes_angle__.txt"},
		{"clean-file_v2.pdf", "clean-file_v2.pdf"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := export.SanitizeFilename(tc.input)
			if got != tc.want {
				t.Fatalf("SanitizeFilename(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExportAttachment_EdgeFilenames(t *testing.T) {
	srcDir := t.TempDir()
	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	content := []byte("data")
	createAttachmentFixture(t, srcDir, hash, content)

	tests := []struct {
		name         string
		filename     string
		wantFilename string // expected output filename
	}{
		{"empty filename falls back to hash", "", hash},
		{"dot filename falls back to hash", ".", hash},
		{"path traversal stripped by Base", "../evil.pdf", "evil.pdf"},
		{"special chars sanitized", "file:name|v2.pdf", "file_name_v2.pdf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			destDir := t.TempDir()
			h := &handlers{
				engine: &querytest.MockEngine{
					Attachments: map[int64]*query.AttachmentInfo{
						1: {ID: 1, Filename: tc.filename, MimeType: "application/pdf", Size: int64(len(content)), ContentHash: hash},
					},
				},
				attachmentsDir: srcDir,
			}
			resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
				"attachment_id": float64(1),
				"destination":   destDir,
			})
			if resp.Filename != tc.wantFilename {
				t.Fatalf("expected filename %q, got %q", tc.wantFilename, resp.Filename)
			}
		})
	}
}

func TestGetAttachment_RejectsOversizedBeforeFileIO(t *testing.T) {
	// The att.Size metadata from the database tells us this attachment is too
	// large BEFORE we try to open the file. The handler should reject with a
	// "too large" error immediately, without attempting any file I/O.
	//
	// Without the pre-flight check, the handler would try to open the file
	// and produce a misleading "not available" error instead.

	oversizeAtt := &query.AttachmentInfo{
		ID:          99,
		Filename:    "huge.bin",
		MimeType:    "application/octet-stream",
		Size:        maxAttachmentSize + 1,
		ContentHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}

	h := &handlers{
		engine: &querytest.MockEngine{
			Attachments: map[int64]*query.AttachmentInfo{99: oversizeAtt},
		},
		attachmentsDir: t.TempDir(), // empty dir — file does NOT exist on disk
	}

	// getAttachment should reject based on metadata size, not file I/O error
	r := runToolExpectError(t, "get_attachment", h.getAttachment, map[string]any{
		"attachment_id": float64(99),
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "too large") {
		t.Fatalf("expected 'too large' rejection from metadata check, got: %s", txt)
	}
}

func TestExportAttachment_RejectsOversizedBeforeFileIO(t *testing.T) {
	oversizeAtt := &query.AttachmentInfo{
		ID:          99,
		Filename:    "huge.bin",
		MimeType:    "application/octet-stream",
		Size:        maxAttachmentSize + 1,
		ContentHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}

	h := &handlers{
		engine: &querytest.MockEngine{
			Attachments: map[int64]*query.AttachmentInfo{99: oversizeAtt},
		},
		attachmentsDir: t.TempDir(),
	}

	r := runToolExpectError(t, "export_attachment", h.exportAttachment, map[string]any{
		"attachment_id": float64(99),
		"destination":   t.TempDir(),
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "too large") {
		t.Fatalf("expected 'too large' rejection from metadata check, got: %s", txt)
	}
}

func TestLimitArgClamping(t *testing.T) {
	tests := []struct {
		name string
		val  float64
		want int
	}{
		{"negative clamped to 0", -5, 0},
		{"zero stays zero", 0, 0},
		{"normal value", 50, 50},
		{"above max clamped", 5000, maxLimit},
		{"huge float clamped", 1e18, maxLimit},
		{"NaN clamped to 0", math.NaN(), 0},
		{"Inf clamped", math.Inf(1), maxLimit},
		{"negative Inf clamped to 0", math.Inf(-1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := limitArg(map[string]any{"x": tt.val}, "x", 20)
			if got != tt.want {
				t.Fatalf("limitArg(%v) = %d, want %d", tt.val, got, tt.want)
			}
		})
	}
}

func TestAccountFilter(t *testing.T) {
	eng := &querytest.MockEngine{
		Accounts: []query.AccountInfo{
			{ID: 1, Identifier: "alice@gmail.com"},
			{ID: 2, Identifier: "bob@gmail.com"},
		},
		SearchFastResults: []query.MessageSummary{
			testutil.NewMessageSummary(1).WithSubject("Test").WithFromEmail("alice@gmail.com").Build(),
		},
		ListResults: []query.MessageSummary{
			testutil.NewMessageSummary(2).WithSubject("List Test").WithFromEmail("bob@gmail.com").Build(),
		},
		AggregateRows: []query.AggregateRow{
			{Key: "alice@gmail.com", Count: 100},
		},
	}
	h := newTestHandlers(eng)

	t.Run("search with valid account", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "search_messages", h.searchMessages, map[string]any{
			"query":   "test",
			"account": "alice@gmail.com",
		})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
	})

	t.Run("search with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
			"query":   "test",
			"account": "unknown@gmail.com",
		})
		txt := resultText(t, r)
		if !strings.Contains(txt, "account not found") {
			t.Fatalf("expected 'account not found' error, got: %s", txt)
		}
	})

	t.Run("list with valid account", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "list_messages", h.listMessages, map[string]any{
			"account": "bob@gmail.com",
		})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
	})

	t.Run("list with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "list_messages", h.listMessages, map[string]any{
			"account": "unknown@gmail.com",
		})
		txt := resultText(t, r)
		if !strings.Contains(txt, "account not found") {
			t.Fatalf("expected 'account not found' error, got: %s", txt)
		}
	})

	t.Run("aggregate with valid account", func(t *testing.T) {
		rows := runTool[[]query.AggregateRow](t, "aggregate", h.aggregate, map[string]any{
			"group_by": "sender",
			"account":  "alice@gmail.com",
		})
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
	})

	t.Run("aggregate with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "aggregate", h.aggregate, map[string]any{
			"group_by": "sender",
			"account":  "unknown@gmail.com",
		})
		txt := resultText(t, r)
		if !strings.Contains(txt, "account not found") {
			t.Fatalf("expected 'account not found' error, got: %s", txt)
		}
	})

	t.Run("empty account means no filter", func(t *testing.T) {
		// Empty string should not filter - return all results
		msgs := runTool[[]query.MessageSummary](t, "search_messages", h.searchMessages, map[string]any{
			"query":   "test",
			"account": "",
		})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
	})
}

// stageDeletionResponse matches the JSON response from stageDeletion.
type stageDeletionResponse struct {
	BatchID      string `json:"batch_id"`
	MessageCount int    `json:"message_count"`
	Status       string `json:"status"`
	NextStep     string `json:"next_step"`
}

func TestStageDeletion(t *testing.T) {
	eng := &querytest.MockEngine{
		Accounts: []query.AccountInfo{
			{ID: 1, Identifier: "alice@gmail.com"},
		},
		SearchFastResults: []query.MessageSummary{
			testutil.NewMessageSummary(1).
				WithSubject("Newsletter").
				WithFromEmail("news@example.com").
				WithSourceMessageID("gmail-001").
				Build(),
			testutil.NewMessageSummary(2).
				WithSubject("Promo").
				WithFromEmail("promo@example.com").
				WithSourceMessageID("gmail-002").
				Build(),
		},
		GmailIDs: []string{"gmail-010", "gmail-011"},
	}

	t.Run("query-based staging", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		resp := runTool[stageDeletionResponse](
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"query": "from:news"},
		)
		if resp.MessageCount != 2 {
			t.Fatalf("expected 2 messages, got %d", resp.MessageCount)
		}
		if resp.Status != "pending" {
			t.Fatalf("expected pending status, got %s", resp.Status)
		}
		if resp.BatchID == "" {
			t.Fatal("expected non-empty batch_id")
		}
	})

	t.Run("structured filter staging", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		resp := runTool[stageDeletionResponse](
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"from": "news@example.com"},
		)
		if resp.MessageCount != 2 {
			t.Fatalf("expected 2 messages, got %d", resp.MessageCount)
		}
	})

	t.Run("whitespace-only query rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"query": "   "},
		)
		txt := resultText(t, r)
		if !strings.Contains(txt, "must provide") {
			t.Fatalf("expected validation error, got: %s", txt)
		}
	})

	t.Run("query and filters rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{
				"query": "from:alice",
				"from":  "alice@example.com",
			},
		)
		txt := resultText(t, r)
		if !strings.Contains(txt, "not both") {
			t.Fatalf("expected mutual exclusion error, got: %s", txt)
		}
	})

	t.Run("no filters rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{},
		)
		txt := resultText(t, r)
		if !strings.Contains(txt, "must provide") {
			t.Fatalf("expected validation error, got: %s", txt)
		}
	})

	t.Run("no matches returns error", func(t *testing.T) {
		dataDir := t.TempDir()
		emptyEng := &querytest.MockEngine{
			SearchFastResults: nil,
			GmailIDs:          nil,
		}
		h := &handlers{engine: emptyEng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"from": "nobody@example.com"},
		)
		txt := resultText(t, r)
		if !strings.Contains(txt, "no messages match") {
			t.Fatalf("expected no-match error, got: %s", txt)
		}
	})

	t.Run("account filter propagated", func(t *testing.T) {
		dataDir := t.TempDir()
		var capturedFilter query.MessageFilter
		eng := &querytest.MockEngine{
			Accounts: []query.AccountInfo{
				{ID: 1, Identifier: "alice@gmail.com"},
			},
			GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
				capturedFilter = f
				return []string{"gmail-100"}, nil
			},
		}
		h := &handlers{engine: eng, dataDir: dataDir}

		runTool[stageDeletionResponse](
			t, "stage_deletion", h.stageDeletion,
			map[string]any{
				"account": "alice@gmail.com",
				"from":    "news@example.com",
			},
		)
		if capturedFilter.SourceID == nil {
			t.Fatal("expected SourceID to be set")
		}
		if *capturedFilter.SourceID != 1 {
			t.Fatalf("expected SourceID 1, got %d", *capturedFilter.SourceID)
		}
	})

	t.Run("invalid account rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{
				"account": "unknown@gmail.com",
				"from":    "news@example.com",
			},
		)
		txt := resultText(t, r)
		if !strings.Contains(txt, "account not found") {
			t.Fatalf("expected account error, got: %s", txt)
		}
	})

	t.Run("structured filter limit enforced", func(t *testing.T) {
		dataDir := t.TempDir()
		var capturedFilter query.MessageFilter
		eng := &querytest.MockEngine{
			GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
				capturedFilter = f
				return []string{"gmail-200"}, nil
			},
		}
		h := &handlers{engine: eng, dataDir: dataDir}

		runTool[stageDeletionResponse](
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"domain": "example.com"},
		)
		if capturedFilter.Pagination.Limit != maxStageDeletionResults {
			t.Fatalf("expected limit %d, got %d",
				maxStageDeletionResults, capturedFilter.Pagination.Limit)
		}
	})
}

// fakeBackend is a minimal vector.Backend used to exercise
// find_similar_messages and get_stats without standing up a real
// sqlitevec backend. LoadVector/ActiveGeneration/Search are driven
// by their dedicated fields; BuildingGeneration and Stats expose
// optional fields so the get_stats tests can populate them. Methods
// not otherwise configured return errors and should not be called.
type fakeBackend struct {
	loadVec     []float32
	loadErr     error
	active      vector.Generation
	activeErr   error
	searchHits  []vector.Hit
	searchErr   error
	building    *vector.Generation
	buildingErr error
	stats       map[vector.GenerationID]vector.Stats
	statsErr    error
}

func (f *fakeBackend) LoadVector(_ context.Context, _ int64) ([]float32, error) {
	return f.loadVec, f.loadErr
}
func (f *fakeBackend) ActiveGeneration(_ context.Context) (vector.Generation, error) {
	return f.active, f.activeErr
}
func (f *fakeBackend) Search(_ context.Context, _ vector.GenerationID, _ []float32, _ int, _ vector.Filter) ([]vector.Hit, error) {
	return f.searchHits, f.searchErr
}
func (f *fakeBackend) CreateGeneration(_ context.Context, _ string, _ int) (vector.GenerationID, error) {
	return 0, errors.New("not implemented")
}
func (f *fakeBackend) ActivateGeneration(_ context.Context, _ vector.GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) RetireGeneration(_ context.Context, _ vector.GenerationID) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) BuildingGeneration(_ context.Context) (*vector.Generation, error) {
	return f.building, f.buildingErr
}
func (f *fakeBackend) Upsert(_ context.Context, _ vector.GenerationID, _ []vector.Chunk) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) Delete(_ context.Context, _ vector.GenerationID, _ []int64) error {
	return errors.New("not implemented")
}
func (f *fakeBackend) Stats(_ context.Context, gen vector.GenerationID) (vector.Stats, error) {
	if f.statsErr != nil {
		return vector.Stats{}, f.statsErr
	}
	return f.stats[gen], nil
}
func (f *fakeBackend) Close() error { return nil }
func (f *fakeBackend) EnsureSeeded(_ context.Context, _ vector.GenerationID) error {
	return nil
}

var _ vector.Backend = (*fakeBackend)(nil)

// similarResponse matches the JSON response shape of find_similar_messages.
type similarResponse struct {
	SeedMessageID int64                  `json:"seed_message_id"`
	Returned      int                    `json:"returned"`
	Generation    generationSummary      `json:"generation"`
	Messages      []query.MessageSummary `json:"messages"`
}

type generationSummary struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

func TestFindSimilarMessages_VectorNotEnabled(t *testing.T) {
	h := newTestHandlers(&querytest.MockEngine{})

	r := runToolExpectError(t, "find_similar_messages", h.findSimilarMessages, map[string]any{
		"message_id": float64(1),
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "vector_not_enabled") {
		t.Fatalf("expected 'vector_not_enabled' error, got: %s", txt)
	}
}

// TestSearchMessagesTool_AdvertisesVectorModesOnlyWhenAvailable guards the
// capability-discovery contract: when the server has no hybrid engine,
// the search_messages tool omits the "mode" and "explain" parameters so
// clients don't build vector requests that will fail at runtime.
func TestSearchMessagesTool_AdvertisesVectorModesOnlyWhenAvailable(t *testing.T) {
	disabled := searchMessagesTool(false)
	if _, ok := disabled.InputSchema.Properties["mode"]; ok {
		t.Errorf("vectorAvailable=false: tool advertises 'mode' but vector modes are unsupported")
	}
	if _, ok := disabled.InputSchema.Properties["explain"]; ok {
		t.Errorf("vectorAvailable=false: tool advertises 'explain' but vector modes are unsupported")
	}
	if strings.Contains(disabled.Description, "mode=vector") || strings.Contains(disabled.Description, "mode=hybrid") {
		t.Errorf("vectorAvailable=false: tool description mentions vector modes: %q", disabled.Description)
	}

	enabled := searchMessagesTool(true)
	if _, ok := enabled.InputSchema.Properties["mode"]; !ok {
		t.Errorf("vectorAvailable=true: tool is missing 'mode' parameter")
	}
	if _, ok := enabled.InputSchema.Properties["explain"]; !ok {
		t.Errorf("vectorAvailable=true: tool is missing 'explain' parameter")
	}
	if !strings.Contains(enabled.Description, "free-text") {
		t.Errorf("vectorAvailable=true: tool description should call out the free-text requirement, got: %q", enabled.Description)
	}
}

func TestFindSimilarMessages_MissingID(t *testing.T) {
	h := &handlers{
		engine:  &querytest.MockEngine{},
		backend: &fakeBackend{},
	}

	r := runToolExpectError(t, "find_similar_messages", h.findSimilarMessages, map[string]any{})
	txt := resultText(t, r)
	if !strings.Contains(txt, "message_id") {
		t.Fatalf("expected error mentioning 'message_id', got: %s", txt)
	}
}

func TestFindSimilarMessages_HappyPath(t *testing.T) {
	seed := make([]float32, 4)
	for i := range seed {
		seed[i] = float32(i)
	}
	fb := &fakeBackend{
		loadVec: seed,
		active: vector.Generation{
			ID:          7,
			Model:       "nomic-embed",
			Dimension:   4,
			Fingerprint: "nomic-embed:4",
			State:       vector.GenerationActive,
		},
		searchHits: []vector.Hit{
			{MessageID: 100, Score: 0.99, Rank: 1}, // seed — must be filtered out
			{MessageID: 200, Score: 0.95, Rank: 2},
			{MessageID: 300, Score: 0.90, Rank: 3},
		},
	}

	eng := &querytest.MockEngine{
		Messages: map[int64]*query.MessageDetail{
			200: testutil.NewMessageDetail(200).WithSubject("related one").BuildPtr(),
			300: testutil.NewMessageDetail(300).WithSubject("related two").BuildPtr(),
		},
	}

	h := &handlers{engine: eng, backend: fb}

	resp := runTool[similarResponse](t, "find_similar_messages", h.findSimilarMessages, map[string]any{
		"message_id": float64(100),
		"limit":      float64(20),
	})

	if resp.SeedMessageID != 100 {
		t.Fatalf("seed_message_id = %d, want 100", resp.SeedMessageID)
	}
	if resp.Returned != 2 {
		t.Fatalf("returned = %d, want 2", resp.Returned)
	}
	if resp.Generation.ID != 7 {
		t.Fatalf("generation.id = %d, want 7", resp.Generation.ID)
	}
	if resp.Generation.Fingerprint != "nomic-embed:4" {
		t.Fatalf("generation.fingerprint = %s, want nomic-embed:4", resp.Generation.Fingerprint)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(resp.Messages))
	}
	for _, m := range resp.Messages {
		if m.ID == 100 {
			t.Fatalf("seed message 100 must not appear in results")
		}
	}
	if resp.Messages[0].ID != 200 || resp.Messages[1].ID != 300 {
		t.Fatalf("unexpected order: %v", resp.Messages)
	}
}

func TestFindSimilarMessages_NoActiveGeneration(t *testing.T) {
	fb := &fakeBackend{
		loadErr: vector.ErrNoActiveGeneration,
	}
	h := &handlers{engine: &querytest.MockEngine{}, backend: fb}

	r := runToolExpectError(t, "find_similar_messages", h.findSimilarMessages, map[string]any{
		"message_id": float64(1),
	})
	txt := resultText(t, r)
	if !strings.Contains(txt, "no_active_generation") {
		t.Fatalf("expected 'no_active_generation' error, got: %s", txt)
	}
}

func TestSearchByDomains(t *testing.T) {
	eng := &querytest.MockEngine{
		SearchResults: []query.MessageSummary{
			testutil.NewMessageSummary(1).WithSubject("From Acme").WithFromEmail("alice@acme.com").Build(),
			testutil.NewMessageSummary(2).WithSubject("To Acme").WithFromEmail("bob@example.com").Build(),
		},
	}
	h := newTestHandlers(eng)

	t.Run("valid domains", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": "acme.com,example.com"})
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
	})

	t.Run("domains with whitespace", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": " acme.com , example.com "})
		if len(msgs) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(msgs))
		}
	})

	t.Run("missing domains", func(t *testing.T) {
		runToolExpectError(t, "search_by_domains", h.searchByDomains, map[string]any{})
	})

	t.Run("empty domains string", func(t *testing.T) {
		runToolExpectError(t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": ""})
	})

	t.Run("whitespace-only domains", func(t *testing.T) {
		runToolExpectError(t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": "  ,  , "})
	})

	t.Run("arguments forwarded correctly", func(t *testing.T) {
		var capturedDomains []string
		var capturedLimit, capturedOffset int
		eng := &querytest.MockEngine{
			SearchByDomainsFunc: func(_ context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
				capturedDomains = domains
				capturedLimit = limit
				capturedOffset = offset
				if after == nil {
					t.Fatal("expected after to be set")
				}
				if before == nil {
					t.Fatal("expected before to be set")
				}
				return []query.MessageSummary{
					testutil.NewMessageSummary(1).WithSubject("Match").Build(),
				}, nil
			},
		}
		h := newTestHandlers(eng)

		msgs := runTool[[]query.MessageSummary](t, "search_by_domains", h.searchByDomains,
			map[string]any{
				"domains": "acme.com,globex.com",
				"limit":   float64(50),
				"offset":  float64(10),
				"after":   "2024-01-01",
				"before":  "2024-12-31",
			})
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message, got %d", len(msgs))
		}
		if len(capturedDomains) != 2 || capturedDomains[0] != "acme.com" || capturedDomains[1] != "globex.com" {
			t.Fatalf("unexpected domains: %v", capturedDomains)
		}
		if capturedLimit != 50 {
			t.Fatalf("expected limit 50, got %d", capturedLimit)
		}
		if capturedOffset != 10 {
			t.Fatalf("expected offset 10, got %d", capturedOffset)
		}
	})

	t.Run("default limit and offset", func(t *testing.T) {
		var capturedLimit, capturedOffset int
		eng := &querytest.MockEngine{
			SearchByDomainsFunc: func(_ context.Context, _ []string, _, _ *time.Time, limit, offset int) ([]query.MessageSummary, error) {
				capturedLimit = limit
				capturedOffset = offset
				return nil, nil
			},
		}
		h := newTestHandlers(eng)

		runTool[[]query.MessageSummary](t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": "acme.com"})
		if capturedLimit != 100 {
			t.Fatalf("expected default limit 100, got %d", capturedLimit)
		}
		if capturedOffset != 0 {
			t.Fatalf("expected default offset 0, got %d", capturedOffset)
		}
	})
}

// TestServeHTTPWithOptions_ContextCancellation verifies that the HTTP
// transport honours ctx cancellation by gracefully shutting the server
// down and returning ctx.Err(). Regression-guards a roborev #299
// finding where Start was called without ever consulting ctx.
func TestServeHTTPWithOptions_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Bind on :0 so we don't conflict with anything on the host.
	opts := ServeOptions{
		Engine:         &querytest.MockEngine{},
		AttachmentsDir: t.TempDir(),
		DataDir:        t.TempDir(),
	}

	done := make(chan error, 1)
	go func() {
		done <- ServeHTTPWithOptions(ctx, opts, "127.0.0.1:0")
	}()

	// Give the goroutine a moment to start the listener.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ServeHTTPWithOptions did not return after context cancellation")
	}
}
