package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostAndCommentNodeReads(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/v26.0/100_200":
			assert.Equal(t, "id,message,created_time,from", r.URL.Query().Get("fields"))
		case "/v26.0/100_200_5":
			assert.Equal(t, "id,message,created_time,from,parent", r.URL.Query().Get("fields"))
		default:
			assert.Fail(t, "unexpected node path", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"message":"Synthetic comment","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300"},"parent":{"id":"100_200"}}`, r.URL.Path[len("/v26.0/"):])
	})
	post, err := c.Post(t.Context(), "100_200")
	require.NoError(t, err)
	assert.Equal(t, "100_200", post.ID)
	comment, err := c.Comment(t.Context(), "100_200_5")
	require.NoError(t, err)
	assert.Equal(t, "100_200", comment.Parent.ID)
	for _, id := range []string{"", "../me", "100/comments?token=x"} {
		_, err = c.Post(t.Context(), id)
		assert.Error(t, err)
		_, err = c.Comment(t.Context(), id)
		assert.Error(t, err)
	}
}

func TestTokenAppReadsID(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/app", r.URL.Path)
		assert.Equal(t, "id", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"9"}`)
	})
	id, err := c.TokenApp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "9", id)
}
func TestTokenAppRejectsEmptyOrNonDecimal(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":""}`, `{"id":"app"}`, `{"id":"0"}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) })
		_, err := c.TokenApp(t.Context())
		assert.Error(t, err)
	}
}
func TestSubscribedFieldsFindsApp(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/100/subscribed_apps", r.URL.Path)
		assert.Equal(t, "id,subscribed_fields", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"data":[{"id":"8","subscribed_fields":["messages"]},{"id":"9","subscribed_fields":["feed","mention"]}]}`)
	})
	installed, fields, err := c.SubscribedFields(t.Context(), "100", "9")
	require.NoError(t, err)
	assert.True(t, installed)
	assert.Equal(t, []string{"feed", "mention"}, fields)
	installed, fields, err = c.SubscribedFields(t.Context(), "100", "7")
	require.NoError(t, err)
	assert.False(t, installed)
	assert.Empty(t, fields)
}
func TestSubscribedFieldsMissingAppIDRefuses(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"subscribed_fields":["feed"]}]}`)
	})
	_, _, err := c.SubscribedFields(t.Context(), "100", "9")
	require.ErrorContains(t, err, "cannot identify the app")
}
func TestSubscribePostsFields(t *testing.T) {
	for _, body := range []string{`{"success":true}`, `{"success":false}`, `{}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/v26.0/100/subscribed_apps", r.URL.Path)
			assert.Empty(t, r.URL.RawQuery)
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "feed,messages", r.PostForm.Get("subscribed_fields"))
			_, _ = fmt.Fprint(w, body)
		})
		err := c.Subscribe(t.Context(), "100", []string{"feed", "messages"})
		if body == `{"success":true}` {
			assert.NoError(t, err)
		} else {
			assert.Error(t, err)
		}
	}
}
func TestSubscribeRejectsUnknownField(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "invalid subscription made request") })
	for _, fields := range [][]string{{"unknown"}, {"comments"}, nil} {
		assert.Error(t, c.Subscribe(t.Context(), "100", fields))
	}
	assert.Error(t, c.Subscribe(t.Context(), "../me", []string{"feed"}))
}
func TestSubscribePermissionRefusal(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":200,"error_subcode":3,"message":"secret-bearing text"}}`)
	})
	err := c.Subscribe(t.Context(), "100", []string{"feed"})
	var api *APIError
	require.ErrorAs(t, err, &api)
	assert.True(t, api.Permission())
	assert.Equal(t, 3, api.Subcode)
	assert.NotContains(t, err.Error(), "secret-bearing")
}
