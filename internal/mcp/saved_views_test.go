package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
)

type savedViewServiceFixture struct {
	views     []store.SavedView
	page      *savedview.RunPage
	runID     int64
	runLimit  int
	runCursor string
	err       error
}

func savedViewTestMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	require.True(t, ok, "map value: %#v", value)
	return result
}

func savedViewTestSlice(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	require.True(t, ok, "slice value: %#v", value)
	return result
}

func savedViewToolErrorText(t *testing.T, result map[string]any) string {
	t.Helper()
	content := savedViewTestSlice(t, result["content"])
	require.NotEmpty(t, content)
	block := savedViewTestMap(t, content[0])
	text, ok := block["text"].(string)
	require.True(t, ok, "error text: %#v", block["text"])
	return text
}

func (f *savedViewServiceFixture) ListSavedViews(context.Context) ([]store.SavedView, error) {
	return append([]store.SavedView(nil), f.views...), f.err
}

func (f *savedViewServiceFixture) GetSavedView(_ context.Context, id int64) (*store.SavedView, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.views {
		if f.views[i].ID == id {
			view := f.views[i]
			return &view, nil
		}
	}
	return nil, store.ErrSavedViewNotFound
}

func (f *savedViewServiceFixture) RunSavedView(
	_ context.Context, id int64, limit int, cursor string,
) (*savedview.RunPage, error) {
	f.runID, f.runLimit, f.runCursor = id, limit, cursor
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

func (f *savedViewServiceFixture) CreateSavedView(context.Context, store.SavedViewInput) (*store.SavedView, error) {
	return nil, errors.ErrUnsupported
}

func (f *savedViewServiceFixture) UpdateSavedView(
	context.Context, int64, int64, savedview.Patch,
) (*store.SavedView, error) {
	return nil, errors.ErrUnsupported
}

func (f *savedViewServiceFixture) DeleteSavedView(context.Context, int64, int64) error {
	return errors.ErrUnsupported
}

func TestSavedViewOutputSchemasPublishCanonicalStateAsSchemaObject(t *testing.T) {
	assertions := assert.New(t)
	const canonicalStateDescription = "Stored definition, including incompatible values; check incompatibility_reason before execution"
	view := store.SavedView{
		ID: 7, Name: "Invoices", CanonicalState: json.RawMessage(`{"query":"invoice"}`),
		SchemaVersion: 1, Revision: 1,
	}
	opts := ServeOptions{
		Engine:     &querytest.MockEngine{},
		SavedViews: &savedViewServiceFixture{views: []store.SavedView{view}},
	}
	tools := toolsByName(t, rawListTools(t, opts, true))
	cases := []struct {
		name string
		tool string
		path []string
	}{
		{name: "create", tool: ToolCreateSavedView, path: []string{"properties", "canonical_state"}},
		{name: "get", tool: ToolGetSavedView, path: []string{"properties", "canonical_state"}},
		{name: "update", tool: ToolUpdateSavedView, path: []string{"properties", "canonical_state"}},
		{name: "list", tool: ToolListSavedViews, path: []string{"properties", "saved_views", "items", "properties", "canonical_state"}},
		{name: "run", tool: ToolRunSavedView, path: []string{"properties", "saved_view", "properties", "canonical_state"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			value := tools[tc.tool]["outputSchema"]
			for _, segment := range tc.path {
				schema, ok := value.(map[string]any)
				requirements.True(ok, "%s schema node: %#v", segment, value)
				value, ok = schema[segment]
				requirements.True(ok, "%s schema node in %#v", segment, schema)
			}
			schema, ok := value.(map[string]any)
			requirements.True(ok, "canonical_state schema: %#v", value)
			assertions.Equal(map[string]any{"description": canonicalStateDescription}, schema)
		})
	}
}

func TestSavedViewReadToolsUseTypedDefinitions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 8, 18, 9, 30, 0, 0, time.UTC)
	description := "Invoices from the primary archive"
	view := store.SavedView{
		ID: 17, Name: "Invoices", Description: &description,
		CanonicalState: json.RawMessage(`{
			"query":"invoice","search_mode":"full_text",
			"filters":[{"field":"source","operator":"in","values":["3"]}],
			"grouping":["domain"],"presentation":"table",
			"sort":[{"field":"occurred_at","direction":"desc"}],
			"columns":["kind","title","time"]
		}`),
		SchemaVersion: 1, Revision: 4, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}
	reader := &savedViewServiceFixture{views: []store.SavedView{view}}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: reader}

	listed := rawCallTool(t, opts, ToolListSavedViews, map[string]any{})
	requirements.NotEqual(true, listed["isError"], "result: %#v", listed)
	listContent := savedViewTestMap(t, listed["structuredContent"])
	views := savedViewTestSlice(t, listContent["saved_views"])
	requirements.Len(views, 1)
	listedView := savedViewTestMap(t, views[0])
	assertions.Equal("Invoices", listedView["name"])
	assertions.Equal("invoice", savedViewTestMap(t, listedView["canonical_state"])["query"])

	got := rawCallTool(t, opts, ToolGetSavedView, map[string]any{"id": 17})
	requirements.NotEqual(true, got["isError"], "result: %#v", got)
	definition := savedViewTestMap(t, got["structuredContent"])
	assertions.InDelta(float64(17), definition["id"], 0)
	assertions.InDelta(float64(4), definition["revision"], 0)
	assertions.Equal([]any{"domain"}, savedViewTestMap(t, definition["canonical_state"])["grouping"])
}

func TestSavedViewReadToolsReturnNonObjectCanonicalStateVerbatim(t *testing.T) {
	legacyState := []any{"legacy", map[string]any{"query": "invoice"}}
	reader := &savedViewServiceFixture{views: []store.SavedView{{
		ID: 17, Name: "Legacy", CanonicalState: json.RawMessage(`["legacy",{"query":"invoice"}]`),
		SchemaVersion: 0, Revision: 1,
	}}}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: reader}

	for _, tool := range []string{ToolGetSavedView, ToolListSavedViews} {
		t.Run(tool, func(t *testing.T) {
			arguments := map[string]any{}
			if tool == ToolGetSavedView {
				arguments["id"] = 17
			}
			result := rawCallTool(t, opts, tool, arguments)
			require.NotEqual(t, true, result["isError"], "result: %#v", result)
			definition := savedViewTestMap(t, result["structuredContent"])
			if tool == ToolListSavedViews {
				views := savedViewTestSlice(t, definition["saved_views"])
				require.Len(t, views, 1)
				definition = savedViewTestMap(t, views[0])
			}
			assert.Equal(t, legacyState, definition["canonical_state"])
		})
	}
}

func TestRunSavedViewReturnsTypedExplorePage(t *testing.T) {
	assertions := assert.New(t)
	view := store.SavedView{
		ID: 7, Name: "Project", CanonicalState: json.RawMessage(`{"presentation":"table"}`),
		SchemaVersion: 1, Revision: 1,
	}
	total := int64(9)
	reader := &savedViewServiceFixture{page: &savedview.RunPage{
		View: view, ResultKind: savedview.ResultEntries,
		Rows:       []query.EntryRow{{Key: "message:42", Kind: query.EntryEmail, Title: "Project update"}},
		TotalCount: &total, NextCursor: "next-page",
		CacheRevision: "cache-7",
	}}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: reader}

	result := rawCallTool(t, opts, ToolRunSavedView, map[string]any{
		"id": 7, "limit": 5000, "cursor": "current-page",
	})
	require.NotEqual(t, true, result["isError"], "result: %#v", result)
	structured := savedViewTestMap(t, result["structuredContent"])
	assertions.Equal("entries", structured["result_kind"])
	assertions.InDelta(float64(1), structured["returned"], 0)
	assertions.Equal(true, structured["has_more"])
	assertions.Equal("next-page", structured["next_cursor"])
	assertions.Len(structured["rows"], 1)
	assertions.Equal(int64(7), reader.runID)
	assertions.Equal(maxSearchMessagesLimit, reader.runLimit)
	assertions.Equal("current-page", reader.runCursor)
}

func TestSavedViewReadToolsReturnCleanErrors(t *testing.T) {
	assertions := assert.New(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{
		err: store.ErrSavedViewNotFound,
	}}

	missing := rawCallTool(t, opts, ToolGetSavedView, map[string]any{"id": 999})
	assertions.Equal(true, missing["isError"])
	assertions.Contains(savedViewToolErrorText(t, missing), "saved_view_not_found")

	invalid := opts
	invalid.SavedViews = &savedViewServiceFixture{err: store.ErrSavedViewInvalidState}
	failed := rawCallTool(t, invalid, ToolRunSavedView, map[string]any{"id": 7})
	assertions.Equal(true, failed["isError"])
	assertions.Contains(savedViewToolErrorText(t, failed), "invalid_saved_view")
}

func TestSavedViewWriteToolsRejectInvalidInputBeforeCallingTheService(t *testing.T) {
	assertions := assert.New(t)
	service := &savedViewServiceFixture{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: service}

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{
			name: "transient workspace state", tool: ToolCreateSavedView,
			args: map[string]any{"name": "Invalid", "schema_version": 1, "canonical_state": map[string]any{"selection": []any{1}}},
			want: "selection",
		},
		{
			// An explicit null is not an absent field: the store rejects it,
			// and the input schema rejects it before the handler runs.
			name: "explicit null", tool: ToolCreateSavedView,
			args: map[string]any{"name": "Null filters", "schema_version": 1, "canonical_state": map[string]any{"filters": nil}},
			want: `"null"`,
		},
		{
			name: "missing name", tool: ToolCreateSavedView,
			args: map[string]any{"schema_version": 1, "canonical_state": map[string]any{}},
			want: `"name"`,
		},
		{
			name: "empty patch", tool: ToolUpdateSavedView,
			args: map[string]any{"id": 7, "revision": 1},
			want: "at least one mutable",
		},
		{
			name: "non-object canonical state", tool: ToolUpdateSavedView,
			args: map[string]any{"id": 7, "revision": 1, "canonical_state": "table"},
			want: "object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := rawCallTool(t, opts, tc.tool, tc.args)
			assertions.Equal(true, result["isError"], "result: %#v", result)
			assertions.Contains(savedViewToolErrorText(t, result), tc.want)
		})
	}
}

func TestRunSavedViewMapsDaemonRunFailuresToActionableToolErrors(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{code: "archive_revision_changed", want: "run the Saved View again without a cursor"},
		{code: "search_revision_changed", want: "run the Saved View again without a cursor"},
		{code: "analytical_cache_unavailable", want: "retry after it finishes preparing"},
		{code: "invalid_cursor", want: "Saved View execution failed"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			assertions := assert.New(t)
			opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: &savedViewServiceFixture{
				err: codedPeopleError{code: tc.code},
			}}
			result := rawCallTool(t, opts, ToolRunSavedView, map[string]any{"id": 7})
			assertions.Equal(true, result["isError"], "result: %#v", result)
			text := savedViewToolErrorText(t, result)
			assertions.Contains(text, tc.code+": ")
			assertions.Contains(text, tc.want)
		})
	}
}

func TestSavedViewReadToolsPreserveDaemonMetadata(t *testing.T) {
	requirements := require.New(t)
	definition := json.RawMessage(`{
        "id":7,"name":"Future view","schema_version":2,"revision":1,
        "canonical_state":{"query":{"text":"invoice"},"inspector_pinned":false},
        "incompatibility_reason":"unsupported schema version 2",
        "created_at":"2026-08-18T08:00:00Z","updated_at":"2026-08-18T09:00:00Z"
    }`)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/saved-views" {
			_ = json.NewEncoder(w).Encode(map[string]any{"saved_views": []json.RawMessage{definition}})
			return
		}
		_ = json.NewEncoder(w).Encode(definition)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client(), Context: t.Context()})
	requirements.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, SavedViews: client}
	for _, tool := range []string{ToolListSavedViews, ToolGetSavedView} {
		t.Run(tool, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			args := map[string]any{}
			if tool == ToolGetSavedView {
				args["id"] = 7
			}
			result := rawCallTool(t, opts, tool, args)
			requirements.NotEqual(true, result["isError"], "%#v", result)
			definition := savedViewTestMap(t, result["structuredContent"])
			if tool == ToolListSavedViews {
				views := savedViewTestSlice(t, definition["saved_views"])
				requirements.Len(views, 1)
				definition = savedViewTestMap(t, views[0])
			}
			assertions.Equal("unsupported schema version 2", definition["incompatibility_reason"])
			state := savedViewTestMap(t, definition["canonical_state"])
			assertions.Equal(false, state["inspector_pinned"])
			assertions.Equal(map[string]any{"text": "invoice"}, state["query"])
		})
	}
}
