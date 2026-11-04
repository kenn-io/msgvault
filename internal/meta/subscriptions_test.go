package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostAndCommentNodeReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		id := "100_200"
		switch r.URL.Path {
		case "/v26.0/100_200":
			assert.Equal("id,message,created_time,from", r.URL.Query().Get("fields"))
		case "/v26.0/100_200_5":
			id = "100_200_5"
			assert.Equal("id,message,created_time,from,parent", r.URL.Query().Get("fields"))
		default:
			assert.Fail("unexpected node path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		data := fmt.Sprintf(`{"id":%q,"message":"Synthetic comment","created_time":"2026-10-01T12:00:00+0000","from":{"id":"300","name":"Example Customer"},"parent":{"id":"100_200"}}`, id)
		_, _ = w.Write([]byte(data))
	})
	post, err := c.Post(t.Context(), "100_200")
	require.NoError(err)
	assert.Equal("100_200", post.ID)
	assert.Equal("Synthetic comment", post.Message)
	require.NotNil(post.From)
	assert.Equal("300", post.From.ID)
	assert.Equal("Example Customer", post.From.Name)
	comment, err := c.Comment(t.Context(), "100_200_5")
	require.NoError(err)
	require.NotNil(comment.Parent)
	assert.Equal("100_200", comment.Parent.ID)
	for _, id := range []string{"", "../me", "100/comments?token=x"} {
		_, err = c.Post(t.Context(), id)
		require.Error(err)
		_, err = c.Comment(t.Context(), id)
		require.Error(err)
	}
}

func TestTokenAppReadsID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v26.0/app", r.URL.Path)
		assert.Equal("id", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"9"}`)
	})
	id, err := c.TokenApp(t.Context())
	require.NoError(err)
	assert.Equal("9", id)
}
func TestTokenAppRejectsEmptyOrNonDecimal(t *testing.T) {
	require := require.New(t)
	for _, body := range []string{`{}`, `{"id":""}`, `{"id":"app"}`, `{"id":"0"}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, body) // #nosec G705 -- body comes from a literal table of synthetic JSON responses.
		})
		_, err := c.TokenApp(t.Context())
		require.Error(err)
	}
}
func TestSubscribedFieldsFindsApp(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v26.0/100/subscribed_apps", r.URL.Path)
		assert.Equal("id,subscribed_fields", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"data":[{"id":"8","subscribed_fields":["messages"]},{"id":"9","subscribed_fields":["feed","mention"]}]}`)
	})
	installed, fields, err := c.SubscribedFields(t.Context(), "100", "9")
	require.NoError(err)
	assert.True(installed)
	assert.Equal([]string{"feed", "mention"}, fields)
	installed, fields, err = c.SubscribedFields(t.Context(), "100", "7")
	require.NoError(err)
	assert.False(installed)
	assert.Empty(fields)
}
func TestSubscribePostsFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, body := range []string{`{"success":true}`, `{"success":false}`, `{}`} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(http.MethodPost, r.Method)
			assert.Equal("/v26.0/100/subscribed_apps", r.URL.Path)
			assert.Empty(r.URL.RawQuery)
			if !assert.NoError(r.ParseForm()) {
				return
			}
			assert.Equal("feed,messages", r.PostForm.Get("subscribed_fields"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, body) // #nosec G705 -- body comes from a literal table of synthetic JSON responses.
		})
		err := c.Subscribe(t.Context(), "100", []string{"feed", "messages"})
		if body == `{"success":true}` {
			assert.NoError(err)
		} else {
			require.Error(err)
		}
	}
}
func TestSubscribeRejectsUnknownField(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail("invalid subscription made request")
	})
	for _, fields := range [][]string{{"unknown"}, {"comments"}, nil} {
		require.Error(c.Subscribe(t.Context(), "100", fields))
	}
	require.Error(c.Subscribe(t.Context(), "../me", []string{"feed"}))
}
func TestSubscribePermissionRefusal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":200,"error_subcode":3,"message":"secret-bearing text"}}`)
	})
	err := c.Subscribe(t.Context(), "100", []string{"feed"})
	var api *APIError
	require.ErrorAs(err, &api)
	assert.True(api.Permission())
	assert.Equal(3, api.Subcode)
	assert.NotContains(err.Error(), "secret-bearing")
}
