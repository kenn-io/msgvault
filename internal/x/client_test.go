package x

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rootJSON = `{"id":"9007199254740993","author_id":"1001","conversation_id":"9007199254740993","text":"Example post","created_at":"2026-10-29T12:00:00Z"}`

func servePage(t *testing.T, body string) (*Client, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "author_id", r.URL.Query().Get("expansions"))
		assert.NotEmpty(t, r.URL.Query().Get("tweet.fields"))
		_, err := fmt.Fprint(w, body)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, srv.Client()), calls
}

func TestClientDecodesAliasesWithoutFallback(t *testing.T) {
	for _, spelling := range []string{"tweet", "post"} {
		t.Run(spelling, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			post := strings.TrimSuffix(rootJSON, "}") + fmt.Sprintf(`,"edit_history_%s_ids":["9007199254740993"],"referenced_%ss":[{"type":"quoted","id":"91"}],"note_%s":{"text":"Full example"}}`, spelling, spelling, spelling)
			client, calls := servePage(t, `{"data":[`+post+`]}`)
			page, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			_testRequire.NoError(err)
			_testRequire.Len(page.Posts, 1)
			_testAssert.Equal("9007199254740993", page.Posts[0].ID)
			_testAssert.Equal("Full example", page.Posts[0].LongText)
			_testAssert.Equal("quoted", page.Posts[0].References[0].Type)
			_testAssert.Equal(int64(1), calls.Load())
		})
	}
	cases := map[string]string{
		"conflicting history":       strings.TrimSuffix(rootJSON, "}") + `,"edit_history_tweet_ids":["9007199254740993"],"edit_history_post_ids":["9","9007199254740993"]}`,
		"conflicting reference":     strings.TrimSuffix(rootJSON, "}") + `,"referenced_tweets":[{"type":"quoted","id":"91"}],"referenced_posts":[{"type":"quoted","id":"92"}]}`,
		"conflicting long text":     strings.TrimSuffix(rootJSON, "}") + `,"note_tweet":{"text":"one"},"note_post":{"text":"two"}}`,
		"missing author":            strings.ReplaceAll(rootJSON, `"author_id":"1001",`, ""),
		"missing conversation":      strings.ReplaceAll(rootJSON, `"conversation_id":"9007199254740993",`, ""),
		"missing time":              strings.ReplaceAll(rootJSON, `,"created_at":"2026-10-29T12:00:00Z"`, ""),
		"parent absent":             strings.TrimSuffix(rootJSON, "}") + `,"referenced_tweets":[{"type":"replied_to"}]}`,
		"reply user without parent": strings.TrimSuffix(rootJSON, "}") + `,"in_reply_to_user_id":"1002"}`,
		"multiple parents":          strings.TrimSuffix(rootJSON, "}") + `,"referenced_tweets":[{"type":"replied_to","id":"91"},{"type":"replied_to","id":"92"}]}`,
		"history missing returned":  strings.TrimSuffix(rootJSON, "}") + `,"edit_history_tweet_ids":["91"]}`,
	}
	for name, post := range cases {
		t.Run(name, func(t *testing.T) {
			client, calls := servePage(t, `{"data":[`+rootJSON+`,`+post+`]}`)
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			require.Error(t, err)
			assert.Equal(t, int64(1), calls.Load())
		})
	}
	for _, id := range []string{"", "0", "01", "-1", "１２", strings.Repeat("9", 129)} {
		t.Run("id_"+id, func(t *testing.T) {
			client, _ := servePage(t, `{"data":[`+strings.ReplaceAll(rootJSON, "9007199254740993", id)+`]}`)
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			require.Error(t, err)
		})
	}
	t.Run("128 digit ID", func(t *testing.T) {
		_testAssert := assert.New(t)
		_testRequire := require.New(t)

		id := strings.Repeat("9", 128)
		client, _ := servePage(t, `{"data":[`+strings.ReplaceAll(rootJSON, "9007199254740993", id)+`]}`)
		page, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
		_testRequire.NoError(err)
		_testRequire.Len(page.Posts, 1)
		_testAssert.Equal(id, page.Posts[0].ID)
		_testAssert.Positive(CompareID(id, "9007199254740993"))
		_testAssert.Positive(CompareID("10", "9"))
	})
	for name, body := range map[string]string{
		"malformed": "{",
		"oversized": `{"data":[],"padding":"` + strings.Repeat("x", 8<<20) + `"}`,
		"trailing":  `{"data":[` + rootJSON + `]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := servePage(t, body)
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			require.Error(t, err)
		})
	}
}

func TestClientRequestContracts(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	ctx := t.Context()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		switch r.URL.Path {
		case "/users/me":
			assert.Equal(t, http.MethodGet, r.Method)
			_, _ = fmt.Fprint(w, `{"data":{"id":"1001","name":"Example Author","username":"example_author"}}`)
		case "/users/1001/mentions":
			assert.Equal(t, "9", q.Get("since_id"))
			assert.Equal(t, "page-2", q.Get("pagination_token"))
			assert.Equal(t, "5", q.Get("max_results"))
			assert.Equal(t, "2026-10-29T13:00:00Z", q.Get("end_time"))
			assert.Empty(t, q.Get("exclude")) // Owner replies must remain eligible.
			_, _ = fmt.Fprint(w, `{"data":[],"meta":{"next_token":"page-3"},"errors":[{"resource_type":"tweet","resource_id":"91","title":"Temporary error"}]}`)
		case "/tweets/search/all":
			assert.Equal(t, "recency", q.Get("sort_order"))
			assert.Equal(t, "from:example_author -is:retweet", q.Get("query"))
			assert.Equal(t, "archive-2", q.Get("next_token"))
			assert.Equal(t, "10", q.Get("max_results"))
			assert.Empty(t, q.Get("since_id"))
			_, _ = fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
		case "/tweets":
			if r.Method == http.MethodGet {
				assert.Equal(t, "9007199254740993,91", q.Get("ids"))
				_, _ = fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			} else {
				assert.Equal(t, http.MethodPost, r.Method)
				b, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var got map[string]any
				if !assert.NoError(t, json.Unmarshal(b, &got)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, map[string]any{"text": "Thanks, example!", "reply": map[string]any{"in_reply_to_tweet_id": "91"}}, got)
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprint(w, `{"data":{"id":"9001"}}`)
			}
		default:
			assert.Fail(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()
	client := NewClient(srv.URL, srv.Client())
	me, err := client.Me(ctx)
	_testRequire.NoError(err)
	_testAssert.Equal("1001", me.ID)
	before := time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)
	page, err := client.Timeline(ctx, TimelineRequest{OwnerID: "1001", Mentions: true, SinceID: "9", Before: before, Token: "page-2", MaxResults: 5})
	_testRequire.NoError(err)
	_testAssert.Empty(page.Posts)
	_testAssert.Equal("page-3", page.NextToken)
	_testRequire.Len(page.Errors, 1)
	_testAssert.Equal("91", page.Errors[0].ID)
	_testAssert.False(page.Errors[0].Unavailable)
	_, err = client.SearchAll(ctx, SearchRequest{Query: "from:example_author -is:retweet", Before: before, Token: "archive-2", MaxResults: 10})
	_testRequire.NoError(err)
	_, err = client.Lookup(ctx, []string{"9007199254740993", "91"})
	_testRequire.NoError(err)
	remote, err := client.CreateReply(ctx, "Thanks, example!", "91")
	_testRequire.NoError(err)
	_testAssert.Equal("9001", remote)
	_testAssert.Equal(int64(5), calls.Load())
}

func TestClientRejectsInvalidRequestsBeforeHTTP(t *testing.T) {
	_testRequire := require.New(t)

	client, calls := servePage(t, `{"data":[]}`)
	now := time.Now()
	for _, r := range []TimelineRequest{{OwnerID: "01", MaxResults: 5}, {OwnerID: "1001", MaxResults: 4}, {OwnerID: "1001", MaxResults: 101}, {OwnerID: "1001", SinceID: "9", After: now, MaxResults: 5}, {OwnerID: "1001", After: now, Before: now, MaxResults: 5}, {OwnerID: "1001", SinceID: "-9", MaxResults: 5}} {
		_, err := client.Timeline(t.Context(), r)
		_testRequire.Error(err)
	}
	for _, r := range []SearchRequest{{Query: "query", MaxResults: 9}, {Query: "query", MaxResults: 501}, {MaxResults: 10}, {Query: "query", After: now, Before: now, MaxResults: 10}} {
		_, err := client.SearchAll(t.Context(), r)
		_testRequire.Error(err)
	}
	tooManyUniqueIDs := make([]string, 101)
	for i := range tooManyUniqueIDs {
		tooManyUniqueIDs[i] = strconv.Itoa(i + 1)
	}
	for _, ids := range [][]string{nil, {"91", "91"}, {"01"}, tooManyUniqueIDs} {
		_, err := client.Lookup(t.Context(), ids)
		_testRequire.Error(err)
	}
	assert.Zero(t, calls.Load())
}

func TestClientSingleRequestErrorsAndRedirects(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429, 500, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "9")
				w.Header().Set("X-Rate-Limit-Reset", "1793304000")
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"title":"Never expose this provider text or secret"}`)
			}))
			defer srv.Close()
			client := NewClient(srv.URL, srv.Client())
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			var apiErr *APIError
			_testRequire.ErrorAs(err, &apiErr)
			_testAssert.Equal(status, apiErr.StatusCode)
			_testAssert.NotContains(err.Error(), "secret")
			_testAssert.Equal("9", apiErr.RetryAfter)
			_testAssert.Equal(status >= 400 && status < 500, apiErr.Definite)
			_, err = client.CreateReply(t.Context(), "Example", "91")
			_testRequire.Error(err)
			_testAssert.Equal(int64(2), calls.Load())
		})
	}
	t.Run("malformed success", func(t *testing.T) {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"data":{"id":"01"}}`)
		}))
		defer srv.Close()
		_, err := NewClient(srv.URL, srv.Client()).CreateReply(t.Context(), "Example", "91")
		require.ErrorIs(t, err, ErrDecodeResponse)
		assert.Equal(t, int64(1), calls.Load())
	})
}

func TestClientRejectsUnadmittedResourceShape(t *testing.T) {
	for name, body := range map[string]string{
		"posts over limit":     `{"data":[` + strings.TrimSuffix(strings.Repeat(rootJSON+",", 6), ",") + `]}`,
		"unrelated author":     `{"data":[` + rootJSON + `],"includes":{"users":[{"id":"1002","username":"other"}]}}`,
		"duplicate author":     `{"data":[` + rootJSON + `],"includes":{"users":[{"id":"1001","username":"example"},{"id":"1001","username":"example"}]}}`,
		"unexpected expansion": `{"data":[` + rootJSON + `],"includes":{"tweets":[` + rootJSON + `]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := servePage(t, body)
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			require.ErrorIs(t, err, ErrResponseShape)
		})
	}
}

func TestClientAcceptsAgreeingAliasFixture(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	body, err := os.ReadFile("testdata/wire_aliases.json")
	_testRequire.NoError(err)
	client, _ := servePage(t, string(body))
	page, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
	_testRequire.NoError(err)
	_testRequire.Len(page.Posts, 1)
	_testAssert.Equal("9007199254740993", page.Posts[0].ID)
	_testAssert.Equal(1, page.UserCount)
}

func TestClientRejectsMissingPageShape(t *testing.T) {
	for name, body := range map[string]string{"null": "null", "missing data": "{}", "null data": `{"data":null}`, "duplicate key": `{"data":[],"data":[]}`} {
		t.Run(name, func(t *testing.T) {
			client, _ := servePage(t, body)
			_, err := client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
			require.ErrorIs(t, err, ErrDecodeResponse)
		})
	}
}
