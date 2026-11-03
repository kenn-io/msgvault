package meta

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mentionIG = "17841400000000001"
const mentionMedia = "17900000000000001"
const mentionComment = "17800000000000001"

func TestMentionedMediaRequestShape(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/17841400000000001", r.URL.Path)
		assert.Equal(t, "mentioned_media.media_id(17900000000000001){id,caption,media_type,timestamp,username,owner{id}}", r.URL.Query().Get("fields"))
		assert.Equal(t, "Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		assert.False(t, r.URL.Query().Has("access_token"))
		_, _ = fmt.Fprint(w, `{"id":"17841400000000001","mentioned_media":{"id":"17900000000000001","caption":"hi acct","media_type":"IMAGE","timestamp":"2026-10-30T10:00:00+0000","username":"example_user"}}`)
	})
	m, err := c.MentionedMedia(t.Context(), mentionIG, mentionMedia)
	require.NoError(t, err)
	assert.Equal(t, IGMentionMedia{ID: mentionMedia, Caption: "hi acct", MediaType: "IMAGE", Timestamp: "2026-10-30T10:00:00+0000", Username: "example_user"}, m)
}

func TestMentionedCommentRequestShape(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/17841400000000001", r.URL.Path)
		assert.Equal(t, "mentioned_comment.comment_id(17800000000000001){id,text,timestamp,media{id,caption,media_type,timestamp,username,owner{id}}}", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"17841400000000001","mentioned_comment":{"id":"17800000000000001","text":"hello acct","timestamp":"2026-10-30T10:00:00Z","media":{"id":"17900000000000001"}}}`)
	})
	m, err := c.MentionedComment(t.Context(), mentionIG, mentionComment)
	requirements.NoError(err)
	requirements.NotNil(m.Media)
	assertions.Equal(mentionMedia, m.Media.ID)
	assertions.Equal("hello acct", m.Text)
}

func TestMentionReadsRefuseNonDecimalIDs(t *testing.T) {
	requirements := require.New(t)
	c := fixtureClient(t, func(http.ResponseWriter, *http.Request) { assert.Fail(t, "invalid ID reached provider") })
	for _, id := range []string{"1)", "1{x}", "1,2", "abc", "", "0"} {
		_, err := c.MentionedMedia(t.Context(), mentionIG, id)
		requirements.Error(err)
		_, err = c.MentionedComment(t.Context(), mentionIG, id)
		requirements.Error(err)
		_, err = c.MentionedMedia(t.Context(), id, mentionMedia)
		requirements.Error(err)
		_, err = c.MentionedComment(t.Context(), id, mentionComment)
		requirements.Error(err)
		_, err = c.Tags(t.Context(), id, "")
		requirements.Error(err)
	}
}

func TestMentionReadsRefuseIdentityMismatch(t *testing.T) {
	for _, body := range []string{
		`{"id":"99","mentioned_media":{"id":"17900000000000001"}}`,
		`{"mentioned_media":{"id":"99"}}`,
		`{"mentioned_media":{"id":"17900000000000001","owner":{"id":"bad"}}}`,
		`{"mentioned_comment":{"id":"99","media":{"id":"17900000000000001"}}}`,
		`{"mentioned_comment":{"id":"17800000000000001"}}`,
		`{"mentioned_comment":{"id":"17800000000000001","media":{"id":"bad"}}}`,
		`{"mentioned_comment":{"id":"17800000000000001","media":{"id":"17900000000000001","owner":{"id":"bad"}}}}`,
		`{"id":"99","mentioned_comment":{"id":"17800000000000001","media":{"id":"17900000000000001"}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
			if strings.Contains(body, "mentioned_comment") {
				_, err := c.MentionedComment(t.Context(), mentionIG, mentionComment)
				require.Error(t, err)
			} else {
				_, err := c.MentionedMedia(t.Context(), mentionIG, mentionMedia)
				require.Error(t, err)
			}
		})
	}
}

func TestTagsPagingWithoutNext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/17841400000000001/tags", r.URL.Path)
		assert.Equal(t, "id,caption,media_type,timestamp,username,owner{id}", r.URL.Query().Get("fields"))
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		if r.URL.Query().Get("after") == "" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"17900000000000001"},{"id":"17900000000000002"}],"paging":{"cursors":{"after":"c1"}}}`)
		} else {
			assert.Equal(t, "c1", r.URL.Query().Get("after"))
			_, _ = fmt.Fprint(w, `{"data":[],"paging":{"cursors":{"after":"c1"}}}`)
		}
	})
	p, err := c.Tags(t.Context(), mentionIG, "")
	requirements.NoError(err)
	assertions.Len(p.Data, 2)
	assertions.Equal("c1", p.After)
	p, err = c.Tags(t.Context(), mentionIG, p.After)
	requirements.NoError(err)
	assertions.Empty(p.After)
}

func TestTagsNonAdvancingCursorRefused(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":"1"}],"paging":{"cursors":{"after":"same"}}}`,
		`{"data":[{"id":"1"}],"paging":{"cursors":{"after":"synthetic-page-secret"}}}`,
		`{"data":[{"id":"1"}],"paging":{"cursors":{"after":"bad\n"}}}`,
		`{"data":[{"id":"bad"}]}`, `{"data":null}`, `{}`,
	} {
		c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) })
		_, err := c.Tags(t.Context(), mentionIG, "same")
		require.Error(t, err)
	}
}

func TestMentionReadsRefusedError(t *testing.T) {
	for _, status := range []int{400, 403} {
		c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"error":{"message":"provider detail synthetic-page-secret"}}`)
		})
		_, mediaErr := c.MentionedMedia(t.Context(), mentionIG, mentionMedia)
		_, commentErr := c.MentionedComment(t.Context(), mentionIG, mentionComment)
		_, tagsErr := c.Tags(t.Context(), mentionIG, "")
		for _, err := range []error{mediaErr, commentErr, tagsErr} {
			require.ErrorIs(t, err, ErrMentionsRefused)
			assert.NotContains(t, err.Error(), "synthetic-page-secret")
			assert.NotContains(t, err.Error(), "provider detail")
		}
	}
}

// Raw provider JSON catches shape mismatches hidden by Go round-trip fixtures.
func TestMentionReadsOwnerNode(t *testing.T) {
	for _, kind := range []string{"tags", "media", "comment"} {
		t.Run(kind, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				media := `{"id":"17900000000000001","owner":{"id":"17841400000000001"}}`
				body := `{"mentioned_media":` + media + `}`
				if kind == "tags" {
					body = `{"data":[` + media + `]}`
				}
				if kind == "comment" {
					body = `{"mentioned_comment":{"id":"17800000000000001","media":` + media + `}}`
				}
				_, _ = fmt.Fprint(w, body)
			})
			var err error
			switch kind {
			case "tags":
				_, err = c.Tags(t.Context(), mentionIG, "")
			case "media":
				_, err = c.MentionedMedia(t.Context(), mentionIG, mentionMedia)
			default:
				_, err = c.MentionedComment(t.Context(), mentionIG, mentionComment)
			}
			require.NoError(t, err)
		})
	}
}
