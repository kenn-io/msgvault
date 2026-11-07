package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type expireTokenAfterFirstEligibility struct {
	*Client

	expired bool
}

func (c *expireTokenAfterFirstEligibility) CheckReply(ctx context.Context, target ReplyTarget) error {
	if err := c.Client.CheckReply(ctx, target); err != nil {
		return err
	}
	if !c.expired {
		c.mu.Lock()
		c.expires = time.Time{}
		c.mu.Unlock()
		c.expired = true
	}
	return nil
}

func TestReplyClientRechecksChannelParentAndPostsOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var inserts atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/channels":
			assert.Equal("true", r.URL.Query().Get("mine"))
			_, _ = fmt.Fprint(w, `{"items":[{"id":"owner-1"}]}`)
		case "/commentThreads":
			assert.Equal("video-1", r.URL.Query().Get("videoId"))
			assert.Equal("published", r.URL.Query().Get("moderationStatus"))
			assert.Empty(r.URL.Query().Get("id"))
			_, _ = fmt.Fprint(w, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":true,"topLevelComment":{"id":"parent-1","snippet":{"moderationStatus":"published"}}}}]}`)
		case "/comments":
			if !assert.Equal(http.MethodPost, r.Method) {
				return
			}
			assert.Equal("application/json", r.Header.Get("Content-Type"))
			var payload struct {
				Snippet struct {
					ParentID string `json:"parentId"`
					Text     string `json:"textOriginal"`
				} `json:"snippet"`
			}
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
				return
			}
			assert.Equal("parent-1", payload.Snippet.ParentID)
			assert.Equal("Example reply", payload.Snippet.Text)
			inserts.Add(1)
			_, _ = fmt.Fprint(w, `{"id":"sent-1","snippet":{"parentId":"parent-1","videoId":"video-1","textOriginal":"Example reply","authorChannelId":{"value":"owner-1"}}}`)
		default:
			assert.Fail("unexpected provider request", r.URL.String())
		}
	})
	target := ReplyTarget{ChannelID: "owner-1", VideoID: "video-1", ParentID: "parent-1", ThreadID: "thread-1"}
	require.NoError(c.CheckReply(t.Context(), target))
	comment, err := c.InsertReply(t.Context(), "parent-1", "Example reply")
	require.NoError(err)
	assert.Equal("sent-1", comment.ID)
	assert.Equal(int32(1), inserts.Load())
}

func TestReplyTokenRefreshFailureBeforeCommentDispatchCanRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	service, _, target, _ := replyFixture(t)
	var client *Client
	var tokenRequests atomic.Int32
	var posts atomic.Int32
	client = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if tokenRequests.Add(1) == 2 {
				http.Error(w, "temporary refresh failure", http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/channels":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"owner-1"}]}`)
		case "/commentThreads":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":true,"topLevelComment":{"id":"parent-1","snippet":{"moderationStatus":"published"}}}}]}`)
		case "/comments":
			posts.Add(1)
			_, _ = fmt.Fprint(w, `{"id":"sent-1","snippet":{"parentId":"parent-1","videoId":"video-1","textOriginal":"Example reply","authorChannelId":{"value":"owner-1"}}}`)
		default:
			assert.Fail("unexpected provider request", r.URL.String())
		}
	})
	service.Provider = &expireTokenAfterFirstEligibility{Client: client}

	preview, err := service.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	failed, err := service.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, errAPIRequestNotDispatched)
	require.NotNil(failed)
	assert.Equal("rejected", failed.State)
	assert.Equal(int32(2), tokenRequests.Load())
	assert.Zero(posts.Load())

	retryPreview, err := service.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	assert.NotEqual(preview.Token, retryPreview.Token)
	_, err = service.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.Error(err)
	assert.Zero(posts.Load())

	retried, err := service.Confirm(t.Context(), target, "Example reply", retryPreview.Token)
	require.NoError(err)
	assert.Equal("archived", retried.State)
	assert.Equal(int32(3), tokenRequests.Load())
	assert.Equal(int32(1), posts.Load())
}

func TestReplyClientRejectsChangedChannelAndUnknownEligibility(t *testing.T) {
	for _, fixture := range []struct{ name, channel, thread string }{
		{"channel changed", `{"items":[{"id":"other-owner"}]}`, `{"items":[]}`},
		{"held parent", `{"items":[{"id":"owner-1"}]}`, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":true,"topLevelComment":{"id":"parent-1","snippet":{"moderationStatus":"heldForReview"}}}}]}`},
		{"unknown status", `{"items":[{"id":"owner-1"}]}`, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":true,"topLevelComment":{"id":"parent-1","snippet":{}}}}]}`},
		{"cannot reply", `{"items":[{"id":"owner-1"}]}`, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":false,"topLevelComment":{"id":"parent-1","snippet":{"moderationStatus":"published"}}}}]}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
				case "/channels":
					_, _ = fmt.Fprint(w, fixture.channel)
				case "/commentThreads":
					_, _ = fmt.Fprint(w, fixture.thread)
				default:
					assert.Fail(t, "unexpected mutation")
				}
			})
			require.ErrorIs(t, c.CheckReply(t.Context(), ReplyTarget{ChannelID: "owner-1", VideoID: "video-1", ParentID: "parent-1", ThreadID: "thread-1"}), ErrReplyRejected)
		})
	}
}

func TestReplyClientRejectsRepeatedEligibilityResultsPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	pageCalls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/channels":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"owner-1"}]}`)
		case "/commentThreads":
			pageCalls++
			if pageCalls == 2 {
				assert.Equal("repeat", r.URL.Query().Get("pageToken"))
			}
			_, _ = fmt.Fprint(w, `{"items":[],"nextPageToken":"repeat"}`)
		default:
			assert.Fail("unexpected provider request", r.URL.String())
		}
	})
	target := ReplyTarget{ChannelID: "owner-1", VideoID: "video-1", ParentID: "parent-1", ThreadID: "thread-1"}
	err := c.CheckReply(t.Context(), target)
	require.EqualError(err, "YouTube repeated a results page; nothing sent")
	assert.Equal(2, pageCalls)
}

func TestReplyClientNeverRetriesPostAndRedactsResponse(t *testing.T) {
	for _, status := range []int{400, 403, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
					return
				}
				calls++
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"error":{"message":"fixture-secret"}}`)
			})
			_, err := c.InsertReply(t.Context(), "parent-1", "Example reply")
			require.Error(err)
			assert.NotContains(err.Error(), "fixture-secret")
			assert.Equal(1, calls)
			if status < 500 {
				require.ErrorIs(err, ErrReplyRejected)
			} else {
				assert.NotErrorIs(err, ErrReplyRejected)
			}
		})
	}
}

// A real HTTP disconnect after acceptance must not cause another insert.
func TestReplyHTTPAcceptedThenDisconnectedRemainsBlocked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	service, _, target, archived := replyFixture(t)
	var posts atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
		case "/channels":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"owner-1"}]}`)
		case "/commentThreads":
			_, _ = fmt.Fprint(w, `{"items":[{"id":"thread-1","snippet":{"videoId":"video-1","canReply":true,"topLevelComment":{"id":"parent-1","snippet":{"moderationStatus":"published"}}}}]}`)
		case "/comments":
			posts.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(ok) {
				return
			}
			conn, _, err := hijacker.Hijack()
			if !assert.NoError(err) {
				return
			}
			assert.NoError(conn.Close())
		default:
			assert.Fail("unexpected request", r.URL.Path)
		}
	})
	service.Provider = client
	preview, err := service.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	_, err = service.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	_, err = service.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	_, err = service.Preview(t.Context(), target, "Example reply")
	require.ErrorIs(err, ErrReplyUncertain)
	assert.Equal(int32(1), posts.Load())
	assert.Zero(archived.Load())
}

func TestReplyClientExplicitEvidenceRequiresOneMatchingComment(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `{"items":[{"id":"different"}]}`, `{"items":[{"id":"sent-1","snippet":{"parentId":"parent-1"}}]}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
				return
			}
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "sent-1", r.URL.Query().Get("id"))
			_, _ = fmt.Fprint(w, body)
		})
		result, err := c.GetReply(t.Context(), "sent-1")
		if strings.Contains(body, `"id":"sent-1"`) {
			require.NoError(t, err)
			assert.Equal(t, "sent-1", result.ID)
		} else {
			require.Error(t, err)
		}
	}
}
