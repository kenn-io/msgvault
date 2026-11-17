//go:build cgo

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func TestSearchMetadataHTTPDisconnectCancelsSQLite(t *testing.T) {
	for _, version := range []string{"2025-03-26", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var enabled atomic.Bool
			// Give the page and count reads separate connections so both can be
			// running when the client disconnects.
			db, observer := sqlobserve.OpenFile(t, filepath.Join(t.TempDir(), "archive.db"), func(conn *sqlite3.SQLiteConn) error {
				var once sync.Once
				return conn.RegisterFunc(sqliteutil.UnicodeLowerFunction, func(value string) string {
					if enabled.Load() {
						once.Do(func() { started <- struct{}{} })
						<-release
					}
					return strings.ToLower(value)
				}, true)
			})
			// Reuse must wait for an interrupted read, never open a third connection.
			db.SetMaxOpenConns(2)
			seedMetadataSearch(t, db)
			var requests atomic.Int32
			adapter := metadataDaemon(t, query.NewSQLiteEngine(db), &requests)
			sqlContexts := make(chan context.Context, 2)
			observer.BeforeQuery = func(ctx context.Context, _ string) {
				select {
				case sqlContexts <- ctx:
				default:
				}
			}
			server := httptest.NewServer(newMCPHTTPServer(ServeOptions{Engine: adapter}, HTTPOptions{}).Handler)
			t.Cleanup(server.Close)
			observer.Reset()
			enabled.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_metadata","arguments":{"query":"subject:note","limit":2}}}`
			if version == "2026-07-28" {
				body = task3ToolCallBody(1, ToolSearchMetadata, `{"query":"subject:note","limit":2}`)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", strings.NewReader(body))
			require.NoError(err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", version)
			req.Header.Set("Mcp-Method", "tools/call")
			req.Header.Set("Mcp-Name", ToolSearchMetadata)
			responseDone := make(chan error, 1)
			go func() {
				resp, err := server.Client().Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				responseDone <- err
			}()
			for range 2 {
				select {
				case <-started:
				case err := <-responseDone:
					require.FailNow("request ended before both SQL reads started", "%v", err)
				case <-time.After(10 * time.Second):
					require.FailNow("both SQL reads did not start")
				}
			}
			cancel() // net/http closes the connection without an MCP cancel notification.
			for range 2 {
				sqlCtx := <-sqlContexts
				select {
				case <-sqlCtx.Done():
				case <-time.After(3 * time.Second):
					assert.Fail("client disconnect did not cancel the SQLite query context")
				}
			}
			unblock()
			select {
			case err := <-responseDone:
				require.ErrorIs(err, context.Canceled)
			case <-time.After(10 * time.Second):
				require.FailNow("HTTP client did not stop")
			}
			enabled.Store(false)
			var value int
			require.NoError(db.QueryRowContext(t.Context(), "SELECT 1").Scan(&value), "SQLite connection should be reusable after cancellation")
			assert.Equal(1, value)
			assert.Len(observer.Statements(), 3, "interrupted page and count, then connection reuse; no stats")
		})
	}
}
