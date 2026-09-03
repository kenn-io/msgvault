package notionmeetings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientQueryMeetingNotesUsesDocumentedReadContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v1/blocks/meeting_notes/query", r.URL.Path)
		assert.Equal("Bearer ntn_test_secret", r.Header.Get("Authorization"))
		assert.Equal(APIVersion, r.Header.Get("Notion-Version"))
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.JSONEq(`{
			"sort":[{"property":"created_time","direction":"descending"}],
			"limit":50
		}`, string(body))
		_, err = io.WriteString(w, `{
			"results":[{
				"object":"block",
				"id":"meeting-1",
				"type":"meeting_notes",
				"meeting_notes":{
					"title":[{"plain_text":"Weekly planning"}],
					"status":"notes_ready",
					"children":{"summary_block_id":"summary-1","transcript_block_id":"transcript-1"},
					"calendar_event":{"start_time":"2026-08-29T10:00:00Z","end_time":"2026-08-29T10:30:00Z","attendees":["user-1"]}
				},
				"created_time":"2026-08-29T09:59:00Z",
				"last_edited_time":"2026-08-29T10:35:00Z",
				"created_by":{"id":"creator-1","object":"user"},
				"parent":{"type":"page_id","page_id":"page-1"},
				"has_children":true,
				"future_field":{"kept":true}
			}],
			"has_more":true
		}`)
		assert.NoError(err)
	}))
	defer server.Close()

	client := NewClient(server.URL, "ntn_test_secret")
	result, err := client.QueryMeetingNotes(context.Background(), 50)
	require.NoError(err)
	require.Len(result.Results, 1)
	assert.True(result.HasMore)
	assert.Equal("meeting-1", result.Results[0].ID)
	assert.Equal("Weekly planning", result.Results[0].Title())
	assert.Equal("page-1", result.Results[0].Parent.PageID)
	assert.Equal("summary-1", result.Results[0].MeetingNotes.Children.SummaryBlockID)
	assert.JSONEq(`{"kept":true}`, string(result.Results[0].Extra["future_field"]))
	assert.Contains(string(result.Results[0].Raw), `"future_field"`)
}

func TestClientReadEndpointsPreserveRawResponses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(APIVersion, r.Header.Get("Notion-Version"))
		assert.Equal("Bearer ntn_test", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/blocks/block-1":
			assert.Equal(http.MethodGet, r.Method)
			_, _ = io.WriteString(w, `{"object":"block","id":"block-1","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"Hello"}]}}`)
		case "/v1/blocks/block-1/children":
			assert.Equal("cursor-1", r.URL.Query().Get("start_cursor"))
			assert.Equal("100", r.URL.Query().Get("page_size"))
			_, _ = io.WriteString(w, `{"object":"list","results":[{"object":"block","id":"child-1","type":"paragraph","has_children":false,"paragraph":{}}],"next_cursor":"cursor-2","has_more":true}`)
		case "/v1/pages/page-1/markdown":
			assert.Equal("true", r.URL.Query().Get("include_transcript"))
			_, _ = io.WriteString(w, `{"object":"page_markdown","id":"page-1","markdown":"# Weekly planning\nTranscript: Ready","truncated":false,"unknown_block_ids":[]}`)
		case "/v1/users":
			assert.Equal("users-1", r.URL.Query().Get("start_cursor"))
			assert.Equal("100", r.URL.Query().Get("page_size"))
			_, _ = io.WriteString(w, `{"object":"list","results":[{"object":"user","id":"user-1","name":"Test User","type":"person","person":{"email":"user@example.com","email_verified":true}}],"next_cursor":null,"has_more":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "ntn_test")
	block, err := client.RetrieveBlock(context.Background(), "block-1")
	require.NoError(err)
	assert.Equal("Hello", block.PlainText())
	assert.Contains(string(block.Raw), `"paragraph"`)

	children, err := client.RetrieveBlockChildren(context.Background(), "block-1", "cursor-1")
	require.NoError(err)
	require.Len(children.Results, 1)
	assert.Equal("cursor-2", children.NextCursor)
	assert.Contains(string(children.Raw), `"child-1"`)

	markdown, err := client.RetrievePageMarkdown(context.Background(), "page-1", true)
	require.NoError(err)
	assert.Contains(markdown.Markdown, "Transcript: Ready")
	assert.Contains(string(markdown.Raw), `"page_markdown"`)

	users, err := client.ListUsers(context.Background(), "users-1")
	require.NoError(err)
	require.Len(users.Results, 1)
	assert.Equal("user@example.com", users.Results[0].Person.Email)
	assert.True(users.Results[0].Person.EmailVerified)
}

func TestBlockPlainTextExtractsContentBearingPayloads(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "heading 4", raw: `{"type":"heading_4","heading_4":{"rich_text":[{"plain_text":"Decisions"}]}}`, want: "Decisions"},
		{name: "code and caption", raw: `{"type":"code","code":{"rich_text":[{"plain_text":"ship()"}],"caption":[{"plain_text":"Example"}]}}`, want: "ship()\nExample"},
		{name: "media caption", raw: `{"type":"image","image":{"caption":[{"plain_text":"Architecture diagram"}]}}`, want: "Architecture diagram"},
		{name: "child title", raw: `{"type":"child_page","child_page":{"title":"Project notes"}}`, want: "Project notes"},
		{name: "equation", raw: `{"type":"equation","equation":{"expression":"e=mc^2"}}`, want: "e=mc^2"},
		{name: "table row", raw: `{"type":"table_row","table_row":{"cells":[[{"plain_text":"Owner"}],[{"plain_text":"Status"}]]}}`, want: "Owner\nStatus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var block Block
			require.NoError(t, json.Unmarshal([]byte(tt.raw), &block))
			assert.Equal(t, tt.want, block.PlainText())
		})
	}
}

func TestClientClassifiesAndSanitizesProviderErrors(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		status    int
		code      string
		wantError error
	}{
		{name: "unauthorized", path: "/query", status: http.StatusUnauthorized, code: "unauthorized", wantError: ErrUnauthorized},
		{name: "meeting access", path: "/query", status: http.StatusForbidden, code: "restricted_resource", wantError: ErrMeetingAccess},
		{name: "read content", path: "/block", status: http.StatusForbidden, code: "restricted_resource", wantError: ErrReadContent},
		{name: "user information", path: "/users", status: http.StatusForbidden, code: "restricted_resource", wantError: ErrUserInformation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"object":"error","code":"`+tc.code+`","message":"private meeting title ntn_test_secret"}`)
			}))
			defer server.Close()
			client := NewClient(server.URL, "ntn_test_secret")

			var err error
			switch tc.path {
			case "/query":
				_, err = client.QueryMeetingNotes(context.Background(), 1)
			case "/block":
				_, err = client.RetrieveBlock(context.Background(), "block-1")
			case "/users":
				_, err = client.ListUsers(context.Background(), "")
			}
			require.Error(err)
			require.ErrorIs(err, tc.wantError)
			assert.NotContains(err.Error(), "private meeting title")
			assert.NotContains(err.Error(), "ntn_test_secret")
		})
	}
}

func TestClientRetriesRateLimitThenReturnsQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"object":"error","code":"rate_limited","message":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[],"has_more":false}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "ntn_test")
	var waits []time.Duration
	client.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		return nil
	}
	result, err := client.QueryMeetingNotes(context.Background(), 50)
	require.NoError(err)
	assert.Empty(result.Results)
	assert.Equal(3, attempts)
	require.Len(waits, 2)
	for _, wait := range waits {
		assert.LessOrEqual(wait, maxRetryAfter)
	}
}

func TestClientRejectsMalformedSuccessWithoutLeakingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[private meeting content]}`)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "ntn_test").QueryMeetingNotes(context.Background(), 50)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMalformedResponse)
	assert.NotContains(t, err.Error(), "private meeting content")
}

func TestClientQueryMeetingNotesRejectsMalformedListEnvelopes(t *testing.T) {
	const responseSentinel = "private meeting response sentinel"
	tests := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "null", body: `null`},
		{name: "missing has more", body: `{"results":[],"private":"` + responseSentinel + `"}`},
		{name: "missing results", body: `{"has_more":false}`},
		{name: "null results", body: `{"results":null,"has_more":false}`},
		{name: "null has more", body: `{"results":[],"has_more":null}`},
		{name: "wrong results type", body: `{"results":{},"has_more":false}`},
		{name: "wrong has more type", body: `{"results":[],"has_more":"false"}`},
		{name: "empty body", body: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newStaticNotionClient(t, tc.body)

			result, err := client.QueryMeetingNotes(context.Background(), 50)

			require.ErrorIs(t, err, ErrMalformedResponse)
			assert.Nil(t, result)
			assert.NotContains(t, err.Error(), responseSentinel)
		})
	}
}

func TestClientQueryMeetingNotesAcceptsCompleteListEnvelopes(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantResult int
	}{
		{
			name: "empty page without next cursor",
			body: `{"results":[],"has_more":false}`,
		},
		{
			name: "canonical page with null next cursor",
			body: `{
				"object":"list",
				"results":[{"object":"block","id":"meeting-1","type":"meeting_notes","has_children":false,"meeting_notes":{"title":[]}}],
				"next_cursor":null,
				"has_more":false
			}`,
			wantResult: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newStaticNotionClient(t, tc.body)

			result, err := client.QueryMeetingNotes(context.Background(), 50)

			require.NoError(t, err)
			assert.Len(t, result.Results, tc.wantResult)
		})
	}
}

func TestClientQueryMeetingNotesRejectsNonMeetingBlocks(t *testing.T) {
	tests := []struct {
		name  string
		block string
	}{
		{
			name:  "wrong object",
			block: `{"object":"page","id":"meeting-1","type":"meeting_notes","has_children":false,"meeting_notes":{}}`,
		},
		{
			name:  "wrong block type",
			block: `{"object":"block","id":"meeting-1","type":"paragraph","has_children":false,"paragraph":{}}`,
		},
		{
			name:  "missing required block field",
			block: `{"object":"block","id":"meeting-1","type":"meeting_notes","meeting_notes":{}}`,
		},
		{
			name:  "missing meeting payload",
			block: `{"object":"block","id":"meeting-1","type":"meeting_notes","has_children":false}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newStaticNotionClient(t, `{"results":[`+tt.block+`],"has_more":false}`)

			result, err := client.QueryMeetingNotes(context.Background(), 50)

			require.ErrorIs(t, err, ErrMalformedResponse)
			assert.Nil(t, result)
		})
	}
}

func TestClientOtherListEndpointsRejectMalformedEnvelopes(t *testing.T) {
	t.Run("block children", func(t *testing.T) {
		client := newStaticNotionClient(t, `{}`)

		result, err := client.RetrieveBlockChildren(context.Background(), "block-1", "")

		require.ErrorIs(t, err, ErrMalformedResponse)
		assert.Nil(t, result)
	})

	t.Run("users", func(t *testing.T) {
		client := newStaticNotionClient(t, `null`)

		result, err := client.ListUsers(context.Background(), "")

		require.ErrorIs(t, err, ErrMalformedResponse)
		assert.Nil(t, result)
	})
}

func TestClientOtherListEndpointsAllowOptionalNextCursor(t *testing.T) {
	t.Run("block children without next cursor", func(t *testing.T) {
		client := newStaticNotionClient(t, `{"results":[],"has_more":false}`)

		result, err := client.RetrieveBlockChildren(context.Background(), "block-1", "")

		require.NoError(t, err)
		assert.Empty(t, result.NextCursor)
	})

	t.Run("users with null next cursor", func(t *testing.T) {
		client := newStaticNotionClient(t, `{"results":[],"next_cursor":null,"has_more":false}`)

		result, err := client.ListUsers(context.Background(), "")

		require.NoError(t, err)
		assert.Empty(t, result.NextCursor)
	})
}

func TestClientRejectsMalformedBlockResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing fields", body: `{}`},
		{name: "wrong ID", body: `{"object":"block","id":"other","type":"paragraph","has_children":false,"paragraph":{}}`},
		{name: "missing child flag", body: `{"object":"block","id":"block-1","type":"paragraph","paragraph":{}}`},
		{name: "missing type payload", body: `{"object":"block","id":"block-1","type":"paragraph","has_children":false}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := newStaticNotionClient(t, tc.body).RetrieveBlock(context.Background(), "block-1")

			require.ErrorIs(t, err, ErrMalformedResponse)
			assert.Nil(t, result)
		})
	}
}

func TestClientRejectsMalformedBlockChildren(t *testing.T) {
	client := newStaticNotionClient(t, `{
		"results":[{"object":"block","id":"child-1","type":"paragraph","has_children":false}],
		"has_more":false
	}`)

	result, err := client.RetrieveBlockChildren(context.Background(), "block-1", "")

	require.ErrorIs(t, err, ErrMalformedResponse)
	assert.Nil(t, result)
}

func TestClientRejectsMalformedPageMarkdownResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing fields", body: `{}`},
		{name: "wrong ID", body: `{"object":"page_markdown","id":"other","markdown":"","truncated":false,"unknown_block_ids":[]}`},
		{name: "missing Markdown", body: `{"object":"page_markdown","id":"page-1","truncated":false,"unknown_block_ids":[]}`},
		{name: "missing completeness fields", body: `{"object":"page_markdown","id":"page-1","markdown":""}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := newStaticNotionClient(t, tc.body).RetrievePageMarkdown(context.Background(), "page-1", true)

			require.ErrorIs(t, err, ErrMalformedResponse)
			assert.Nil(t, result)
		})
	}
}

func newStaticNotionClient(t *testing.T, body string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, body)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return NewClient(server.URL, "ntn_test")
}

func TestAPIErrorSupportsErrorsIs(t *testing.T) {
	err := &APIError{Kind: ErrMeetingAccess, Status: http.StatusForbidden, Code: "restricted_resource"}
	require.ErrorIs(t, err, ErrMeetingAccess)
	encoded, marshalErr := json.Marshal(err)
	require.NoError(t, marshalErr)
	assert.NotContains(t, string(encoded), "token")
}
