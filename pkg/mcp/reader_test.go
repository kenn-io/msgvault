package mcp_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"github.com/jackc/pgx/v5"
	internalmcp "go.kenn.io/msgvault/internal/mcp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
	catalog "go.kenn.io/msgvault/pkg/mcp"
)

type readFixture struct{ source, message, attachment int64 }

func TestReaderScopedMCP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := t.Context()
	a := openReaderArchive(t)

	attachments := t.TempDir()
	fixtures := make([]readFixture, 2)
	for i := range fixtures {
		source, err := a.Store().GetOrCreateSource("gmail", fmt.Sprintf("reader-%d@example.com", i))
		requirements.NoError(err)
		conv, err := a.Store().EnsureConversation(source.ID, "thread", "Telescope")
		requirements.NoError(err)
		id, err := a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv,
			SourceMessageID: "same-provider-id", MessageType: "email",
			Subject: sql.NullString{String: fmt.Sprintf("telescope source-%d", i), Valid: true},
			SentAt:  sql.NullTime{Time: time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC), Valid: true}})
		requirements.NoError(err)
		body := fmt.Sprintf("telescope body source-%d", i)
		requirements.NoError(a.Store().UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		requirements.NoError(a.Store().UpsertFTS(id, "telescope", body, "", "", ""))
		sum := sha256.Sum256([]byte(body))
		hash := hex.EncodeToString(sum[:])
		rel := filepath.Join(hash[:2], hash)
		requirements.NoError(os.MkdirAll(filepath.Join(attachments, filepath.Dir(rel)), 0700))
		requirements.NoError(os.WriteFile(filepath.Join(attachments, rel), []byte(body), 0600))
		requirements.NoError(a.Store().UpsertAttachment(id, "example.txt", "text/plain", rel, hash, len(body)))
		var attachment int64
		requirements.NoError(a.Store().DB().QueryRow(a.Store().Rebind("SELECT id FROM attachments WHERE message_id=?"), id).Scan(&attachment))
		fixtures[i] = readFixture{source.ID, id, attachment}
	}
	emptySource, err := a.Store().GetOrCreateSource("gmail", "empty@example.com")
	requirements.NoError(err)
	reader, err := catalog.NewReader(a, catalog.ReaderOptions{AttachmentsDir: attachments})
	requirements.NoError(err)
	t.Cleanup(reader.Close)
	var mu sync.Mutex
	scope := []int64{fixtures[0].source}
	setScope := func(ids []int64) { mu.Lock(); defer mu.Unlock(); scope = ids }
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "embedded-test", Version: "1"}, nil)
	for _, name := range []string{"get_message", "list_messages", "list_thread", "search_metadata", "search_message_bodies", "get_stats", "get_attachment"} {
		tool, err := catalog.ReadTool(name)
		requirements.NoError(err)
		sdkmcp.AddTool(server, tool, func(ctx context.Context, req *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, any, error) {
			mu.Lock()
			ids := slices.Clone(scope)
			mu.Unlock()
			return reader.Call(ctx, ids, req, args)
		})
	}
	ct, st := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(ss.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "reader", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(cs.Close()) })
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		r, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		requirements.NoError(err)
		return r
	}
	payload := func(r *sdkmcp.CallToolResult) map[string]any {
		t.Helper()
		encoded, _ := json.Marshal(r)
		requirements.False(r.IsError, string(encoded))
		data, err := json.Marshal(r.StructuredContent)
		requirements.NoError(err)
		var out map[string]any
		requirements.NoError(json.Unmarshal(data, &out))
		return out
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_messages", map[string]any{"limit": 1}},
		{"search_metadata", map[string]any{"query": "missing | telescope", "limit": 1}},
		{"search_message_bodies", map[string]any{"query": "telescope", "limit": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			out := payload(call(tc.name, tc.args))
			data, ok := out["data"].([]any)
			requirements.True(ok)
			requirements.Len(data, 1)
			row, ok := data[0].(map[string]any)
			requirements.True(ok)
			assertions.EqualValues(fixtures[0].message, row["id"])
			assertions.Equal(false, out["has_more"])
		})
	}
	stats := payload(call("get_stats", map[string]any{}))
	statsData, ok := stats["stats"].(map[string]any)
	requirements.True(ok)
	assertions.EqualValues(1, statsData["MessageCount"])
	requirements.Len(stats["accounts"], 1)
	goodID, badID := fixtures[0].message, fixtures[1].message
	for _, name := range []string{"get_message", "list_thread"} {
		out := payload(call(name, map[string]any{"id": goodID}))
		assertions.NotEmpty(out)
		assertions.True(call(name, map[string]any{"id": badID}).IsError)
	}
	assertions.True(call("list_messages", map[string]any{"account": "reader-1@example.com"}).IsError)
	thread := payload(call("list_thread", map[string]any{"source_message_id": "same-provider-id"}))
	assertions.NotEmpty(thread)
	for _, name := range []string{"search_metadata", "search_message_bodies"} {
		assertions.True(call(name, map[string]any{"query": "telescope", "account": "reader-1@example.com"}).IsError)
	}
	chunk := payload(call("get_attachment", map[string]any{"attachment_id": fixtures[0].attachment, "offset": 0, "length": 4}))
	assertions.Equal("dGVsZQ==", chunk["data_base64"])
	setScope([]int64{fixtures[1].source})
	denied := call("get_attachment", map[string]any{"attachment_id": fixtures[0].attachment, "offset": 4, "length": 4, "sha256": chunk["sha256"]})
	assertions.True(denied.IsError, "cached bytes require fresh scope")
	// The complete permitted archive has the same native wire result.
	setScope([]int64{fixtures[0].source, fixtures[1].source, emptySource.ID})
	nativeClientTransport, nativeServerTransport := sdkmcp.NewInMemoryTransports()
	nativeCtx, nativeCancel := context.WithCancel(ctx)
	nativeDone := make(chan error, 1)
	go func() {
		nativeDone <- internalmcp.ServeTransport(nativeCtx, internalmcp.ServeOptions{Engine: a.QueryEngine(), AttachmentsDir: attachments}, nativeServerTransport)
	}()
	native, err := client.Connect(ctx, nativeClientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(native.Close()); nativeCancel(); <-nativeDone })
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"get_message", map[string]any{"id": goodID, "offset": 2, "max_chars": 5, "body_format": "text"}},
		{"list_messages", map[string]any{"limit": 1, "offset": 1}},
		{"list_thread", map[string]any{"id": goodID}},
		{"search_metadata", map[string]any{"query": "telescope", "limit": 1}},
		{"search_message_bodies", map[string]any{"query": "telescope", "limit": 1}},
		{"get_stats", map[string]any{}},
		{"get_attachment", map[string]any{"attachment_id": fixtures[0].attachment}},
	} {
		expected, err := native.CallTool(ctx, &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		requirements.NoError(err)
		requirements.False(expected.IsError)
		actual := call(tc.name, tc.args)
		requirements.False(actual.IsError)
		want, err := json.Marshal(expected.StructuredContent)
		requirements.NoError(err)
		got, err := json.Marshal(actual.StructuredContent)
		requirements.NoError(err)
		assertions.JSONEq(string(want), string(got), tc.name)
		assertions.Equal(expected.Content, actual.Content, tc.name)
	}
	for _, ids := range [][]int64{nil, {}} {
		setScope(ids)
		out := payload(call("list_messages", map[string]any{}))
		assertions.Empty(out["data"])
		stats = payload(call("get_stats", map[string]any{}))
		assertions.Empty(stats["accounts"])
		statsData, ok = stats["stats"].(map[string]any)
		requirements.True(ok)
		assertions.EqualValues(0, statsData["MessageCount"])
		for _, name := range []string{"search_metadata", "search_message_bodies"} {
			empty := payload(call(name, map[string]any{"query": "telescope"}))
			assertions.Empty(empty["data"])
		}
		assertions.True(call("get_message", map[string]any{"id": goodID}).IsError)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = reader.Call(canceled, []int64{fixtures[0].source}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "get_stats"}}, map[string]any{})
	requirements.ErrorIs(err, context.Canceled)
	_, _, err = reader.Call(ctx, []int64{fixtures[0].source}, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "aggregate"}}, map[string]any{})
	requirements.Error(err, "catalog membership alone must not admit an unsupported operation")
}

func openReaderArchive(t *testing.T) *archive.Archive {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := t.Context()
	var a *archive.Archive
	var err error
	if dsn := os.Getenv("MSGVAULT_TEST_DB"); store.IsPostgresURL(dsn) {
		admin, openErr := sql.Open("pgx", dsn)
		requirements.NoError(openErr)
		schema := "mcp_reader_" + strings.ToLower(rand.Text())
		t.Cleanup(func() {
			_, err := admin.Exec("DROP SCHEMA IF EXISTS " + pgx.Identifier{schema}.Sanitize() + " CASCADE")
			assertions.NoError(err)
			assertions.NoError(admin.Close())
		})
		opts := archive.PostgreSQL{URL: dsn, Schema: schema}
		requirements.NoError(archive.Setup(ctx, opts))
		a, err = archive.Open(ctx, opts)
	} else {
		path := filepath.Join(t.TempDir(), "archive.db")
		requirements.NoError(archive.SetupSQLite(ctx, path))
		a, err = archive.OpenSQLite(ctx, path)
	}
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(a.Close()) })
	return a
}
