//go:build cgo

package mcp

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func seedMetadataSearch(t *testing.T, db *sql.DB) {
	t.Helper()
	schema, err := os.ReadFile("../store/schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(schema))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sources (id,source_type,identifier) VALUES (1,'gmail','reader@example.test');
 INSERT INTO conversations (id,source_id,source_conversation_id,conversation_type) VALUES (1,1,'thread','email_thread');
 INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,sent_at,subject,snippet) VALUES
 (1,1,1,'one','email','2024-01-01','first note','preview'),
 (2,1,1,'two','email','2024-01-02','second note','preview'),
 (3,1,1,'three','email','2024-01-03','third note','preview');`)
	require.NoError(t, err)
}

func metadataDaemon(t *testing.T, engine query.Engine, requests *atomic.Int32) *daemonclient.Engine {
	t.Helper()
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}},
		Engine: engine, Logger: slog.New(slog.DiscardHandler),
	})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/search/fast" {
			requests.Add(1)
		}
		server.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	adapter, err := daemonclient.NewEngine(daemonclient.Config{URL: daemon.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, adapter.Close()) })
	return adapter
}

func TestSearchMetadataSingleRequestSQLiteStatements(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		offset      int
		ids         []int64
		total       int64
		statements  int
	}{
		{"first page", "after:2023-01-01", 0, []int64{3, 2}, 3, 3},
		{"last page", "after:2023-01-01", 2, []int64{1}, 3, 3},
		{"past end", "after:2023-01-01", 9, nil, 3, 2},
		{"empty", "after:2099-01-01", 0, nil, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			db, observer := sqlobserve.Open(t, nil)
			seedMetadataSearch(t, db)
			var requests atomic.Int32
			adapter := metadataDaemon(t, query.NewSQLiteEngine(db), &requests)
			observer.Reset()
			result := runTool[paginatedSearchMessages](t, ToolSearchMetadata, newTestHandlers(adapter).searchMetadata, map[string]any{"query": tc.query, "limit": float64(2), "offset": float64(tc.offset)})
			ids := make([]int64, 0, len(result.Data))
			for _, message := range result.Data {
				ids = append(ids, message.ID)
			}
			assert.Equal(tc.ids, append([]int64(nil), ids...))
			assert.Equal(tc.total, result.Total)
			assert.Equal(int32(1), requests.Load())
			assert.Len(observer.Statements(), tc.statements)
		})
	}
}

func TestRemoteSearchFastZeroLimitKeepsDefaultPage(t *testing.T) {
	assert := assert.New(t)
	db, observer := sqlobserve.Open(t, nil)
	seedMetadataSearch(t, db)
	var requests atomic.Int32
	adapter := metadataDaemon(t, query.NewSQLiteEngine(db), &requests)
	observer.Reset()
	messages, err := adapter.SearchFast(t.Context(), search.Parse("after:2023-01-01"), query.MessageFilter{}, 0, 0)
	require.NoError(t, err)
	assert.Len(messages, 3, "SearchFast keeps its default page; only the combined API uses zero for count-only")
	assert.Equal(int32(1), requests.Load())
	assert.Len(observer.Statements(), 3)
}

// TestSearchByDomainsPagesThroughDaemon pages a real archive across offset
// 1000 through MCP, the daemon adapter, and the HTTP handler.
func TestSearchByDomainsPagesThroughDaemon(t *testing.T) {
	const total = 1501
	db, _ := sqlobserve.Open(t, nil)
	seedMetadataSearch(t, db)
	_, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 4 UNION ALL SELECT i+1 FROM n WHERE i < ?)
 INSERT INTO messages (id,conversation_id,source_id,source_message_id,message_type,sent_at,subject,snippet)
 SELECT i,1,1,'m'||i,'email',datetime('2024-02-01', '+'||i||' seconds'),'note '||i,'preview' FROM n;
 INSERT INTO participants (id,email_address,domain) VALUES (1,'peer@example.com','example.com');
 INSERT INTO message_recipients (message_id,participant_id,recipient_type) SELECT id,1,'to' FROM messages;`, total)
	require.NoError(t, err)
	var requests atomic.Int32
	h := newTestHandlers(metadataDaemon(t, query.NewSQLiteEngine(db), &requests))

	type page struct {
		returned, offset int
		hasMore          bool
	}
	var pages []page
	var got []int64
	offset := 1000
	for range 3 {
		resp := runTool[searchByDomainsResponse](t, "search_by_domains", h.searchByDomains, map[string]any{
			"domains": "example.com",
			"limit":   float64(5000),
			"offset":  float64(offset),
		})
		pages = append(pages, page{resp.Returned, resp.Offset, resp.HasMore})
		for _, m := range resp.Data {
			got = append(got, m.ID)
		}
		if !resp.HasMore {
			break
		}
		offset += resp.Returned
	}

	assert.Equal(t, []page{{500, 1000, true}, {1, 1500, false}}, pages)
	want := make([]int64, total-1000)
	for i := range want {
		want[i] = int64(total - 1000 - i)
	}
	assert.Equal(t, want, got, "newest first, each message once")
}
