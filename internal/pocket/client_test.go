package pocket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPocketRESTClient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("Bearer pk_synthetic", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/public/recordings":
			assertions.Equal("1", r.URL.Query().Get("page"))
			assertions.Equal("100", r.URL.Query().Get("limit"))
			_, _ = fmt.Fprint(w, `{"success":true,"data":[{"id":"rec-a","title":"Planning","recording_at":"2026-09-01T10:00:00Z","duration":60}],"pagination":{"page":1,"limit":100,"has_more":false,"total":1,"total_pages":1}}`)
		case "/public/recordings/rec-a":
			assertions.Equal("true", r.URL.Query().Get("include_transcript"))
			assertions.Equal("true", r.URL.Query().Get("include_summarizations"))
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"id":"rec-a","transcript":[{"speaker":"Speaker One","text":"Decide roadmap","start":0,"end":3}],"duration":60}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", "pk_synthetic")
	p, err := c.ListRecordings(t.Context(), 1)
	requirements.NoError(err)
	requirements.Len(p.Recordings, 1)
	assertions.False(p.HasMore)
	rec, err := c.Recording(t.Context(), "rec-a")
	requirements.NoError(err)
	assertions.Equal("rec-a", rec.ID)
	content, err := Normalize(rec)
	requirements.NoError(err)
	requirements.Len(content.Transcript.Segments, 1)
	assertions.Equal("Decide roadmap", content.Transcript.Segments[0].Text)
}

func TestPocketClientRetryAndCancellation(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "99999999")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "private response pk_synthetic")
			}))
			defer srv.Close()
			client := NewClient(srv.URL, "", "pk_synthetic")
			var waits []time.Duration
			// Network requests execute normally. Only clock delays are captured
			// to test the stable retry contract without real provider waits.
			client.wait = func(_ context.Context, delay time.Duration) error { waits = append(waits, delay); return nil }
			_, err := client.ListRecordings(t.Context(), 1)
			requirements.Error(err)
			assertions.Equal(int32(5), calls.Load())
			requirements.Len(waits, 4)
			for _, delay := range waits {
				assertions.LessOrEqual(delay, 30*time.Second)
			}
			assertions.NotContains(err.Error(), "private response")
			assertions.NotContains(err.Error(), "pk_synthetic")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = client.ListRecordings(ctx, 1)
			assertions.ErrorIs(err, context.Canceled)
		})
	}
}

func TestPocketCurrentAccountRejectsUnconfirmedIdentity(t *testing.T) {
	for _, payload := range []string{`{"email":"owner@example.com"}`, `{"userId":"u"}`, `{"success":false,"data":{"email":"owner@example.com","userId":"u"}}`} {
		server := mcp.NewServer(&mcp.Implementation{Name: "pocket-test", Version: "1"}, nil)
		server.AddTool(&mcp.Tool{Name: "get_account_info", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: payload}}}, nil
		})
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
		_, err := NewClient("", srv.URL, "pk_synthetic").CurrentAccount(t.Context())
		require.Error(t, err)
		srv.Close()
	}
}

func TestPocketRESTClientRejectsIncompleteEnvelopes(t *testing.T) {
	for _, body := range []string{`{}`, `{"success":false,"error":"pk_synthetic private text"}`, `{"success":true,"data":null}`, `{"success":true,"data":[]}`, `{"success":true,"data":[],"pagination":{"page":1,"has_more":true,"total":2,"total_pages":1}}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer srv.Close()
			_, err := NewClient(srv.URL, "", "pk_synthetic").ListRecordings(t.Context(), 1)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private text")
			assert.NotContains(t, err.Error(), "pk_synthetic")
		})
	}
}

func TestPocketClientRefusesRedirectAndRedactsErrors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Empty(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	for _, status := range []int{302, 401, 403} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, "pk_synthetic private transcript")
		}))
		_, err := NewClient(srv.URL, "", "pk_synthetic").ListRecordings(t.Context(), 1)
		requirements.Error(err)
		assertions.NotContains(err.Error(), "pk_synthetic")
		assertions.NotContains(err.Error(), "private transcript")
		srv.Close()
	}
}

func TestPocketCurrentAccountUsesMCP(t *testing.T) {
	for _, structured := range []bool{true, false} {
		t.Run(strconv.FormatBool(structured), func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "pocket-test", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "get_account_info", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if structured {
					return &mcp.CallToolResult{StructuredContent: map[string]any{"success": true, "data": map[string]any{"email": " OWNER@example.com ", "userId": "user-a", "organization": map[string]any{"id": "org-example"}}}}, nil
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"success":true,"data":{"email":"owner@example.com","userId":"user-a"}}`}}}, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer pk_synthetic", r.Header.Get("Authorization"))
				handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			account, err := NewClient("", srv.URL, "pk_synthetic").CurrentAccount(t.Context())
			require.NoError(t, err)
			assert.Equal(t, Account{Email: "owner@example.com", UserID: "user-a"}, account)
		})
	}
}
