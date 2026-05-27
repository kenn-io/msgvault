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
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	requirepkg.NoError(t, err, "handler returned error")
	return result
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	requirepkg.NotEmpty(t, r.Content, "empty content")
	tc, ok := r.Content[0].(mcp.TextContent)
	requirepkg.True(t, ok, "expected TextContent, got %T", r.Content[0])
	return tc.Text
}

// runTool invokes a handler, asserts no error, and unmarshals the JSON result into T.
func runTool[T any](t *testing.T, name string, fn toolHandler, args map[string]any) T {
	t.Helper()
	r := callToolDirect(t, name, fn, args)
	requirepkg.False(t, r.IsError, "unexpected error: %s", resultText(t, r))
	var out T
	requirepkg.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &out), "unmarshal failed")
	return out
}

// runToolExpectError invokes a handler and asserts it returns an error result.
func runToolExpectError(t *testing.T, name string, fn toolHandler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	r := callToolDirect(t, name, fn, args)
	requirepkg.True(t, r.IsError, "expected error result")
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
		requirepkg.Len(t, msgs, 1, "msgs")
		assertpkg.Equal(t, "Hello", msgs[0].Subject, "subject")
		assertpkg.Equal(t, "thread-abc", msgs[0].SourceConversationID, "SourceConversationID")
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
	requirepkg.Len(t, msgs, 1, "FTS fallback msgs")
	assertpkg.Equal(t, int64(2), msgs[0].ID, "FTS fallback ID")
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
	assertpkg.Contains(t, txt, "vector_not_enabled", "expected 'vector_not_enabled' error, got: %s")
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
	assertpkg.Contains(t, txt, "index_building", "expected 'index_building' error, got: %s")
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
	assertpkg.Contains(t, txt, "vector_not_enabled", "expected 'vector_not_enabled' error, got: %s")
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
	assertpkg.Contains(t, txt, "missing_free_text", "expected 'missing_free_text' error, got: %s")
}

// TestSearchMessages_HybridPoolSaturatedAlwaysEmitted regression-guards
// the wire-level contract that pool_saturated is always present (and
// false on a successful, under-cap response). An `omitempty` slip
// would silently drop the field when false; clients that key off
// "saturated vs not" would break.
func TestSearchMessages_HybridPoolSaturatedAlwaysEmitted(t *testing.T) {
	require := requirepkg.New(t)
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
	require.False(r.IsError, "unexpected error: %s", resultText(t, r))
	var raw map[string]json.RawMessage
	require.NoError(json.Unmarshal([]byte(resultText(t, r)), &raw), "unmarshal")
	val, exists := raw["pool_saturated"]
	require.True(exists, "pool_saturated key missing from successful response (raw=%s)", resultText(t, r))
	assertpkg.Equal(t, "false", string(val), "pool_saturated")
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
	assertpkg.Contains(t, txt, "pagination_unsupported", "expected 'pagination_unsupported' error, got: %s")
}

func TestSearchMessages_UnknownMode(t *testing.T) {
	h := newTestHandlers(&querytest.MockEngine{})

	r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
		"query": "meeting notes",
		"mode":  "bogus",
	})
	txt := resultText(t, r)
	assertpkg.Contains(t, txt, "invalid mode", "expected 'invalid mode' error, got: %s")
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
		assertpkg.Equal(t, "Test Message", msg.Subject, "subject")
		assertpkg.Equal(t, "thread-xyz", msg.SourceConversationID, "SourceConversationID")
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
	assert := assertpkg.New(t)
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

	assert.Equal(int64(1000), resp.Stats.MessageCount, "message count")
	assert.Len(resp.Accounts, 2, "accounts")
	assert.Nil(resp.VectorSearch, "expected VectorSearch to be nil when backend is disabled")

	// Also confirm the JSON payload omits the key entirely, so clients
	// that type-check the wire format see a clean absence rather than
	// a null value.
	r := callToolDirect(t, "get_stats", h.getStats, map[string]any{})
	var raw map[string]json.RawMessage
	requirepkg.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &raw), "unmarshal raw")
	assert.NotContains(raw, "vector_search", "expected 'vector_search' to be absent from JSON when backend is nil")
}

func TestGetStats_VectorEnabled(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	require.NotNil(resp.VectorSearch, "expected vector_search sub-object")
	assert.True(resp.VectorSearch.Enabled, "vector_search.enabled")
	require.NotNil(resp.VectorSearch.ActiveGeneration, "expected vector_search.active_generation to be populated")
	ag := resp.VectorSearch.ActiveGeneration
	assert.Equal(vector.GenerationID(5), ag.ID, "active_generation.id")
	assert.Equal("nomic-embed", ag.Model, "active_generation.model")
	assert.Equal(int64(100), ag.MessageCount, "active_generation.message_count")
	assert.Equal(int64(3), resp.VectorSearch.PendingEmbeddingsTotal, "pending_embeddings_total")
	assert.Nil(resp.VectorSearch.BuildingGeneration, "building_generation")
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
			assertpkg.Len(t, rows, 2, "rows")
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
		requirepkg.Len(t, msgs, 1, "msgs")
		assertpkg.Equal(t, "thread-list", msgs[0].SourceConversationID, "SourceConversationID")
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
	requirepkg.NoError(t, os.MkdirAll(hashDir, 0o755), "MkdirAll")
	requirepkg.NoError(t, os.WriteFile(filepath.Join(hashDir, hash), content, 0o644), "WriteFile")
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
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		r := callToolDirect(t, "get_attachment", h.getAttachment, map[string]any{"attachment_id": float64(10)})
		require.False(r.IsError, "unexpected error: %s", resultText(t, r))

		// Should have 2 content blocks: text metadata + embedded resource.
		require.Len(r.Content, 2, "content blocks")

		// First block: text with metadata JSON.
		tc, ok := r.Content[0].(mcp.TextContent)
		require.True(ok, "expected TextContent, got %T", r.Content[0])
		var meta attachmentMeta
		require.NoError(json.Unmarshal([]byte(tc.Text), &meta), "unmarshal metadata")
		assert.Equal("report.pdf", meta.Filename, "filename")
		assert.Equal("application/pdf", meta.MimeType, "mime_type")
		assert.Equal(int64(len(content)), meta.Size, "size")

		// Second block: embedded resource with blob.
		er, ok := r.Content[1].(mcp.EmbeddedResource)
		require.True(ok, "expected EmbeddedResource, got %T", r.Content[1])
		blob, ok := er.Resource.(mcp.BlobResourceContents)
		require.True(ok, "expected BlobResourceContents, got %T", er.Resource)
		assert.Equal("application/pdf", blob.MIMEType, "blob MIME type")
		decoded, err := base64.StdEncoding.DecodeString(blob.Blob)
		require.NoError(err, "base64 decode")
		assert.Equal(string(content), string(decoded), "content")
	})

	t.Run("empty mime type defaults to octet-stream", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
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
		require.False(r.IsError, "unexpected error: %s", resultText(t, r))

		var meta attachmentMeta
		tc, ok := r.Content[0].(mcp.TextContent)
		require.True(ok, "Content[0] is TextContent, got %T", r.Content[0])
		require.NoError(json.Unmarshal([]byte(tc.Text), &meta), "unmarshal metadata")
		assert.Equal("application/octet-stream", meta.MimeType, "default mime_type")

		er, ok := r.Content[1].(mcp.EmbeddedResource)
		require.True(ok, "Content[1] is EmbeddedResource, got %T", r.Content[1])
		blob, ok := er.Resource.(mcp.BlobResourceContents)
		require.True(ok, "Resource is BlobResourceContents, got %T", er.Resource)
		assert.Equal("application/octet-stream", blob.MIMEType, "default blob MIME type")
	})

	t.Run("filename with spaces and unicode", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
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
		require.False(r.IsError, "unexpected error: %s", resultText(t, r))

		// Metadata JSON must be valid and preserve the filename exactly.
		var meta attachmentMeta
		tc, ok := r.Content[0].(mcp.TextContent)
		require.True(ok, "Content[0] is TextContent, got %T", r.Content[0])
		require.NoError(json.Unmarshal([]byte(tc.Text), &meta), "metadata is not valid JSON")
		assert.Equal("report 2024✓.pdf", meta.Filename, "filename")

		// URI must percent-encode spaces and non-ASCII characters.
		er, ok := r.Content[1].(mcp.EmbeddedResource)
		require.True(ok, "Content[1] is EmbeddedResource, got %T", r.Content[1])
		blob, ok := er.Resource.(mcp.BlobResourceContents)
		require.True(ok, "Resource is BlobResourceContents, got %T", er.Resource)
		const wantURI = "attachment:///51/report%202024%E2%9C%93.pdf"
		assert.Equal(wantURI, blob.URI, "URI")
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
				assertpkg.Contains(t, resultText(t, r), tt.errContains, "error message")
			}
		})
	}

	t.Run("oversized attachment", func(t *testing.T) {
		bigHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		createAttachmentFixture(t, tmpDir, bigHash, nil)
		bigPath := filepath.Join(tmpDir, bigHash[:2], bigHash)
		requirepkg.NoError(t, os.Truncate(bigPath, maxAttachmentSize+1), "Truncate")

		h2 := &handlers{
			engine: &querytest.MockEngine{
				Attachments: map[int64]*query.AttachmentInfo{
					40: {ID: 40, Filename: "huge.bin", MimeType: "application/octet-stream", Size: maxAttachmentSize + 1, ContentHash: bigHash},
				},
			},
			attachmentsDir: tmpDir,
		}
		r := runToolExpectError(t, "get_attachment", h2.getAttachment, map[string]any{"attachment_id": float64(40)})
		assertpkg.Contains(t, resultText(t, r), "too large", "error message")
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
		assert := assertpkg.New(t)
		destDir := t.TempDir()
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		assert.Equal("report.pdf", resp.Filename, "filename")
		assert.Equal(int64(len(content)), resp.Size, "size")
		wantPath := filepath.Join(destDir, "report.pdf")
		assert.Equal(wantPath, resp.Path, "path")
		got, err := os.ReadFile(wantPath)
		requirepkg.NoError(t, err, "ReadFile")
		assert.Equal(string(content), string(got), "content")
	})

	t.Run("filename collision appends suffix", func(t *testing.T) {
		destDir := t.TempDir()
		// Create existing file to force collision.
		requirepkg.NoError(t, os.WriteFile(filepath.Join(destDir, "report.pdf"), []byte("old"), 0644), "WriteFile")
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		assertpkg.Equal(t, "report_1.pdf", resp.Filename, "filename")
		// Original file should be untouched.
		old, _ := os.ReadFile(filepath.Join(destDir, "report.pdf"))
		assertpkg.Equal(t, "old", string(old), "original file should not be overwritten")
	})

	t.Run("directory collision appends suffix", func(t *testing.T) {
		destDir := t.TempDir()
		// Create a directory with the same name as the attachment.
		requirepkg.NoError(t, os.Mkdir(filepath.Join(destDir, "report.pdf"), 0755), "Mkdir")
		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
			"destination":   destDir,
		})
		assertpkg.Equal(t, "report_1.pdf", resp.Filename, "filename")
	})

	t.Run("default destination is ~/Downloads", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		downloads := filepath.Join(home, "Downloads")
		requirepkg.NoError(t, os.Mkdir(downloads, 0755), "Mkdir Downloads")

		resp := runTool[exportResponse](t, "export_attachment", h.exportAttachment, map[string]any{
			"attachment_id": float64(10),
		})
		assertpkg.True(t, strings.HasPrefix(resp.Path, downloads), "expected path under ~/Downloads, got %s", resp.Path)
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
			assertpkg.Equal(t, tc.want, got, "SanitizeFilename(%q)", tc.input)
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
			assertpkg.Equal(t, tc.wantFilename, resp.Filename, "filename")
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
	assertpkg.Contains(t, txt, "too large", "expected 'too large' rejection from metadata check, got: %s")
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
	assertpkg.Contains(t, txt, "too large", "expected 'too large' rejection from metadata check, got: %s")
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
			assertpkg.Equal(t, tt.want, got, "limitArg(%v)", tt.val)
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
		assertpkg.Len(t, msgs, 1, "msgs")
	})

	t.Run("search with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "search_messages", h.searchMessages, map[string]any{
			"query":   "test",
			"account": "unknown@gmail.com",
		})
		txt := resultText(t, r)
		assertpkg.Contains(t, txt, "account not found", "expected 'account not found' error, got: %s")
	})

	t.Run("list with valid account", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "list_messages", h.listMessages, map[string]any{
			"account": "bob@gmail.com",
		})
		assertpkg.Len(t, msgs, 1, "msgs")
	})

	t.Run("list with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "list_messages", h.listMessages, map[string]any{
			"account": "unknown@gmail.com",
		})
		txt := resultText(t, r)
		assertpkg.Contains(t, txt, "account not found", "expected 'account not found' error, got: %s")
	})

	t.Run("aggregate with valid account", func(t *testing.T) {
		rows := runTool[[]query.AggregateRow](t, "aggregate", h.aggregate, map[string]any{
			"group_by": "sender",
			"account":  "alice@gmail.com",
		})
		assertpkg.Len(t, rows, 1, "rows")
	})

	t.Run("aggregate with invalid account", func(t *testing.T) {
		r := runToolExpectError(t, "aggregate", h.aggregate, map[string]any{
			"group_by": "sender",
			"account":  "unknown@gmail.com",
		})
		txt := resultText(t, r)
		assertpkg.Contains(t, txt, "account not found", "expected 'account not found' error, got: %s")
	})

	t.Run("empty account means no filter", func(t *testing.T) {
		// Empty string should not filter - return all results
		msgs := runTool[[]query.MessageSummary](t, "search_messages", h.searchMessages, map[string]any{
			"query":   "test",
			"account": "",
		})
		assertpkg.Len(t, msgs, 1, "msgs")
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
		assertpkg.Equal(t, 2, resp.MessageCount, "MessageCount")
		assertpkg.Equal(t, "pending", resp.Status, "Status")
		assertpkg.NotEmpty(t, resp.BatchID, "BatchID")
	})

	t.Run("structured filter staging", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		resp := runTool[stageDeletionResponse](
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"from": "news@example.com"},
		)
		assertpkg.Equal(t, 2, resp.MessageCount, "MessageCount")
	})

	t.Run("whitespace-only query rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{"query": "   "},
		)
		txt := resultText(t, r)
		assertpkg.Contains(t, txt, "must provide", "expected validation error, got: %s")
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
		assertpkg.Contains(t, txt, "not both", "expected mutual exclusion error, got: %s")
	})

	t.Run("no filters rejected", func(t *testing.T) {
		dataDir := t.TempDir()
		h := &handlers{engine: eng, dataDir: dataDir}

		r := runToolExpectError(
			t, "stage_deletion", h.stageDeletion,
			map[string]any{},
		)
		txt := resultText(t, r)
		assertpkg.Contains(t, txt, "must provide", "expected validation error, got: %s")
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
		assertpkg.Contains(t, txt, "no messages match", "expected no-match error, got: %s")
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
		requirepkg.NotNil(t, capturedFilter.SourceID, "expected SourceID to be set")
		assertpkg.Equal(t, int64(1), *capturedFilter.SourceID, "SourceID")
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
		assertpkg.Contains(t, txt, "account not found", "expected account error, got: %s")
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
		assertpkg.Equal(t, maxStageDeletionResults, capturedFilter.Pagination.Limit, "limit")
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
func (f *fakeBackend) CreateGeneration(_ context.Context, _ string, _ int, _ string) (vector.GenerationID, error) {
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
	assertpkg.Contains(t, txt, "vector_not_enabled", "expected 'vector_not_enabled' error, got: %s")
}

// TestSearchMessagesTool_AdvertisesVectorModesOnlyWhenAvailable guards the
// capability-discovery contract: when the server has no hybrid engine,
// the search_messages tool omits the "mode" and "explain" parameters so
// clients don't build vector requests that will fail at runtime.
func TestSearchMessagesTool_AdvertisesVectorModesOnlyWhenAvailable(t *testing.T) {
	assert := assertpkg.New(t)
	disabled := searchMessagesTool(false)
	assert.NotContains(disabled.InputSchema.Properties, "mode", "vectorAvailable=false: tool advertises 'mode' but vector modes are unsupported")
	assert.NotContains(disabled.InputSchema.Properties, "explain", "vectorAvailable=false: tool advertises 'explain' but vector modes are unsupported")
	assert.False(strings.Contains(disabled.Description, "mode=vector") || strings.Contains(disabled.Description, "mode=hybrid"),
		"vectorAvailable=false: tool description mentions vector modes: %q", disabled.Description)

	enabled := searchMessagesTool(true)
	assert.Contains(enabled.InputSchema.Properties, "mode", "vectorAvailable=true: tool is missing 'mode' parameter")
	assert.Contains(enabled.InputSchema.Properties, "explain", "vectorAvailable=true: tool is missing 'explain' parameter")
	assert.Contains(enabled.Description, "free-text", "vectorAvailable=true: tool description should call out the free-text requirement, got: %q", enabled.Description)
}

func TestFindSimilarMessages_MissingID(t *testing.T) {
	h := &handlers{
		engine:  &querytest.MockEngine{},
		backend: &fakeBackend{},
	}

	r := runToolExpectError(t, "find_similar_messages", h.findSimilarMessages, map[string]any{})
	txt := resultText(t, r)
	assertpkg.Contains(t, txt, "message_id", "expected error mentioning 'message_id', got: %s")
}

func TestFindSimilarMessages_HappyPath(t *testing.T) {
	assert := assertpkg.New(t)
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

	assert.Equal(int64(100), resp.SeedMessageID, "seed_message_id")
	assert.Equal(2, resp.Returned, "returned")
	assert.Equal(int64(7), resp.Generation.ID, "generation.id")
	assert.Equal("nomic-embed:4", resp.Generation.Fingerprint, "generation.fingerprint")
	requirepkg.Len(t, resp.Messages, 2, "messages")
	for _, m := range resp.Messages {
		assert.NotEqual(int64(100), m.ID, "seed message 100 must not appear in results")
	}
	assert.Equal(int64(200), resp.Messages[0].ID, "Messages[0].ID")
	assert.Equal(int64(300), resp.Messages[1].ID, "Messages[1].ID")
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
	assertpkg.Contains(t, txt, "no_active_generation", "expected 'no_active_generation' error, got: %s")
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
		assertpkg.Len(t, msgs, 2, "msgs")
	})

	t.Run("domains with whitespace", func(t *testing.T) {
		msgs := runTool[[]query.MessageSummary](t, "search_by_domains", h.searchByDomains,
			map[string]any{"domains": " acme.com , example.com "})
		assertpkg.Len(t, msgs, 2, "msgs")
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
		assert := assertpkg.New(t)
		var capturedDomains []string
		var capturedLimit, capturedOffset int
		eng := &querytest.MockEngine{
			SearchByDomainsFunc: func(_ context.Context, domains []string, after, before *time.Time, limit, offset int) ([]query.MessageSummary, error) {
				capturedDomains = domains
				capturedLimit = limit
				capturedOffset = offset
				assert.NotNil(after, "expected after to be set")
				assert.NotNil(before, "expected before to be set")
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
		assert.Len(msgs, 1, "msgs")
		assert.Equal([]string{"acme.com", "globex.com"}, capturedDomains, "domains")
		assert.Equal(50, capturedLimit, "limit")
		assert.Equal(10, capturedOffset, "offset")
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
		assertpkg.Equal(t, 100, capturedLimit, "default limit")
		assertpkg.Equal(t, 0, capturedOffset, "default offset")
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
		requirepkg.ErrorIs(t, err, context.Canceled, "expected context.Canceled")
	case <-time.After(15 * time.Second):
		requirepkg.Fail(t, "ServeHTTPWithOptions did not return after context cancellation")
	}
}
