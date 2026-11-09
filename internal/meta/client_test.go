package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := newClient("synthetic-page-secret", server.URL, server.Client())
	require.NoError(t, err)
	return c
}

func TestPageIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		valid      bool
	}{
		{"page", `{"id":"100","name":"Example Page","category":"Community"}`, true},
		{"wrong page", `{"id":"101","name":"Other Page","category":"Community"}`, false},
		{"personal user", `{"id":"100","name":"Example Person"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v26.0/me", r.URL.Path)
				assert.Equal(t, "id,name,category", r.URL.Query().Get("fields"))
				assert.Equal(t, "Bearer synthetic-page-secret", r.Header.Get("Authorization"))
				_, _ = fmt.Fprint(w, tt.body)
			})
			page, err := c.VerifyPage(t.Context(), "100")
			if tt.valid {
				require.NoError(t, err)
				assert.Equal(t, "Example Page", page.Name)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPageCommentsPagination(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/v26.0/100_200/comments", r.URL.Path)
		assert.Equal("chronological", r.URL.Query().Get("order"))
		assert.Equal("toplevel", r.URL.Query().Get("filter"))
		assert.Equal("opaque+/=", r.URL.Query().Get("after"))
		_, _ = fmt.Fprint(w, `{"data":[{"id":"100_201","message":"Hello","from":{"id":"300","name":"Example"}}],"paging":{"cursors":{"after":"next-cursor"},"next":"https://evil.example/?access_token=secret"}}`)
	})
	got, err := c.Comments(t.Context(), "100_200", "opaque+/=")
	require.NoError(err)
	require.Len(got.Data, 1)
	assert.Equal("100_201", got.Data[0].ID)
	assert.Equal("next-cursor", got.After)
}

func TestFinalCursorDoesNotContinue(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[],"paging":{"cursors":{"after":"final"}}}`)
	})
	got, err := c.Posts(t.Context(), "100", AdsPosts, "")
	require.NoError(t, err)
	assert.Empty(t, got.After)
}

func TestFixedPostEdges(t *testing.T) {
	for _, edge := range []PostEdge{Posts, AdsPosts, VisitorPosts} {
		t.Run(string(edge), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v26.0/100/"+string(edge), r.URL.Path)
				assert.Contains(t, r.URL.Query().Get("fields"), "from")
				_, _ = fmt.Fprint(w, `{"data":[]}`)
			})
			_, err := c.Posts(t.Context(), "100", edge, "")
			require.NoError(t, err)
		})
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "invalid edge made an HTTP request") })
	_, err := c.Posts(t.Context(), "100", PostEdge("../me"), "")
	require.Error(t, err)
}

func TestGraphTokenRedaction(t *testing.T) {
	for _, code := range []int{200, 400, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = fmt.Fprint(w, `{"error":{"message":"synthetic-page-secret"}}`)
			})
			_, err := c.VerifyPage(t.Context(), "100")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "synthetic-page-secret")
		})
	}
}

func TestGraphRejectsRedirect(t *testing.T) {
	reached := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer other.Close()
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	_, err := c.VerifyPage(t.Context(), "100")
	require.Error(t, err)
	assert.False(t, reached)
}

func TestGraphRejectsMalformedPagingAndPaths(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[],"paging":{"next":"https://example.test"}}`)
	})
	_, err := c.Posts(t.Context(), "100", Posts, "")
	require.Error(t, err)
	invalid := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "invalid Graph ID made an HTTP request")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	})
	for _, id := range []string{"../me", "100/comments?access_token=x", "100/200", ""} {
		_, err = invalid.Comments(t.Context(), id, "")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.VerifyPage(ctx, "100")
	require.ErrorIs(t, err, context.Canceled)
}

type graphErrorTransport struct{}

func (graphErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Get", URL: "https://example.test/?access_token=synthetic-page-secret", Err: errors.New("synthetic-page-secret")}
}

func TestGraphTransportErrorDoesNotReflectToken(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	c, err := newClient("synthetic-page-secret", "https://graph.facebook.com", &http.Client{Transport: graphErrorTransport{}})
	require.NoError(err)
	_, err = c.VerifyPage(t.Context(), "100")
	require.Error(err)
	assert.NotContains(err.Error(), "synthetic-page-secret")
	assert.NotContains(err.Error(), "access_token")
}

// Keep the real HTTP body/error: only observe the first bytes before yielding.
type streamedGraphBody struct {
	io.ReadCloser

	cancel context.CancelCauseFunc
	reads  *atomic.Int32
}

func (b streamedGraphBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.reads.Add(1)
		if b.cancel != nil {
			b.cancel(jobctx.ErrYieldedToWaiter)
		}
	}
	return n, err
}

type streamedGraphTransport struct {
	http.RoundTripper

	cancel context.CancelCauseFunc
	reads  *atomic.Int32
}

func (tr streamedGraphTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := tr.RoundTripper.RoundTrip(r)
	if err == nil && r.URL.Path == "/v26.0/100_301/comments" {
		response.Body = streamedGraphBody{ReadCloser: response.Body, cancel: tr.cancel, reads: tr.reads}
	}
	return response, err
}
