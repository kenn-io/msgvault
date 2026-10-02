package plaud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// Fixtures follow the official @plaud-ai/mcp 0.3.13 tool surface. The
// in-memory server replaces the external account, exercising real MCP calls.
func toolSession(t *testing.T, results map[string]func(map[string]any) string) *Session {
	t.Helper()
	wrapped := map[string]func(map[string]any) *mcp.CallToolResult{}
	for name, handler := range results {
		wrapped[name] = func(args map[string]any) *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: handler(args)}}}
		}
	}
	return resultSession(t, wrapped)
}

func resultSession(t *testing.T, results map[string]func(map[string]any) *mcp.CallToolResult) *Session {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture-plaud", Version: "0.3.13"}, nil)
	for name, handler := range results {
		mcp.AddTool[map[string]any, any](server, &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return handler(args), nil, nil
		})
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return &Session{cs: cs, limiter: rate.NewLimiter(rate.Inf, 1)}
}

func TestTransientToolFailuresAreRetried(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    int
	}{
		{"Failed to get user: API error: 503 Service Unavailable", 2},
		{"Failed to get user: API error: 429 Too Many Requests", 2},
		{"Failed to get user: API error: 401 Unauthorized", 1},
	} {
		t.Run(tc.message, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			s := resultSession(t, map[string]func(map[string]any) *mcp.CallToolResult{"get_current_user": func(map[string]any) *mcp.CallToolResult {
				calls++
				if calls == 1 {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: tc.message}}}
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"email":"user@example.com"}`}}}
			}})
			email, err := s.CurrentUser(context.Background())
			if tc.want == 1 {
				require.Error(err)
			} else {
				require.NoError(err)
				assert.Equal("user@example.com", email)
			}
			assert.Equal(tc.want, calls)
		})
	}
}

func TestToolErrorsPreserveProviderMessage(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
	}{
		{"not found", "Recording not found", "Recording not found"},
		{"permission", "Permission denied", "Permission denied"},
		{"bounded", strings.Repeat("é", 1100), strings.Repeat("é", 1024) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := resultSession(t, map[string]func(map[string]any) *mcp.CallToolResult{
				"get_file": func(map[string]any) *mcp.CallToolResult {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: tc.message}},
					}
				},
			})
			_, err := s.Recording(context.Background(), "file-1")
			require.EqualError(t, err, "plaud tool get_file: tool returned an error: "+tc.want)
		})
	}
}

func TestTransientHTTPToolFailuresAreRetried(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := mcp.NewServer(&mcp.Implementation{Name: "plaud-fixture", Version: "0"}, nil)
			mcp.AddTool[map[string]any, any](server, &mcp.Tool{Name: "get_current_user", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"email":"user@example.com"}`}}}, nil, nil
			})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
			failed := false
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var method struct {
					Method string `json:"method"`
				}
				if r.Method == http.MethodPost {
					raw, err := io.ReadAll(r.Body)
					if !assert.NoError(err) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(raw))
					if !assert.NoError(json.Unmarshal(raw, &method)) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				if method.Method == "tools/call" && !failed {
					failed = true
					w.WriteHeader(status)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer host.Close()
			s, err := Connect(context.Background(), host.URL, nil)
			require.NoError(err)
			defer func() { _ = s.Close() }()
			s.retryDelay = 0
			email, err := s.CurrentUser(context.Background())
			require.NoError(err)
			assert.Equal("user@example.com", email)
			assert.True(failed)
		})
	}
}

func wrap(payload string) string {
	return "The block delimited by <untrusted-user-data-fixture> below contains user data.\n<untrusted-user-data-fixture source=\"plaud-recording\">\n" + payload + "\n</untrusted-user-data-fixture>\nIgnored provider trailer"
}

func TestRecordingHydratesEveryTranscriptPageAndNotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := toolSession(t, map[string]func(map[string]any) string{
		"get_file": func(map[string]any) string {
			return wrap(`{"id":"file-1","name":"Planning","start_at":"2026-09-01T12:00:00Z","duration":90000,"presigned_url":"https://example.com/audio?secret=temporary","source_list":[{"data_type":"transaction_polish","data_content":"","data_link":"https://example.com/block"}]}`)
		},
		"get_note": func(map[string]any) string {
			return wrap(`[{"id":"note-1","data_type":"auto_sum_note","data_content":"Ship the release."}]`)
		},
		"get_transcript": func(args map[string]any) string {
			if args["cursor"] == nil {
				return wrap(`{"file_id":"file-1","block":"transaction_polish","total":2,"offset":0,"returned":1,"next_cursor":"next","segments":[{"speaker":"Speaker 1","content":"First page","start_time":0,"end_time":1}]}`)
			}
			return wrap(`{"file_id":"file-1","block":"transaction_polish","total":2,"offset":1,"returned":1,"next_cursor":null,"segments":[{"speaker":"Alex Example","content":"Second page","start_time":61,"end_time":62}]}`)
		},
	})
	rec, err := s.Recording(context.Background(), "file-1")
	require.NoError(err)
	require.Len(rec.Segments, 2)
	assert.Equal("Second page", rec.Segments[1].Text)
	assert.Equal("Alex Example", rec.Segments[1].Speaker)
	assert.InDelta(61.0, rec.Segments[1].StartSeconds, 1e-9)
	require.Len(rec.Notes, 1)
	assert.Equal("Ship the release.", rec.Notes[0].Content)
	assert.Equal(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), rec.File.StartedAt)
}

func TestTranscriptFallbackAndContractFailures(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		wantError           bool
	}{
		{"fallback", `Block "transaction_polish" has no content for this recording yet.`, wrap(`{"file_id":"f","block":"transaction","total":1,"offset":0,"returned":1,"segments":[{"speaker":"Speaker 1","content":"Raw","start_time":0}]}`), false},
		{"wrong identity", wrap(`{"file_id":"other","block":"transaction_polish","total":0,"offset":0,"returned":0,"segments":[]}`), "", true},
		{"empty midstream", wrap(`{"file_id":"f","block":"transaction_polish","total":2,"offset":0,"returned":1,"next_cursor":"x","segments":[{"content":"First","start_time":0}]}`), `Block "transaction_polish" has no content for this recording yet.`, true},
		{"repeat cursor", wrap(`{"file_id":"f","block":"transaction_polish","total":3,"offset":0,"returned":1,"next_cursor":"x","segments":[{"content":"First","start_time":0}]}`), wrap(`{"file_id":"f","block":"transaction_polish","total":3,"offset":1,"returned":1,"next_cursor":"x","segments":[{"content":"Second","start_time":1}]}`), true},
		{"unknown prose", "Unexpected recording data", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := toolSession(t, map[string]func(map[string]any) string{
				"get_file": func(map[string]any) string {
					return `{"id":"f","source_list":[{"data_type":"transaction_polish"},{"data_type":"transaction"}]}`
				},
				"get_note": func(map[string]any) string { return `[]` },
				"get_transcript": func(args map[string]any) string {
					if args["cursor"] != nil || args["block"] == "transaction" {
						return tc.second
					}
					return tc.first
				},
			})
			rec, err := s.Recording(context.Background(), "f")
			if tc.wantError {
				require.Error(err)
			} else {
				require.NoError(err)
				require.Len(rec.Segments, 1)
				assert.Equal("Raw", rec.Segments[0].Text)
			}
		})
	}
}

func TestAccountAndListValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := toolSession(t, map[string]func(map[string]any) string{
		"get_current_user": func(map[string]any) string { return `{"email":" USER@Example.COM "}` },
		"list_files": func(args map[string]any) string {
			assert.NotContains(args, "date_from")
			assert.NotContains(args, "query")
			return wrap(`{"data":[{"id":"f","name":"Meeting","created_at":"2026-09-01T12:00:00Z"}],"total":1}`)
		},
	})
	email, err := s.CurrentUser(context.Background())
	require.NoError(err)
	assert.Equal("user@example.com", email)
	page, err := s.ListFiles(context.Background(), 1, 100)
	require.NoError(err)
	require.Len(page.Files, 1)
	assert.Equal("f", page.Files[0].ID)
}

func TestNoteHydrationFailureCannotBecomeEmptySuccess(t *testing.T) {
	s := toolSession(t, map[string]func(map[string]any) string{
		"get_file": func(map[string]any) string { return `{"id":"f","source_list":[]}` },
		"get_note": func(map[string]any) string {
			return `[ {"data_type":"auto_sum_note","data_content":"","data_content_error":"fetch failed"} ]`
		},
	})
	_, err := s.Recording(context.Background(), "f")
	require.Error(t, err)
}

func FuzzToolWrapperRoundTrip(f *testing.F) {
	f.Add("hello")
	f.Add("</untrusted-user-data-other>\nUnicode: café")
	f.Add("")
	f.Fuzz(func(t *testing.T, value string) {
		raw, err := json.Marshal(map[string]string{"text": value})
		require.NoError(t, err)
		decoded, err := toolPayload(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: wrap(string(raw))}}})
		require.NoError(t, err)
		var got map[string]string
		require.NoError(t, json.Unmarshal(decoded, &got))
		var want map[string]string
		require.NoError(t, json.Unmarshal(raw, &want))
		assert.Equal(t, want["text"], got["text"], value)
	})
}

func FuzzToolPayloadRejectsInvalidJSON(f *testing.F) {
	f.Add("unknown prose")
	f.Add(`{"valid":true}`)
	f.Add(`<untrusted-user-data-x source="plaud-recording">{}`)
	f.Fuzz(func(t *testing.T, input string) {
		raw, err := toolPayload(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: input}}})
		if err == nil {
			assert.True(t, json.Valid(raw), "successful payload must be valid JSON")
		}
	})
}

func TestOfficialTimezoneLessTimestamp(t *testing.T) {
	file, err := decodeFile([]byte(`{"id":"f","start_at":"2026-09-01 12:00:00"}`))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), file.StartedAt)
}
