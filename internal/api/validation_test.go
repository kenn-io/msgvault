package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

// doGet serves a GET request against the router and returns the recorder.
func doGet(srv *Server, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

// decodeErrorEnvelope decodes a response body as the standard error envelope and
// asserts it carries error+message and no huma $schema field.
func decodeErrorEnvelope(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json",
		"error responses must be JSON")

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw),
		"error body must be JSON object: %s", w.Body.String())
	_, hasSchema := raw["$schema"]
	assert.False(t, hasSchema, "error envelope must not carry $schema: %s", w.Body.String())

	var env ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "decode error envelope")
	assert.NotEmpty(t, env.Error, "error code present")
	assert.NotEmpty(t, env.Message, "error message present")
	return env
}

func TestInvalidQueryParamsReturn400(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		wantCode  string
		wantParam string // substring expected in the message
	}{
		{"aggregate after not a date", "/api/v1/aggregates?view_type=senders&after=not-a-date", "invalid_after", "after"},
		{"aggregate before not a date", "/api/v1/aggregates?view_type=senders&before=nope", "invalid_before", "before"},
		{"aggregate time_granularity bogus", "/api/v1/aggregates?view_type=time&time_granularity=bogus", "invalid_time_granularity", "time_granularity"},
		{"aggregate sort bogus", "/api/v1/aggregates?view_type=senders&sort=bogus", "invalid_sort", "sort"},
		{"aggregate direction bogus", "/api/v1/aggregates?view_type=senders&direction=sideways", "invalid_direction", "direction"},
		{"aggregate limit not numeric", "/api/v1/aggregates?view_type=senders&limit=xyz", "invalid_limit", "limit"},
		{"aggregate source_id not numeric", "/api/v1/aggregates?view_type=senders&source_id=abc", "invalid_source_id", "source_id"},
		{"stats group_by bogus", "/api/v1/stats/total?group_by=bogus", "invalid_group_by", "group_by"},
		{"stats source_id not numeric", "/api/v1/stats/total?source_id=abc", "invalid_source_id", "source_id"},
		{"stats source_ids not numeric", "/api/v1/stats/total?source_ids=1&source_ids=abc", "invalid_source_ids", "source_ids"},
		{"stats attachments_only not boolean", "/api/v1/stats/total?attachments_only=perhaps", "invalid_attachments_only", "attachments_only"},
		{"stats hide_deleted not boolean", "/api/v1/stats/total?hide_deleted=perhaps", "invalid_hide_deleted", "hide_deleted"},
		{"stats search_scope not boolean", "/api/v1/stats/total?search_scope=perhaps", "invalid_search_scope", "search_scope"},
		{"filter conversation_id not numeric", "/api/v1/messages/filter?conversation_id=abc", "invalid_conversation_id", "conversation_id"},
		{"filter sort bogus", "/api/v1/messages/filter?sort=bogus", "invalid_sort", "sort"},
		{"filter direction bogus", "/api/v1/messages/filter?direction=sideways", "invalid_direction", "direction"},
		{"filter empty_targets bogus", "/api/v1/messages/filter?empty_targets=bogusview", "invalid_empty_targets", "empty_targets"},
		{"filter after not a date", "/api/v1/messages/filter?after=not-a-date", "invalid_after", "after"},
		{"messages page not numeric", "/api/v1/messages?page=abc", "invalid_page", "page"},
		{"messages page_size not numeric", "/api/v1/messages?page_size=xyz", "invalid_page_size", "page_size"},
		{"search page not numeric", "/api/v1/search?q=hi&page=abc", "invalid_page", "page"},
		{"search page_size not numeric", "/api/v1/search?q=hi&page_size=xyz", "invalid_page_size", "page_size"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWithEngine(t, &querytest.MockEngine{})
			w := doGet(srv, tc.target)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			env := decodeErrorEnvelope(t, w)
			assert.Equal(t, tc.wantCode, env.Error, "error code")
			assert.Contains(t, env.Message, tc.wantParam, "message names the parameter")
		})
	}
}

func TestValidQueryParamsAccepted(t *testing.T) {
	targets := []string{
		"/api/v1/aggregates?view_type=senders&after=2024-01-01&before=2024-02-01&sort=count&direction=asc&limit=5&source_id=1",
		"/api/v1/aggregates?view_type=time&time_granularity=day",
		"/api/v1/stats/total?group_by=domains&source_ids=1&source_ids=2&hide_deleted=true",
		"/api/v1/messages/filter?conversation_id=5&sort=date&direction=desc&empty_targets=labels",
		"/api/v1/messages?page=2&page_size=10",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			srv := newTestServerWithEngine(t, &querytest.MockEngine{})
			w := doGet(srv, target)
			assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		})
	}
}

func TestOutOfRangePaginationClamps(t *testing.T) {
	tests := []struct {
		name         string
		target       string
		wantPage     int
		wantPageSize int
	}{
		{"page_size too big clamps to max", "/api/v1/messages?page=1&page_size=100000", 1, 100},
		{"page_size zero falls back to default", "/api/v1/messages?page=1&page_size=0", 1, 20},
		{"negative page clamps to 1", "/api/v1/messages?page=-5&page_size=10", 1, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv := newTestServerWithEngine(t, &querytest.MockEngine{})
			w := doGet(srv, tc.target)
			require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
			var resp MessageListResponse
			require.NoError(json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(tc.wantPage, resp.Page, "page clamp")
			assert.Equal(tc.wantPageSize, resp.PageSize, "page_size clamp")
		})
	}
}

func TestUnknownPathReturnsJSON404(t *testing.T) {
	targets := []string{
		"/api/v1/nonexistent",
		"/api/v1/messages/",
		"/api/v1/stats/",
		"/totally/unknown",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			srv := newTestServerWithEngine(t, &querytest.MockEngine{})
			w := doGet(srv, target)
			require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
			env := decodeErrorEnvelope(t, w)
			assert.Equal(t, "not_found", env.Error)
		})
	}
}

func TestTypedRouteErrorEnvelopeHasNoSchema(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	// cli/identities is a typed huma.Register route; an unknown account yields a
	// 400 whose body must match the bare {error,message} envelope, not the
	// $schema-wrapped huma variant.
	w := doGet(srv, "/api/v1/cli/identities?account=nosuch@example.com")
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	decodeErrorEnvelope(t, w)
}

func TestQueryEndpointRejectsNonReadOnly(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		allowed bool
	}{
		{"select allowed", "SELECT 1", true},
		{"delete rejected", "DELETE FROM messages", false},
		{"multi statement rejected", "SELECT 1; DROP TABLE messages", false},
		{"cte delete rejected", "WITH t AS (SELECT id FROM messages) DELETE FROM messages", false},
		{"semicolon in string allowed", "SELECT ';DROP'", true},
	}

	ranSQL := false
	runner := func(_ context.Context, _ string) (*query.QueryResult, error) {
		ranSQL = true
		return &query.QueryResult{Columns: []string{"n"}, Rows: [][]any{{int64(1)}}, RowCount: 1}, nil
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		SQLQueryRunner: runner,
		Logger:         testLogger(),
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ranSQL = false
			body := strings.NewReader(`{"sql":` + mustJSONString(tc.sql) + `}`)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/query", body)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			if tc.allowed {
				assert.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
				assert.True(ranSQL, "runner should execute an allowed query")
				return
			}
			require.Equal(http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			env := decodeErrorEnvelope(t, w)
			assert.Equal("not_read_only", env.Error)
			assert.False(ranSQL, "runner must not execute a rejected query")
		})
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
