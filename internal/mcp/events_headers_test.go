package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcpevents"
)

const eventsNameFixtureParams = `"name":"msgvault.message_archived","arguments":{"conversation_id":"42"},"delivery":{"mode":"webhook","url":"https://callback.example/hook","secret":"whsec_fixture"}`

func eventsNameCall(t *testing.T, method, body string, header map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	req := task3ModernRequest(method, "", body)
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Authorization", "Bearer owner-fixture")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var wire map[string]any
	_ = json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&wire)
	return rec, wire
}

func eventsNameBody(method string) string {
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	return `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":{` + meta + `,` + eventsNameFixtureParams + `}}`
}

func TestEventsNameHeader(t *testing.T) {
	name := "msgvault.message_archived"
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(name)) + "?="
	cases := []struct {
		desc   string
		header map[string]string
		reject bool
	}{
		{"missing", nil, true},
		{"other name", map[string]string{"Mcp-Name": "other.name"}, true},
		{"equal", map[string]string{"Mcp-Name": name}, false},
		{"base64 sentinel", map[string]string{"Mcp-Name": encoded}, false},
		{"malformed base64", map[string]string{"Mcp-Name": "=?base64?!!!?="}, true},
	}
	for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.desc, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				rec, wire := eventsNameCall(t, method, eventsNameBody(method), tc.header)
				if tc.reject {
					require.Equal(http.StatusBadRequest, rec.Code)
					errObj, ok := wire["error"].(map[string]any)
					require.True(ok, rec.Body.String())
					assert.InDelta(-32020.0, errObj["code"], 0.5)
					assert.InDelta(7.0, wire["id"], 0.5)
					return
				}
				assert.NotEqual(http.StatusBadRequest, rec.Code, rec.Body.String())
			})
		}
	}
	t.Run("events/list needs no name", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"events/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		rec, wire := eventsNameCall(t, "events/list", body, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Nil(t, wire["error"])
	})
	t.Run("batch fails closed for events methods", func(t *testing.T) {
		rec, wire := eventsNameCall(t, "events/subscribe", `[`+eventsNameBody("events/subscribe")+`]`, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		errObj, ok := wire["error"].(map[string]any)
		require.True(t, ok, rec.Body.String())
		assert.InDelta(t, -32020.0, errObj["code"], 0.5)
	})
	t.Run("tools/list untouched", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
		rec, _ := eventsNameCall(t, "tools/list", body, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

type countingEventsBackend struct {
	eventsTestBackend

	calls atomic.Int32
}

func (b *countingEventsBackend) MCPEventsSubscribe(ctx context.Context, req mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	b.calls.Add(1)
	return b.eventsTestBackend.MCPEventsSubscribe(ctx, req)
}

func TestEventsNameHeaderCannotBeBypassedByBodyAliases(t *testing.T) {
	name := "msgvault.message_archived"
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	params := `"name":"` + name + `","arguments":{"conversation_id":"42"},"delivery":{"mode":"webhook","url":"https://callback.example/hook","secret":"whsec_fixture"}`
	for _, tc := range []struct {
		desc   string
		body   string
		header map[string]string
	}{
		{"case-variant method member", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","METHOD":"tools/list","params":{` + meta + `,` + params + `}}`, nil},
		{"case-variant params member", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + meta + `,` + params + `},"PARAMS":{"name":"other"}}`, map[string]string{"Mcp-Name": "other"}},
		{"duplicate params.name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + meta + `,"name":"other",` + params + `}}`, map[string]string{"Mcp-Name": name}},
		{"missing params.name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + meta + `}}`, map[string]string{"Mcp-Name": name}},
		{"non-string params.name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + meta + `,"name":5}}`, map[string]string{"Mcp-Name": "5"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := &countingEventsBackend{}
			h := newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{APIKey: "owner-fixture"}).Handler
			req := task3ModernRequest("events/subscribe", "", tc.body)
			req.Header.Set("Authorization", "Bearer owner-fixture")
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(http.StatusBadRequest, rec.Code, rec.Body.String())
			var wire map[string]any
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &wire), rec.Body.String())
			errObj, ok := wire["error"].(map[string]any)
			require.True(ok, rec.Body.String())
			assert.InDelta(-32020.0, errObj["code"], 0.5)
			assert.Zero(backend.calls.Load())
		})
	}
	t.Run("control: valid request reaches the backend", func(t *testing.T) {
		backend := &countingEventsBackend{}
		h := newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{APIKey: "owner-fixture"}).Handler
		req := task3ModernRequest("events/subscribe", name, eventsNameBody("events/subscribe"))
		req.Header.Set("Authorization", "Bearer owner-fixture")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, int32(1), backend.calls.Load())
	})
}

func TestEventsNameHeaderSkippedWhenEventsNotServed(t *testing.T) {
	cases := []struct {
		desc    string
		version string
		http    HTTPOptions
		serve   ServeOptions
	}{
		{"old protocol", "2025-11-25", HTTPOptions{APIKey: "owner-fixture"}, ServeOptions{Events: eventsTestBackend{}}},
		{"independent credential", "2026-07-28", HTTPOptions{APIKey: "owner-fixture", IndependentCredential: true}, ServeOptions{Events: eventsTestBackend{}}},
		{"delegated only", "2026-07-28", HTTPOptions{APIKey: "owner-fixture"}, ServeOptions{DelegatedOnly: true, Events: eventsTestBackend{}}},
		{"no events backend", "2026-07-28", HTTPOptions{APIKey: "owner-fixture"}, ServeOptions{}},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			h := newMCPHTTPServer(tc.serve, tc.http).Handler
			req := task3ModernRequest("events/subscribe", "", eventsNameBody("events/subscribe"))
			req.Header.Set("Mcp-Protocol-Version", tc.version)
			req.Header.Set("Authorization", "Bearer owner-fixture")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// The SDK may answer 400 itself; the Mcp-Name guard must not.
			require.NotContains(rec.Body.String(), "header mismatch: Mcp-Name")
			if tc.version != "2026-07-28" {
				// Legacy protocols reject unregistered methods as plain text.
				assert.Contains(rec.Body.String(), "not handled")
				return
			}
			var wire map[string]any
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &wire), rec.Body.String())
			errObj, ok := wire["error"].(map[string]any)
			require.True(ok, rec.Body.String())
			assert.InDelta(-32601.0, errObj["code"], 0.5)
		})
	}
}

type recordingBody struct {
	r     io.Reader
	reads int
}

func (b *recordingBody) Read(p []byte) (int, error) { b.reads++; return b.r.Read(p) }
func (b *recordingBody) Close() error               { return nil }

func TestEventsNameHeaderSkipsBodyForOtherMethods(t *testing.T) {
	large := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x","pad":"` + strings.Repeat("a", 3<<20) + `"}}`
	small := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	for _, tc := range []struct{ method, body string }{
		{"tools/list", small},
		{"tools/call", large},
		{"events/list", small},
	} {
		t.Run(tc.method+"/"+strconv.Itoa(len(tc.body)), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			body := &recordingBody{r: strings.NewReader(tc.body)}
			var innerBody io.ReadCloser
			var innerReadsBefore int
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				innerBody = r.Body
				innerReadsBefore = body.reads
			})
			h := eventsNameHeaderHandler(func(*http.Request) bool { return true }, next)
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Body = body
			req.Header.Set("Mcp-Method", tc.method)
			h.ServeHTTP(httptest.NewRecorder(), req)
			require.NotNil(innerBody)
			assert.Same(body, innerBody)
			assert.Zero(innerReadsBefore)
			got, err := io.ReadAll(innerBody)
			require.NoError(err)
			assert.Len(got, len(tc.body))
		})
	}
	t.Run("real handler passes a large tools/call", func(t *testing.T) {
		h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
		req := task3ModernRequest("tools/call", "x", large)
		req.Header.Set("Authorization", "Bearer owner-fixture")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
		assert.NotContains(t, rec.Body.String(), "header mismatch: Mcp-Name")
	})
}

func TestEventsNameHeaderMethodHeaderMismatchRejected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Mcp-Method claims tools/call, so the guard stands aside; the SDK's own
	// Mcp-Method validation must reject the lie.
	rec, wire := eventsNameCall(t, "tools/call", eventsNameBody("events/subscribe"), nil)
	require.Equal(http.StatusBadRequest, rec.Code, rec.Body.String())
	errObj, ok := wire["error"].(map[string]any)
	require.True(ok, rec.Body.String())
	assert.InDelta(-32020.0, errObj["code"], 0.5)
}

func TestEventsNameHeaderOversizedEventsBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := eventsNameBody("events/subscribe")
	body = body[:len(body)-2] + `,"pad":"` + strings.Repeat("a", maxEventsBodyBytes) + `"}}`
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := eventsNameHeaderHandler(func(*http.Request) bool { return true }, next)
	req := task3ModernRequest("events/subscribe", "msgvault.message_archived", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(rec.Body.String(), "request body exceeds 1048576 bytes")
	assert.False(called)
}
