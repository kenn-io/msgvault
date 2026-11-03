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
	"strings"
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
			page, err := c.VerifyPage(context.Background(), "100")
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
	got, err := c.Comments(context.Background(), "100_200", "opaque+/=")
	require.NoError(err)
	require.Len(got.Data, 1)
	assert.Equal("100_201", got.Data[0].ID)
	assert.Equal("next-cursor", got.After)
}

func TestFinalCursorDoesNotContinue(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[],"paging":{"cursors":{"after":"final"}}}`)
	})
	got, err := c.Posts(context.Background(), "100", AdsPosts, "")
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
			_, err := c.Posts(context.Background(), "100", edge, "")
			require.NoError(t, err)
		})
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "invalid edge made an HTTP request") })
	_, err := c.Posts(context.Background(), "100", PostEdge("../me"), "")
	require.Error(t, err)
}

func TestGraphTokenRedaction(t *testing.T) {
	for _, code := range []int{200, 400, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = fmt.Fprint(w, `{"error":{"message":"synthetic-page-secret"}}`)
			})
			_, err := c.VerifyPage(context.Background(), "100")
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
	_, err := c.VerifyPage(context.Background(), "100")
	require.Error(t, err)
	assert.False(t, reached)
}

func TestGraphRejectsMalformedPagingAndPaths(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[],"paging":{"next":"https://example.test"}}`)
	})
	_, err := c.Posts(context.Background(), "100", Posts, "")
	require.Error(t, err)
	invalid := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "invalid Graph ID made an HTTP request")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	})
	for _, id := range []string{"../me", "100/comments?access_token=x", "100/200", ""} {
		_, err = invalid.Comments(context.Background(), id, "")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	_, err = c.VerifyPage(context.Background(), "100")
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

func TestGraphPostSendsFormBodyOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v26.0/100/subscribed_apps", r.URL.Path)
		assert.Empty(r.URL.RawQuery)
		assert.Equal("application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		assert.Equal("Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		if !assert.NoError(r.ParseForm()) {
			return
		}
		assert.Equal("feed,messages", r.PostForm.Get("subscribed_fields"))
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	var out struct {
		Success bool `json:"success"`
	}
	require.NoError(c.post(t.Context(), "100/subscribed_apps", url.Values{"subscribed_fields": {"feed,messages"}}, &out))
	assert.True(out.Success)
}

func TestGraphPostJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Empty(r.URL.RawQuery)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			return
		}
		assert.JSONEq(`{"recipient":{"comment_id":"100_200_300"},"message":{"text":"Synthetic reply"}}`, string(body))
		_, _ = fmt.Fprint(w, `{"message_id":"400"}`)
	})
	var out struct {
		ID string `json:"message_id"`
	}
	require.NoError(c.postJSON(t.Context(), "100/messages", map[string]any{"recipient": map[string]string{"comment_id": "100_200_300"}, "message": map[string]string{"text": "Synthetic reply"}}, &out))
	assert.Equal("400", out.ID)
}

func TestGraphDeleteDecodesSuccess(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/v26.0/100_200_300", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		assert.Equal(t, "Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, `{"success":true}`)
	})
	var out struct {
		Success bool `json:"success"`
	}
	require.NoError(t, c.delete(t.Context(), "100_200_300", &out))
	assert.True(t, out.Success)
}

func TestGraphPostRefusesRedirect(t *testing.T) {
	for _, status := range []int{302, 307, 308, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
			})
			var out map[string]any
			require.Error(t, c.post(t.Context(), "100/subscribed_apps", url.Values{"subscribed_fields": {"feed"}}, &out))
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestGraphErrorKeepsOnlyCodes(t *testing.T) {
	for _, method := range []string{"GET", "POST", "JSON", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":{"message":"secret-bearing synthetic-page-secret","code":10,"error_subcode":2018300}}`)
			})
			var out map[string]any
			var err error
			switch method {
			case "GET":
				err = c.get(t.Context(), "100", nil, &out)
			case "POST":
				err = c.post(t.Context(), "100", nil, &out)
			case "JSON":
				err = c.postJSON(t.Context(), "100", map[string]string{}, &out)
			case "DELETE":
				err = c.delete(t.Context(), "100", &out)
			}
			var api *APIError
			require.ErrorAs(err, &api)
			assert.Equal(400, api.Status)
			assert.Equal(10, api.Code)
			assert.Equal(2018300, api.Subcode)
			assert.True(api.Permission())
			assert.Equal("graph request failed (HTTP 400, code 10, subcode 2018300)", err.Error())
			assert.NotContains(err.Error(), "secret")
		})
	}
}

func TestGraphErrorBodyCapped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, body := range []string{"invalid", `{"error":{"message":"` + strings.Repeat("x", 1<<20) + `","code":200}}`, `{"error":{"code":200}}` + strings.Repeat(" ", 9<<20)} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, body)
		})
		var out map[string]any
		err := c.get(t.Context(), "100", nil, &out)
		var api *APIError
		require.ErrorAs(err, &api)
		assert.Equal(403, api.Status)
		assert.Zero(api.Code)
		assert.Zero(api.Subcode)
		assert.Equal("graph request failed (HTTP 403)", err.Error())
	}
}

func TestGraphThrottleAndNotFoundCodes(t *testing.T) {
	assert := assert.New(t)
	for _, code := range []int{4, 17, 32, 613, 80000, 80001, 80014, 80015, 100, 190, 10, 200, 299, 300} {
		e := &APIError{Status: 400, Code: code}
		assert.Equal(code == 4 || code == 17 || code == 32 || code == 613 || (code >= 80000 && code <= 80014), e.Throttled(), "code %d", code)
		assert.Equal(code == 100, e.NotFound())
		assert.Equal(code == 190, e.TokenExpired())
		assert.Equal(code == 10 || (code >= 200 && code <= 299), e.Permission())
	}
	assert.Equal("graph request failed (HTTP 400, code 100)", (&APIError{Status: 400, Code: 100}).Error())
}

func TestGraphErrorBodyCancellation(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := assert.New(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":200,"message":"`)
				flusher, ok := w.(http.Flusher)
				if !assert.True(ok) {
					return
				}
				flusher.Flush()
				<-r.Context().Done()
			})
			c.http.Transport = cancelErrorBodyTransport{base: c.http.Transport, cancel: cancel}
			var out map[string]any
			err := c.post(ctx, "100/subscribed_apps", url.Values{"subscribed_fields": {"feed"}}, &out)
			assert.ErrorIs(err, context.Canceled)
		})
	}
}

type cancelErrorBodyTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (tr cancelErrorBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := tr.base.RoundTrip(r)
	if err == nil {
		response.Body = &cancelErrorBody{ReadCloser: response.Body, cancel: tr.cancel}
	}
	return response, err
}

type cancelErrorBody struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (b *cancelErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.cancel()
	}
	return n, err
}
