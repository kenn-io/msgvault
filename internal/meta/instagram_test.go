package meta

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstagramLinkedAccount(t *testing.T) {
	for _, tt := range []struct {
		name, page, link string
		valid, wrongPage bool
	}{
		{"linked", "100", `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`, true, false},
		{"absent", "100", `{"id":"100"}`, false, false},
		{"different account", "100", `{"id":"100","instagram_business_account":{"id":"201","username":"example_business"}}`, false, false},
		{"different link Page", "100", `{"id":"101","instagram_business_account":{"id":"200"}}`, false, false},
		{"different token Page", "101", ``, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var calls atomic.Int32
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(http.MethodGet, r.Method)
				assert.Equal("Bearer synthetic-page-secret", r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/v26.0/me":
					assert.Equal("id,name,category", r.URL.Query().Get("fields"))
					_, _ = fmt.Fprintf(w, `{"id":%q,"name":"Example Page","category":"Community"}`, tt.page)
				case "/v26.0/100":
					assert.Equal("id,instagram_business_account{id,username}", r.URL.Query().Get("fields"))
					_, _ = fmt.Fprint(w, tt.link)
				default:
					assert.Fail("unexpected Graph request", r.URL.Path)
					http.Error(w, "unexpected fixture route", http.StatusNotFound)
				}
			})
			_, err := c.VerifyPage(context.Background(), "100")
			if tt.wrongPage {
				require.Error(err)
				assert.Equal(int32(1), calls.Load())
				return
			}
			require.NoError(err)
			account, err := c.LinkedInstagramAccount(context.Background(), "100", "200")
			if tt.valid {
				require.NoError(err)
				assert.Equal("200", account.ID)
				assert.Equal("example_business", account.Username)
			} else {
				require.Error(err)
				assert.Empty(account.ID)
			}
			assert.Equal(int32(2), calls.Load())
		})
	}
}

func TestInstagramWireEdges(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var calls atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		q := r.URL.Query()
		assert.Equal("100", q.Get("limit"))
		for _, key := range []string{"order", "filter", "since", "until", "include_inline_create", "access_token"} {
			assert.NotContains(q, key)
		}
		switch r.URL.Path {
		case "/v26.0/200/media":
			assert.Equal("id,caption,timestamp,permalink,owner{id}", q.Get("fields"))
			if q.Get("after") == "next-media" {
				_, _ = fmt.Fprint(w, `{"data":[],"paging":{"cursors":{"after":"last-media"}}}`)
				return
			}
			assert.Equal("opaque+/=", q.Get("after"))
			_, _ = fmt.Fprint(w, `{"data":[{"id":"300","caption":"Synthetic caption","timestamp":"2026-01-02T03:04:05+0000","permalink":"https://www.instagram.com/p/synthetic/","owner":{"id":"200"},"message":"wrong Facebook field"}],"paging":{"cursors":{"after":"next-media"},"next":"https://unexpected.example.test/?access_token=do-not-follow"}}`)
		case "/v26.0/300/comments":
			assert.Equal("id,text,timestamp,from{id,username,self_ig_scoped_id},parent_id", q.Get("fields"))
			assert.Empty(q.Get("after"))
			_, _ = fmt.Fprint(w, `{"data":[{"id":"400","text":"Synthetic comment","timestamp":"2026-01-02T04:00:00+0000","from":{"id":"700","username":"example_commenter","self_ig_scoped_id":"800"},"message":"wrong Facebook field","parent":{"id":"wrong Facebook parent"}}],"paging":{"cursors":{"after":"last-comment"}}}`)
		case "/v26.0/400/replies":
			assert.Equal("id,text,timestamp,from{id,username,self_ig_scoped_id},parent_id", q.Get("fields"))
			assert.Equal("reply+/=", q.Get("after"))
			_, _ = fmt.Fprint(w, `{"data":[{"id":"500","text":"Synthetic reply","timestamp":"2026-01-02T05:00:00+0000","parent_id":"400","from":{"id":"701","username":"example_reply"}}]}`)
		default:
			assert.Fail("unexpected Graph request", r.URL.Path)
			http.Error(w, "unexpected fixture route", http.StatusNotFound)
		}
	})
	ctx := context.Background()
	media, err := c.InstagramMedia(ctx, "200", "opaque+/=")
	require.NoError(err)
	require.Len(media.Data, 1)
	assert.Equal("300", media.Data[0].ID)
	assert.Equal("Synthetic caption", media.Data[0].Caption)
	assert.Equal("2026-01-02T03:04:05+0000", media.Data[0].Timestamp)
	assert.Equal("https://www.instagram.com/p/synthetic/", media.Data[0].Permalink)
	require.NotNil(media.Data[0].Owner)
	assert.Equal("200", media.Data[0].Owner.ID)
	assert.Equal("next-media", media.After)
	last, err := c.InstagramMedia(ctx, "200", media.After)
	require.NoError(err)
	assert.Empty(last.Data)
	assert.Empty(last.After)
	comments, err := c.InstagramComments(ctx, "300", "")
	require.NoError(err)
	require.Len(comments.Data, 1)
	assert.Equal("400", comments.Data[0].ID)
	assert.Equal("Synthetic comment", comments.Data[0].Text)
	assert.Equal("2026-01-02T04:00:00+0000", comments.Data[0].Timestamp)
	assert.Empty(comments.Data[0].ParentID)
	require.NotNil(comments.Data[0].From)
	assert.Equal("700", comments.Data[0].From.ID)
	assert.Equal("800", comments.Data[0].From.SelfIGScopedID)
	assert.Equal("example_commenter", comments.Data[0].From.Username)
	assert.Empty(comments.After)
	replies, err := c.InstagramReplies(ctx, "400", "reply+/=")
	require.NoError(err)
	require.Len(replies.Data, 1)
	assert.Equal("500", replies.Data[0].ID)
	assert.Equal("Synthetic reply", replies.Data[0].Text)
	assert.Equal("2026-01-02T05:00:00+0000", replies.Data[0].Timestamp)
	assert.Equal("400", replies.Data[0].ParentID)
	require.NotNil(replies.Data[0].From)
	assert.Equal("701", replies.Data[0].From.ID)
	assert.Equal("example_reply", replies.Data[0].From.Username)
	assert.Empty(replies.After)
	assert.Equal(int32(4), calls.Load())
}
