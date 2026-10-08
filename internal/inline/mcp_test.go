package inline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inlineToolCall struct {
	name string
	args map[string]any
}
type inlineMCPFixture struct {
	endpoint   string
	owner      atomic.Int64
	missing    atomic.Int32
	inits      atomic.Int32
	expired    atomic.Int32
	mu         sync.Mutex
	calls      []inlineToolCall
	payloads   map[string]string
	catalog    func(map[string]any) string
	structured bool
	reject     string
}

func newInlineMCPFixture(t *testing.T, payloads map[string]string, structured bool) *inlineMCPFixture {
	t.Helper()
	f := &inlineMCPFixture{payloads: payloads, structured: structured}
	f.owner.Store(1)
	srv := mcp.NewServer(&mcp.Implementation{Name: "inline-fixture", Version: "0.2.0"}, nil)
	for _, name := range []string{"account.me", "conversations.get", "conversations.list", "messages.list", "files.get"} {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.calls = append(f.calls, inlineToolCall{name: req.Params.Name, args: args})
			reject := f.reject
			body := f.payloads[req.Params.Name]
			catalog := f.catalog
			f.mu.Unlock()
			if req.Params.Name == "conversations.list" && catalog != nil {
				body = catalog(args)
			}
			if req.Params.Name == "account.me" {
				body = fmt.Sprintf(`{"user":{"id":"%d"},"session":{"scopes":["messages:read","offline_access"]},"allowed":{"spaceIds":["9"],"allowDms":true,"allowHomeThreads":true}}`, f.owner.Load())
			}
			if req.Params.Name == reject {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "private-secret-provider-payload"}}}, nil
			}
			if f.structured {
				var content map[string]any
				if err := json.Unmarshal([]byte(body), &content); err != nil {
					return nil, err
				}
				return &mcp.CallToolResult{StructuredContent: content, Content: []mcp.Content{&mcp.TextContent{Text: "presentation text is not the archive"}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/v2" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read MCP request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var request struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, "decode MCP request", http.StatusBadRequest)
				return
			}
			// notifications/initialized can also be sessionless. Count the
			// actual handshake request so the test measures reconnects.
			if request.Method == "initialize" {
				f.inits.Add(1)
			}
		}
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") != "" && f.missing.Load() > 0 {
			f.missing.Add(-1)
			f.expired.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unknown_session"}`))
			return
		}
		handler.ServeHTTP(w, r)
	}))
	f.endpoint = httpServer.URL + "/mcp/v2"
	t.Cleanup(httpServer.Close)
	return f
}

func (f *inlineMCPFixture) historyCalls() []inlineToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []inlineToolCall
	for _, call := range f.calls {
		if call.name == "messages.list" {
			calls = append(calls, call)
		}
	}
	return calls
}

func connectInlineFixture(t *testing.T, f *inlineMCPFixture) *MCPClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := NewMCPClient(ctx, f.endpoint, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	return c
}

func inlineMessageJSON(t *testing.T, id int64, text string, media string) string {
	t.Helper()
	data, err := json.Marshal(text)
	require.NoError(t, err)
	if media == "" {
		media = "null"
	}
	return fmt.Sprintf(`{"id":"%d","chatId":"7","fromId":"2","text":%s,"out":false,"date":"1788220800","editDate":"1788220900","replyToMsgId":"3","senderDisplayName":"Example Author","media":%s,"links":[],"urlPreviews":[],"externalTasks":[]}`, id, data, media)
}

func TestMCPHistoryBothResultFormats(t *testing.T) {
	for _, structured := range []bool{true, false} {
		t.Run(fmt.Sprintf("structured_%t", structured), func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)

			longText := strings.Repeat("complete history text ", 80)
			media := `{"kind":"document","id":"11","url":"https://media.example.invalid/signed","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":42}`
			f := newInlineMCPFixture(t, map[string]string{
				"messages.list":     `{"chat":{"chatId":"7","title":"Selected","kind":"space_chat"},"nextOffsetId":"4","messages":[` + inlineMessageJSON(t, 4, "older", "") + `,` + inlineMessageJSON(t, 5, longText, media) + `]}`,
				"conversations.get": `{"chat":{"chatId":"7","title":"Selected","kind":"space_chat"},"details":{"parentChatId":"6","parentMessageId":"3","groupParticipantCount":2},"participants":[{"userId":"2"}]}`,
			}, structured)
			c := connectInlineFixture(t, f)
			a, err := c.Me(t.Context())
			requires.NoError(err)
			assertions.Equal(int64(1), a.UserID)
			assertions.Equal(ProductionOrigin, a.Origin)
			conv, err := c.Conversation(t.Context(), 7)
			requires.NoError(err)
			assertions.Equal("channel", conv.Type)
			assertions.Equal(int64(6), conv.ParentChatID)
			assertions.Equal(int64(3), conv.RootMessageID)
			assertions.False(conv.MemberCountKnown)
			page, err := c.Messages(t.Context(), 7, 9)
			requires.NoError(err)
			requires.Len(page.Messages, 2)
			assertions.True(page.HasMore)
			assertions.Equal(int64(4), page.NextBeforeID)
			assertions.Equal(int64(5), page.Messages[0].ID)
			assertions.Equal(longText, page.Messages[0].Text)
			assertions.Equal(RawMCPFormat, page.Messages[0].RawFormat)
			assertions.Equal("Example Author", page.Messages[0].SenderName)
			assertions.Equal(time.Unix(1788220800, 0).UTC(), page.Messages[0].SentAt)
			assertions.Nil(page.Messages[0].Reactions)
			requires.Len(page.Messages[0].Media, 1)
			assertions.Equal("document:11", page.Messages[0].Media[0].ID)
			assertions.Equal(int64(5), page.Messages[0].Media[0].MessageID)
			assertions.Equal(int64(42), page.Messages[0].Media[0].Size)
			assertions.JSONEq(inlineMessageJSON(t, 5, longText, media), string(page.Messages[0].Raw))
			requires.Len(f.historyCalls(), 1)
			assertions.Equal(map[string]any{"chatId": "7", "limit": float64(50), "offsetId": "9"}, f.historyCalls()[0].args)
		})
	}
}

func TestMCPRejectsInvalidHistory(t *testing.T) {
	good := inlineMessageJSON(t, 5, "complete", "")
	tests := []struct {
		name, payload string
		before        int64
	}{
		{"missing messages", `{"chat":{"chatId":"7"},"nextOffsetId":null}`, 0},
		{"wrong conversation", `{"chat":{"chatId":"8"},"nextOffsetId":null,"messages":[]}`, 0},
		{"duplicate IDs", `{"chat":{"chatId":"7"},"nextOffsetId":"5","messages":[` + good + `,` + good + `]}`, 0},
		{"nonprogressing cursor", `{"chat":{"chatId":"7"},"nextOffsetId":"9","messages":[` + good + `]}`, 9},
		{"cursor skips rows", `{"chat":{"chatId":"7"},"nextOffsetId":"1","messages":[` + good + `]}`, 0},
		{"empty continuing page", `{"chat":{"chatId":"7"},"nextOffsetId":"5","messages":[]}`, 0},
		{"offset is exclusive", `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + good + `]}`, 5},
		{"missing full text", `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[{"id":"5","chatId":"7","out":false,"date":"1788220800"}]}`, 0},
		{"invalid date", `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[{"id":"5","chatId":"7","text":"","out":false,"date":"yesterday"}]}`, 0},
		{"overflow ID", `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[{"id":"9223372036854775808","chatId":"7","text":"","out":false,"date":"1788220800"}]}`, 0},
		{"wrong message chat", `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + strings.Replace(good, `"chatId":"7"`, `"chatId":"8"`, 1) + `]}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newInlineMCPFixture(t, map[string]string{"messages.list": tc.payload}, false)
			c := connectInlineFixture(t, f)
			_, err := c.Messages(t.Context(), 7, tc.before)
			require.ErrorIs(t, err, ErrContract)
		})
	}
}

func TestMCPSessionExpiryReconnectsOnceAndPinsOwner(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineMCPFixture(t, map[string]string{"messages.list": `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[]}`}, true)
	c := connectInlineFixture(t, f)
	initial := f.inits.Load()
	f.missing.Store(1)
	page, err := c.Messages(t.Context(), 7, 0)
	requires.NoError(err)
	assertions.False(page.HasMore)
	assertions.Equal(initial+1, f.inits.Load())
	assertions.Equal(int32(1), f.expired.Load())
	f.owner.Store(2)
	f.missing.Store(1)
	_, err = c.Messages(t.Context(), 7, 0)
	requires.ErrorIs(err, ErrContract)
	_, err = c.Messages(t.Context(), 7, 0)
	requires.ErrorIs(err, ErrContract)
	assertions.Len(f.historyCalls(), 1, "new principal must never read into old account archive")
	assertions.Equal(initial+2, f.inits.Load(), "a rejected principal must not reconnect on subsequent calls")
	assertions.Equal(int32(2), f.expired.Load())
}

func TestMCPReadReconnectIsBounded(t *testing.T) {
	assertions := assert.New(t)

	f := newInlineMCPFixture(t, map[string]string{"messages.list": `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[]}`}, true)
	c := connectInlineFixture(t, f)
	initial := f.inits.Load()
	f.missing.Store(50)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := c.Messages(ctx, 7, 0)
	require.Error(t, err)
	assertions.Equal(initial+1, f.inits.Load())
	assertions.Equal(int32(2), f.expired.Load(), "original read and reconnected account check must each be attempted once")
	assertions.Empty(f.historyCalls(), "expired sessions must not execute history reads")
}

func TestMCPToolErrorDoesNotExposePayload(t *testing.T) {
	f := newInlineMCPFixture(t, nil, true)
	c := connectInlineFixture(t, f)
	f.mu.Lock()
	f.reject = "messages.list"
	f.mu.Unlock()
	_, err := c.Messages(t.Context(), 7, 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-secret-provider-payload")
	_, err = inlineToolJSON(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "not json"}}})
	require.ErrorIs(t, err, ErrContract)
}

func TestMCPFilesRefreshOnlyRequestedDirectMedia(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	media := `{"kind":"document","id":"11","url":"https://media.example.invalid/fresh","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":42}`
	file := `{"source":"message_media","messageId":"5","kind":"document","id":"11","url":"https://media.example.invalid/fresh","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":42}`
	payload := `{"chat":{"chatId":"7"},"items":[{"message":` + inlineMessageJSON(t, 5, "", media) + `,"files":[` + file + `]}]}`
	f := newInlineMCPFixture(t, map[string]string{"files.get": payload}, true)
	c := connectInlineFixture(t, f)
	files, err := c.Files(t.Context(), 7, []int64{5})
	requires.NoError(err)
	requires.Len(files, 1)
	assertions.Equal("document:11", files[0].ID)
	assertions.Equal("https://media.example.invalid/fresh", files[0].URL)
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	assertions.Equal(map[string]any{"chatId": "7", "messageIds": []any{"5"}, "includeUrlPreviews": false}, last.args)
	for _, bad := range []string{strings.Replace(payload, `"source":"message_media"`, `"source":"url_preview_media"`, 1), strings.Replace(payload, `"messageId":"5"`, `"messageId":"6"`, 1), strings.Replace(payload, `"kind":"document","id":"11","url":"https://media.example.invalid/fresh","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":42}]}]}`, `"kind":"document","id":"12","url":"https://media.example.invalid/fresh","fileName":"notes.txt","mimeType":"text/plain","sizeBytes":42}]}]}`, 1)} {
		f.mu.Lock()
		f.payloads["files.get"] = bad
		f.mu.Unlock()
		_, err = c.Files(t.Context(), 7, []int64{5})
		requires.ErrorIs(err, ErrContract)
	}
}

func TestMCPEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"https://example.invalid/mcp/v2", "https://mcp.inline.chat/mcp/v2?tenant=other", "https://user:secret@mcp.inline.chat/mcp/v2", "http://mcp.inline.chat/mcp/v2", "https://mcp.inline.chat/mcp/v2/"} {
		_, err := validateMCPEndpoint(endpoint)
		require.Error(t, err)
	}
	_, err := validateMCPEndpoint(DefaultMCPEndpoint)
	require.NoError(t, err)
}

func TestMCPPhotoVideoWithoutMIMEPreserveKind(t *testing.T) {
	for _, kind := range []string{"photo", "video"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)

			// Actual MCP photo/video projection carries dimensions and URLs,
			// but has no MIME field. Classification must retain the source kind.
			media := fmt.Sprintf(`{"kind":%q,"id":"11","url":"https://media.example.invalid/signed","sizeBytes":42,"width":640,"height":480}`, kind)
			payload := `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + inlineMessageJSON(t, 5, "", media) + `]}`
			f := newInlineMCPFixture(t, map[string]string{"messages.list": payload}, true)
			c := connectInlineFixture(t, f)
			page, err := c.Messages(t.Context(), 7, 0)
			requires.NoError(err)
			requires.Len(page.Messages, 1)
			requires.Len(page.Messages[0].Media, 1)
			assertions.Equal(kind, page.Messages[0].Media[0].Role)
			assertions.Equal(kind+":11", page.Messages[0].Media[0].ID)
			assertions.Empty(page.Messages[0].Media[0].MIMEType)
			assertions.Equal(640, page.Messages[0].Media[0].Width)
		})
	}
}

func TestMCPDiscoverCompleteCatalogBeyondFifty(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	f := newInlineMCPFixture(t, nil, true)
	f.catalog = func(args map[string]any) string {
		after := int64(0)
		if raw, ok := args["afterChatId"].(string); ok {
			after, _ = positiveID(raw)
		}
		var items []string
		last := min(after+50, int64(125))
		for id := after + 1; id <= last; id++ {
			items = append(items, fmt.Sprintf(`{"chatId":"%d","title":"Example %d","kind":"space_chat"}`, id, id))
		}
		next := "null"
		if last < 125 {
			next = fmt.Sprintf(`"%d"`, last)
		}
		return fmt.Sprintf(`{"sort":"id","nextAfterChatId":%s,"items":[%s]}`, next, strings.Join(items, ","))
	}
	c := connectInlineFixture(t, f)
	chats, err := c.Discover(t.Context())
	requires.NoError(err)
	requires.Len(chats, 125)
	for index, chat := range chats {
		assertions.Equal(int64(index+1), chat.ID)
		assertions.False(chat.MemberCountKnown)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []map[string]any
	for _, call := range f.calls {
		if call.name == "conversations.list" {
			calls = append(calls, call.args)
		}
	}
	assertions.Equal([]map[string]any{
		{"includeSubthreads": true, "sort": "id", "limit": float64(50)},
		{"includeSubthreads": true, "sort": "id", "limit": float64(50), "afterChatId": "50"},
		{"includeSubthreads": true, "sort": "id", "limit": float64(50), "afterChatId": "100"},
	}, calls)
}

func TestMCPDiscoverRequiresValidPagesAndAdvancingCursor(t *testing.T) {
	for _, payload := range []string{
		`{"items":[]}`,
		`{"sort":"recent","nextAfterChatId":null,"items":[]}`,
		`{"sort":"id","items":[]}`,
		`{"sort":"id","nextAfterChatId":"5","items":[]}`,
		`{"sort":"id","nextAfterChatId":"6","items":[{"chatId":"5","kind":"dm"}]}`,
		`{"sort":"id","nextAfterChatId":null,"items":[{"chatId":"5","kind":"dm"},{"chatId":"5","kind":"dm"}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			f := newInlineMCPFixture(t, map[string]string{"conversations.list": payload}, true)
			c := connectInlineFixture(t, f)
			_, err := c.Discover(t.Context())
			require.ErrorIs(t, err, ErrContract)
		})
	}
	f := newInlineMCPFixture(t, map[string]string{"conversations.list": `{"sort":"id","nextAfterChatId":null,"items":[]}`}, true)
	c := connectInlineFixture(t, f)
	chats, err := c.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, chats)
}

func TestMCPDiscoverValidatesEveryCatalogPage(t *testing.T) {
	f := newInlineMCPFixture(t, nil, true)
	f.catalog = func(args map[string]any) string {
		if args["afterChatId"] == "5" {
			return `{"sort":"recent","nextAfterChatId":null,"items":[]}`
		}
		return `{"sort":"id","nextAfterChatId":"5","items":[{"chatId":"5","kind":"dm"}]}`
	}
	c := connectInlineFixture(t, f)
	chats, err := c.Discover(t.Context())
	require.ErrorIs(t, err, ErrContract)
	assert.Nil(t, chats, "a partial catalog must not be mistaken for a completed discovery")
}

func TestMCPDMPeerEstablishesOnlyKnownDirectMembership(t *testing.T) {
	for _, tc := range []struct {
		name, peer string
		count      int
		known      bool
		invalid    bool
	}{
		{"self DM", `{"userId":"1"}`, 1, true, false},
		{"other DM", `{"userId":"2"}`, 2, true, false},
		{"unknown peer", `null`, 0, false, false},
		{"null peer ID", `{"userId":null}`, 0, false, false},
		{"missing peer ID", `{}`, 0, false, true},
		{"malformed peer ID", `{"userId":"other"}`, 0, false, true},
		{"noncanonical peer ID", `{"userId":"02"}`, 0, false, true},
		{"unsafe peer ID", `{"userId":"9007199254740992"}`, 0, false, true},
		{"numeric peer ID", `{"userId":2}`, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			payload := `{"chat":{"chatId":"7","title":"Direct","kind":"dm","peer":` + tc.peer + `},"details":{"parentChatId":null,"parentMessageId":null}}`
			f := newInlineMCPFixture(t, map[string]string{"conversations.get": payload}, true)
			c := connectInlineFixture(t, f)
			conversation, err := c.Conversation(t.Context(), 7)
			if tc.invalid {
				require.ErrorIs(t, err, ErrContract)
				return
			}
			require.NoError(t, err)
			assertions.Equal(tc.known, conversation.MemberCountKnown)
			assertions.Equal(tc.count, conversation.MemberCount)
		})
	}
}

func TestMCPRejectsUnsafeIDsBeforeAnyProviderRead(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	id, err := positiveID("9007199254740991")
	requires.NoError(err)
	assertions.Equal(MaxID, id)
	for _, raw := range []string{"9007199254740992", "9223372036854775807", "01", "-1", "0"} {
		_, err := positiveID(raw)
		requires.ErrorIs(err, ErrContract)
	}
	_, err = decodeAccount([]byte(`{"user":{"id":"9007199254740992"},"session":{"scopes":["messages:read"]}}`))
	requires.ErrorIs(err, ErrContract)
	f := newInlineMCPFixture(t, nil, true)
	c := connectInlineFixture(t, f)
	_, err = c.Conversation(t.Context(), MaxID+1)
	requires.Error(err)
	_, err = c.Messages(t.Context(), 7, MaxID+1)
	requires.Error(err)
	_, err = c.Files(t.Context(), 7, []int64{MaxID + 1})
	requires.Error(err)
	f.mu.Lock()
	defer f.mu.Unlock()
	requires.Len(f.calls, 1, "unsafe request IDs must fail before calling a provider tool")
	assertions.Equal("account.me", f.calls[0].name)
}

func TestMCPResponseByteLimitChecksOverflowAndExactEOF(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		streamed   bool
		invalid    bool
	}{
		{"known exact limit", "12345678", false, false},
		{"streamed exact limit", "12345678", true, false},
		{"known overflow", "123456789", false, true},
		{"streamed overflow", "123456789", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.streamed {
					flusher, ok := w.(http.Flusher)
					if !assert.True(t, ok, "the fixture must support streamed responses") {
						http.Error(w, "streaming unsupported", http.StatusInternalServerError)
						return
					}
					flusher.Flush()
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			client := &http.Client{Transport: &mcpResponseTransport{base: http.DefaultTransport, limit: 8}}
			resp, err := client.Get(srv.URL)
			if err == nil {
				var body []byte
				body, err = io.ReadAll(resp.Body)
				requires.NoError(resp.Body.Close(), "the response byte limit must still permit closing the HTTP body")
				if !tc.invalid {
					assertions.Equal(tc.body, string(body))
				}
			}
			if tc.invalid {
				requires.ErrorIs(err, ErrContract, "overflow must produce an error, never silent truncation")
			} else {
				requires.NoError(err)
			}
		})
	}
}
