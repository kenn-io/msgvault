package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type exportChunkResp struct {
	MessageID            int64   `json:"message_id"`
	SourceMessageID      string  `json:"source_message_id"`
	ConversationID       int64   `json:"conversation_id"`
	SourceConversationID string  `json:"source_conversation_id"`
	SourceID             int64   `json:"source_id"`
	Account              string  `json:"account"`
	SourceType           string  `json:"source_type"`
	LastSyncAt           *string `json:"last_sync_at"`
	Offset               int64   `json:"offset"`
	Length               int64   `json:"length"`
	Size                 int64   `json:"size"`
	SHA256               string  `json:"sha256"`
	Complete             bool    `json:"complete"`
	DataBase64           string  `json:"data_base64"`
}

// exportMIME is 1,000 bytes of CRLF, bare LF, NUL and 8-bit content so a
// small chunk length splits it across many calls, including mid-line.
var exportMIME = func() []byte {
	var b bytes.Buffer
	b.WriteString("From: sender@example.com\r\nSubject: chunked\r\n\r\n")
	for b.Len() < 1000 {
		b.Write([]byte{'a', 0x00, 0xe9, '\r', '\n', 'b', '\n', 0xff})
	}
	return b.Bytes()[:1000]
}()

type exportFixture struct {
	f       *storetest.Fixture
	engine  *query.SQLiteEngine
	h       *handlers
	withRaw int64
	noRaw   int64
}

func newExportFixture(t *testing.T) exportFixture {
	t.Helper()
	f := storetest.New(t)
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	withRaw := f.NewMessage().WithSourceMessageID("export-provider-id").
		WithSentAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)).Create(t, f.Store)
	require.NoError(t, f.Store.UpsertMessageRaw(withRaw, exportMIME))
	noRaw := f.NewMessage().WithSourceMessageID("export-no-raw").
		WithSentAt(time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)).Create(t, f.Store)
	return exportFixture{f: f, engine: engine, h: newTestHandlers(engine), withRaw: withRaw, noRaw: noRaw}
}

func downloadEML(t *testing.T, h *handlers, args map[string]any, length int) ([]byte, exportChunkResp, int) {
	t.Helper()
	var out []byte
	var last exportChunkResp
	calls := 0
	for {
		call := map[string]any{"offset": float64(len(out)), "length": float64(length)}
		maps.Copy(call, args)
		last = runTool[exportChunkResp](t, ToolExportEML, h.exportEML, call)
		calls++
		require.Equal(t, int64(len(out)), last.Offset)
		part, err := base64.StdEncoding.DecodeString(last.DataBase64)
		require.NoError(t, err)
		require.Equal(t, last.Length, int64(len(part)))
		require.LessOrEqual(t, len(part), length)
		out = append(out, part...)
		if last.Complete {
			return out, last, calls
		}
		require.NotEmpty(t, part, "empty chunk before completion")
	}
}

func TestExportEMLChunksAreByteExact(t *testing.T) {
	checks := assert.New(t)
	x := newExportFixture(t)
	sum := sha256.Sum256(exportMIME)

	got, last, calls := downloadEML(t, x.h, map[string]any{"id": float64(x.withRaw)}, 97)
	checks.Equal(exportMIME, got)
	checks.Equal(hex.EncodeToString(sum[:]), last.SHA256)
	checks.Equal(int64(len(exportMIME)), last.Size)
	checks.Equal(11, calls, "1000 bytes in 97-byte chunks")
	checks.Equal(x.withRaw, last.MessageID)
	checks.Equal("export-provider-id", last.SourceMessageID)
	checks.Equal("default-thread", last.SourceConversationID)
	checks.Equal("test@example.com", last.Account)
	checks.Equal("gmail", last.SourceType)
	checks.Nil(last.LastSyncAt, "never-synced account omits last_sync_at")

	byProviderID, _, _ := downloadEML(t, x.h, map[string]any{"source_message_id": "export-provider-id"}, 4194304)
	checks.Equal(exportMIME, byProviderID)
}

func TestExportEMLDefaultChunkAndEndOffset(t *testing.T) {
	checks := assert.New(t)
	x := newExportFixture(t)

	whole := runTool[exportChunkResp](t, ToolExportEML, x.h.exportEML, map[string]any{"id": float64(x.withRaw)})
	checks.True(whole.Complete)
	checks.Equal(int64(1000), whole.Length)

	end := runTool[exportChunkResp](t, ToolExportEML, x.h.exportEML,
		map[string]any{"id": float64(x.withRaw), "offset": float64(1000)})
	checks.True(end.Complete)
	checks.Zero(end.Length)
	checks.Empty(end.DataBase64)
}

func TestExportEMLErrors(t *testing.T) {
	x := newExportFixture(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no identifier", map[string]any{}, "provide exactly one of id or source_message_id"},
		{"both identifiers", map[string]any{"id": float64(x.withRaw), "source_message_id": "export-provider-id"}, "provide exactly one of id or source_message_id"},
		{"unknown", map[string]any{"id": float64(999999)}, "message not found"},
		{"no raw", map[string]any{"id": float64(x.noRaw)}, "raw_mime_unavailable"},
		{"offset past end", map[string]any{"id": float64(x.withRaw), "offset": float64(1001)}, "offset 1001 is past the end"},
		{"zero length", map[string]any{"id": float64(x.withRaw), "length": float64(0)}, "length must be between 1 and 4194304"},
		{"length too large", map[string]any{"id": float64(x.withRaw), "length": float64(4194305)}, "length must be between 1 and 4194304"},
		{"fractional offset", map[string]any{"id": float64(x.withRaw), "offset": 1.5}, "offset must be a non-negative integer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runToolExpectError(t, ToolExportEML, x.h.exportEML, tc.args)
			assert.Contains(t, resultText(t, r), tc.want)
		})
	}

	unsupported := runToolExpectError(t, ToolExportEML, newTestHandlers(&querytest.MockEngine{}).exportEML,
		map[string]any{"id": float64(1)})
	assert.Contains(t, resultText(t, unsupported), "not supported")
}

func TestExportEMLThroughOfficialSDK(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	x := newExportFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Engine: x.engine}, false).Connect(ctx, serverTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "export-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(session.Close()) })

	var got []byte
	var last exportChunkResp
	for {
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      ToolExportEML,
			Arguments: map[string]any{"id": x.withRaw, "offset": len(got), "length": 400},
		})
		must.NoError(err)
		must.False(result.IsError, "%v", result.Content)
		text, ok := result.Content[0].(*sdkmcp.TextContent)
		must.True(ok, "first content is the JSON text a gateway client reads")
		must.NoError(json.Unmarshal([]byte(text.Text), &last))
		part, err := base64.StdEncoding.DecodeString(last.DataBase64)
		must.NoError(err)
		got = append(got, part...)
		if last.Complete {
			break
		}
	}
	sum := sha256.Sum256(got)
	checks.Equal(exportMIME, got)
	checks.Equal(hex.EncodeToString(sum[:]), last.SHA256)
}

type listThreadResp struct {
	ConversationID       int64   `json:"conversation_id"`
	SourceConversationID string  `json:"source_conversation_id"`
	Account              string  `json:"account"`
	MessageID            int64   `json:"message_id"`
	Total                int64   `json:"total"`
	Offset               int     `json:"offset"`
	HasMore              bool    `json:"has_more"`
	LastSyncAt           *string `json:"last_sync_at"`
	Messages             []struct {
		ID              int64           `json:"id"`
		SourceMessageID string          `json:"source_message_id"`
		SentAt          string          `json:"sent_at"`
		HasRaw          bool            `json:"has_raw"`
		From            []query.Address `json:"from"`
	} `json:"messages"`
}

func TestListThread(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	x := newExportFixture(t)

	byID := runTool[listThreadResp](t, ToolListThread, x.h.listThread, map[string]any{"id": float64(x.noRaw)})
	must.Len(byID.Messages, 2)
	checks.Equal(x.withRaw, byID.Messages[0].ID)
	checks.True(byID.Messages[0].HasRaw)
	checks.Equal("2026-01-02T03:04:05Z", byID.Messages[0].SentAt)
	checks.Equal(x.noRaw, byID.Messages[1].ID)
	checks.False(byID.Messages[1].HasRaw)
	checks.Equal(int64(2), byID.Total)
	checks.False(byID.HasMore)
	checks.Equal(x.noRaw, byID.MessageID)
	checks.Equal("default-thread", byID.SourceConversationID)
	checks.Equal("test@example.com", byID.Account)

	page := runTool[listThreadResp](t, ToolListThread, x.h.listThread,
		map[string]any{"thread_id": "default-thread", "account": "test@example.com", "limit": float64(1)})
	must.Len(page.Messages, 1)
	checks.Equal(x.withRaw, page.Messages[0].ID)
	checks.True(page.HasMore)

	byProvider := runTool[listThreadResp](t, ToolListThread, x.h.listThread,
		map[string]any{"source_message_id": "export-provider-id", "offset": float64(1)})
	must.Len(byProvider.Messages, 1)
	checks.Equal(x.noRaw, byProvider.Messages[0].ID)
	checks.Equal(1, byProvider.Offset)

	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"no anchor":     {map[string]any{}, "provide exactly one of id, source_message_id, or thread_id"},
		"two anchors":   {map[string]any{"id": float64(x.noRaw), "thread_id": "default-thread"}, "provide exactly one of id, source_message_id, or thread_id"},
		"limit too big": {map[string]any{"thread_id": "default-thread", "limit": float64(501)}, "limit must be between 1 and 500"},
		"bad offset":    {map[string]any{"thread_id": "default-thread", "offset": float64(-1)}, "offset must be a non-negative integer"},
		"unknown":       {map[string]any{"thread_id": "missing"}, "thread not found"},
	} {
		t.Run(name, func(t *testing.T) {
			r := runToolExpectError(t, ToolListThread, x.h.listThread, tc.args)
			assert.Contains(t, resultText(t, r), tc.want)
		})
	}
}

func TestGetAttachmentChunks(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	tmpDir := t.TempDir()
	content := bytes.Repeat([]byte{0x00, 0xff, '\r', '\n', 'x'}, 41) // 205 bytes
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	createAttachmentFixture(t, tmpDir, hash, content)
	h := &handlers{
		engine: &querytest.MockEngine{Attachments: map[int64]*query.AttachmentInfo{
			10: {ID: 10, Filename: "sheet.xlsx", MimeType: "application/octet-stream", Size: int64(len(content)), ContentHash: hash},
		}},
		attachmentsDir: tmpDir,
	}

	var got []byte
	for {
		r := callToolDirect(t, ToolGetAttachment, h.getAttachment,
			map[string]any{"attachment_id": float64(10), "offset": float64(len(got)), "length": float64(64)})
		must.False(r.isError, "unexpected error: %s", resultText(t, r))
		checks.Nil(r.embeddedResource, "chunk mode carries bytes in data_base64 only")
		var chunk struct {
			Filename   string `json:"filename"`
			Offset     int64  `json:"offset"`
			Length     int64  `json:"length"`
			Size       int64  `json:"size"`
			SHA256     string `json:"sha256"`
			Complete   bool   `json:"complete"`
			DataBase64 string `json:"data_base64"`
		}
		must.NoError(json.Unmarshal([]byte(resultText(t, r)), &chunk))
		checks.Equal("sheet.xlsx", chunk.Filename)
		checks.Equal(int64(len(got)), chunk.Offset)
		checks.Equal(hash, chunk.SHA256)
		part, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
		must.NoError(err)
		checks.Equal(chunk.Length, int64(len(part)))
		got = append(got, part...)
		if chunk.Complete {
			break
		}
	}
	checks.Equal(content, got)

	legacy := callToolDirect(t, ToolGetAttachment, h.getAttachment, map[string]any{"attachment_id": float64(10)})
	must.False(legacy.isError)
	must.NotNil(legacy.embeddedResource, "without offset or length the blob stays embedded")
	var legacyFields map[string]any
	must.NoError(json.Unmarshal([]byte(resultText(t, legacy)), &legacyFields))
	checks.NotContains(legacyFields, "data_base64")
	checks.NotContains(legacyFields, "offset")

	bad := runToolExpectError(t, ToolGetAttachment, h.getAttachment,
		map[string]any{"attachment_id": float64(10), "offset": float64(206)})
	checks.Contains(resultText(t, bad), "offset 206 is past the end")
}

func TestGetAttachmentChunkThroughOfficialSDK(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	fixture := newTask5Fixture(t, "000")
	client := task5ConnectClient(t, fixture.opts, false)
	result, err := client.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      ToolGetAttachment,
		Arguments: map[string]any{"attachment_id": 7, "offset": 0, "length": 8},
	})
	must.NoError(err)
	task5AssertJSONParity(t, ToolGetAttachment, result)
	must.Len(result.Content, 1, "chunk mode has no embedded resource")
	chunk := task5StructuredAs[map[string]any](t, result)
	encoded, ok := chunk["data_base64"].(string)
	must.True(ok, "data_base64 is a string")
	data, err := base64.StdEncoding.DecodeString(encoded)
	must.NoError(err)
	checks.Equal(fixture.attachmentBytes[:8], data)
	checks.Equal(false, chunk["complete"])
}
