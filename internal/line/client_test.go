package line_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func newClient(t *testing.T, options ...line.ClientOption) (*line.Client, *lineapi.Server) {
	t.Helper()
	api := lineapi.New()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	options = append([]line.ClientOption{line.WithBaseURLs(server.URL, server.URL)}, options...)
	return line.NewClient("2000000000", "synthetic-secret", options...), api
}

func TestClientTokenAndProfiles(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	client, api := newClient(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, err := client.BotInfo(ctx); assert.NoError(t, err) })
	}
	wg.Wait()
	checks.Equal(1, api.Count("POST", "/oauth2/v3/token"))
	request := api.Requests()[0]
	form, err := url.ParseQuery(string(request.Body))
	must.NoError(err)
	checks.Equal("client_credentials", form.Get("grant_type"))
	checks.Equal("2000000000", form.Get("client_id"))
	checks.Equal("synthetic-secret", form.Get("client_secret"))
	for _, tc := range []struct {
		source line.Source
		path   string
	}{
		{line.Source{Type: "user", UserID: "U00000000000000000000000000000002"}, "/v2/bot/profile/U00000000000000000000000000000002"},
		{line.Source{Type: "group", GroupID: "C00000000000000000000000000000003"}, "/v2/bot/group/C00000000000000000000000000000003/member/U00000000000000000000000000000002"},
		{line.Source{Type: "room", RoomID: "R00000000000000000000000000000004"}, "/v2/bot/room/R00000000000000000000000000000004/member/U00000000000000000000000000000002"},
	} {
		profile, err := client.Profile(ctx, tc.source, "U00000000000000000000000000000002")
		must.NoError(err)
		checks.Equal("Example User", profile.DisplayName)
		checks.Equal(1, api.Count("GET", tc.path))
	}
	group, err := client.GroupSummary(ctx, "C00000000000000000000000000000003")
	must.NoError(err)
	checks.Equal("Example Group", group.GroupName)
	bot, err := client.BotInfo(ctx)
	must.NoError(err)
	checks.Equal("U00000000000000000000000000000001", bot.UserID)
	checks.Equal(1, api.Count("POST", "/oauth2/v3/token"))
}

func TestClientUnauthorizedRefresh(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	client, api := newClient(t)
	api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 401}, lineapi.Response{Status: 200, Body: `{"userId":"U00000000000000000000000000000001"}`})
	_, err := client.BotInfo(context.Background())
	must.NoError(err)
	checks.Equal(2, api.Count("POST", "/oauth2/v3/token"))
	var authorizations []string
	for _, request := range api.Requests() {
		if request.Path == "/v2/bot/info" {
			authorizations = append(authorizations, request.Header.Get("Authorization"))
		}
	}
	checks.Equal([]string{"Bearer synthetic-token-1", "Bearer synthetic-token-2"}, authorizations, "the retry must present the newly minted token")
	api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 401}, lineapi.Response{Status: 401})
	_, err = client.BotInfo(context.Background())
	var apiErr *line.APIError
	must.ErrorAs(err, &apiErr)
	checks.Equal(401, apiErr.StatusCode)
	checks.Equal(3, api.Count("POST", "/oauth2/v3/token"))
}

func TestClientRefreshBeforeExpiration(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	client, api := newClient(t, line.WithClock(func() time.Time { return now }))
	_, err := client.BotInfo(context.Background())
	must.NoError(err)
	now = now.Add(14*time.Minute - time.Second)
	_, err = client.BotInfo(context.Background())
	must.NoError(err)
	checks.Equal(1, api.Count("POST", "/oauth2/v3/token"))
	now = now.Add(time.Second)
	_, err = client.BotInfo(context.Background())
	must.NoError(err)
	checks.Equal(2, api.Count("POST", "/oauth2/v3/token"))
}

func TestClientCancellationAndRateLimit(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	client, api := newClient(t, line.WithClock(func() time.Time { return now }))
	for index, header := range []string{"120", now.Add(time.Minute).Format(http.TimeFormat)} {
		api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": {header}}, Body: "synthetic-secret synthetic-token"})
		_, err := client.BotInfo(context.Background())
		var apiErr *line.APIError
		must.ErrorAs(err, &apiErr)
		checks.Equal(429, apiErr.StatusCode)
		checks.Equal(now.Add([]time.Duration{120 * time.Second, time.Minute}[index]), apiErr.RetryAfter)
		checks.NotContains(err.Error(), "synthetic-secret")
		checks.NotContains(err.Error(), "synthetic-token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.BotInfo(ctx)
	must.ErrorIs(err, context.Canceled)
}

func TestClientWebhookSetup(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	client, api := newClient(t)
	endpoint := "https://archive.example.com/webhooks/line/2000000000"
	must.NoError(client.SetWebhook(context.Background(), endpoint))
	result, err := client.TestWebhook(context.Background(), endpoint)
	must.NoError(err)
	checks.True(result.Success)
	for _, request := range api.Requests() {
		if strings.Contains(request.Path, "/webhook/") {
			checks.JSONEq(`{"endpoint":"`+endpoint+`"}`, string(request.Body))
		}
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	var calls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	for _, path := range []string{"/oauth2/v3/token", "/v2/bot/info", "/v2/bot/message/123/content"} {
		t.Run(path, func(t *testing.T) {
			client, api := newClient(t, line.WithHTTPClient(target.Client()))
			method := http.MethodGet
			if path == "/oauth2/v3/token" {
				method = http.MethodPost
			}
			api.Enqueue(method, path, lineapi.Response{Status: 307, Header: http.Header{"Location": {target.URL}}})
			var err error
			if strings.HasSuffix(path, "content") {
				var response *http.Response
				response, err = client.Content(context.Background(), "123")
				if response != nil {
					require.NoError(t, response.Body.Close())
				}
			} else {
				_, err = client.BotInfo(context.Background())
			}
			require.Error(t, err)
		})
	}
	assert.Zero(t, calls)
}

func TestClientBoundsAndRedactsResponses(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	client, api := newClient(t)
	oversized := `{"userId":"U00000000000000000000000000000001","basicId":"@synthetic","displayName":"Synthetic bot","padding":"` + strings.Repeat("x", 1<<20) + `"}`
	api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 200, Body: oversized})
	_, err := client.BotInfo(context.Background())
	must.EqualError(err, "line: API response exceeds limit")
	checks.Less(len(err.Error()), 200)

	for _, body := range []string{`{"userId":`, `{"userId":"U00000000000000000000000000000001"} {}`} {
		api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 200, Body: body})
		_, err = client.BotInfo(context.Background())
		must.Error(err)
		checks.Less(len(err.Error()), 200)
	}
	api.Enqueue("GET", "/v2/bot/info", lineapi.Response{Status: 500, Body: "synthetic-secret synthetic-token"})
	_, err = client.BotInfo(context.Background())
	must.Error(err)
	checks.NotContains(err.Error(), "synthetic-")
}

func TestClientEscapesPathSegments(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	client, api := newClient(t)
	_, err := client.Profile(context.Background(), line.Source{Type: "group", GroupID: "group/with?query#fragment"}, "user/with?query#fragment")
	must.NoError(err)
	requests := api.Requests()
	checks.Equal("/v2/bot/group/group%2Fwith%3Fquery%23fragment/member/user%2Fwith%3Fquery%23fragment", requests[len(requests)-1].Path)
	_, err = client.Profile(context.Background(), line.Source{Type: "group"}, "U00000000000000000000000000000002")
	must.Error(err)
	checks.NotErrorIs(err, context.Canceled)
}

// Observe the real transport's Body.Read so cancellation happens during
// decoding rather than racing the initial request/response exchange.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type signalBody struct {
	io.ReadCloser

	once    sync.Once
	reading chan struct{}
}

func (b *signalBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	return b.ReadCloser.Read(p)
}

func TestClientCancellationDuringResponse(t *testing.T) {
	for _, path := range []string{"/oauth2/v3/token", "/v2/bot/info"} {
		t.Run(path, func(t *testing.T) {
			must := require.New(t)
			reading := make(chan struct{})
			api := lineapi.New()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					api.ServeHTTP(w, r)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{`)
				assert.NoError(t, http.NewResponseController(w).Flush())
				<-r.Context().Done()
			}))
			defer server.Close()
			defaultTransport, ok := http.DefaultTransport.(*http.Transport)
			must.True(ok)
			transport := defaultTransport.Clone()
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err == nil && r.URL.Path == path {
					response.Body = &signalBody{ReadCloser: response.Body, reading: reading}
				}
				return response, err
			})}
			client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(server.URL, server.URL), line.WithHTTPClient(httpClient))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.BotInfo(ctx); done <- err }()
			select {
			case <-reading:
			case <-time.After(5 * time.Second):
				must.FailNow("response read did not start")
			}
			cancel()
			select {
			case err := <-done:
				must.ErrorIs(err, context.Canceled)
			case <-time.After(5 * time.Second):
				must.FailNow("response read did not cancel")
			}
		})
	}
}

func TestClientTokenWaitCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	api := lineapi.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/v3/token" {
			close(started)
			<-release
		}
		api.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer close(release)
	client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(server.URL, server.URL))
	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	go func() { _, _ = client.BotInfo(firstCtx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "token request did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.BotInfo(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "token waiter did not cancel")
	}
}

func TestClientTokenValidationAndRateLimit(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	for _, body := range []string{
		`{"access_token":"","expires_in":900,"token_type":"Bearer"}`,
		`{"access_token":"synthetic-token","expires_in":0,"token_type":"Bearer"}`,
		`{"access_token":"synthetic-token","expires_in":9223372036854775807,"token_type":"Bearer"}`,
		`{"access_token":"synthetic-token","expires_in":900,"token_type":"Other"}`,
		`null`,
	} {
		client, api := newClient(t)
		api.Enqueue("POST", "/oauth2/v3/token", lineapi.Response{Status: 200, Body: body})
		_, err := client.BotInfo(context.Background())
		must.Error(err)
		checks.NotContains(err.Error(), "synthetic-token")
		checks.Zero(api.Count("GET", "/v2/bot/info"))
	}
	client, api := newClient(t)
	api.Enqueue("POST", "/oauth2/v3/token", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": {"60"}}})
	_, err := client.BotInfo(context.Background())
	var apiErr *line.APIError
	must.ErrorAs(err, &apiErr)
	checks.Equal(429, apiErr.StatusCode)
	checks.False(apiErr.RetryAfter.IsZero())
	checks.Equal(1, api.Count("POST", "/oauth2/v3/token"))
}

func TestClientSeparateDataHost(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	api, data := lineapi.New(), lineapi.New()
	apiServer, dataServer := httptest.NewServer(api), httptest.NewServer(data)
	defer apiServer.Close()
	defer dataServer.Close()
	client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(apiServer.URL, dataServer.URL))
	response, err := client.Content(context.Background(), "123")
	must.NoError(err)
	must.NoError(response.Body.Close())
	checks.Equal(1, api.Count("POST", "/oauth2/v3/token"))
	checks.Equal(0, data.Count("POST", "/oauth2/v3/token"))
	checks.Equal(0, api.Count("GET", "/v2/bot/message/123/content"))
	checks.Equal(1, data.Count("GET", "/v2/bot/message/123/content"))
}

func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(server.URL, server.URL), line.WithHTTPClient(&http.Client{Timeout: 25 * time.Millisecond})) //nolint:kennlint // deadlinetest: HTTP timeout expiry is asserted as context.DeadlineExceeded.
	_, err := client.BotInfo(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClientContentUsesAttachmentDownloadTimeout(t *testing.T) {
	var downloadDeadline time.Time
	client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs("https://api.example.test", "https://data.example.test"), line.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`
		if request.URL.Path == "/v2/bot/message/123/content" {
			var ok bool
			downloadDeadline, ok = request.Context().Deadline()
			require.True(t, ok)
			body = "synthetic-content"
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}))
	response, err := client.Content(context.Background(), "123")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.True(t, downloadDeadline.After(time.Now().Add(30*time.Second)))
}

func TestClientRejectsInvalidBaseURLs(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	userInfo := strings.Replace(server.URL, "http://", "http://user:secret@", 1)
	for _, base := range []string{userInfo, server.URL + "?query", server.URL + "#fragment"} {
		client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(base, base))
		_, err := client.BotInfo(context.Background())
		require.EqualError(t, err, "line: invalid API base URL")
		assert.NotContains(t, err.Error(), "secret")
	}
	assert.Zero(t, requests.Load())
}

type trackedBody struct {
	io.ReadCloser

	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return b.ReadCloser.Close() }

func TestClientResponseOwnership(t *testing.T) {
	for _, status := range []int{200, 202, 404, 410, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)

			api := lineapi.New()
			api.Enqueue("GET", "/v2/bot/message/123/content", lineapi.Response{Status: status, Body: "example-content"}, lineapi.Response{Status: status, Body: "example-content"})
			server := httptest.NewServer(api)
			defer server.Close()
			defaultTransport, ok := http.DefaultTransport.(*http.Transport)
			must.True(ok)
			transport := defaultTransport.Clone()
			defer transport.CloseIdleConnections()
			var bodies []*trackedBody
			client := line.NewClient("2000000000", "synthetic-secret", line.WithBaseURLs(server.URL, server.URL), line.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(r)
				if err == nil {
					body := &trackedBody{ReadCloser: response.Body}
					bodies = append(bodies, body)
					response.Body = body
				}
				return response, err
			})}))
			response, err := client.Content(context.Background(), "123")
			if status == 200 || status == 202 || status == 404 || status == 410 {
				must.NoError(err)
				checks.Equal(status, response.StatusCode)
				checks.False(bodies[len(bodies)-1].closed)
				body, readErr := io.ReadAll(response.Body)
				must.NoError(readErr)
				checks.Equal("example-content", string(body))
				must.NoError(response.Body.Close())
			} else {
				must.Error(err)
				if status == 500 {
					var apiErr *line.APIError
					must.ErrorAs(err, &apiErr)
					checks.Equal(status, apiErr.StatusCode)
					checks.Nil(response)
				}
			}
			for _, body := range bodies {
				checks.True(body.closed)
			}
		})
	}
}
