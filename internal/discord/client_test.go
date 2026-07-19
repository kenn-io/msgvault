package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientReadAPI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fake := newFakeDiscord(t)
	server := fake.server()
	t.Cleanup(server.Close)

	client, err := NewClient(server.URL+"/api/v10", "test-bot-token")
	require.NoError(err)
	ctx := context.Background()

	me, err := client.Me(ctx)
	require.NoError(err)
	assert.Equal("101", me.ID)
	assert.Equal("Archive Bot", me.GlobalName)
	assert.True(me.Bot)

	guilds, err := client.Guilds(ctx)
	require.NoError(err)
	assert.Len(guilds, 201)
	assert.Equal("1200", guilds[len(guilds)-1].ID)

	guild, err := client.Guild(ctx, "201")
	require.NoError(err)
	assert.Equal("Test Guild", guild.Name)

	channels, err := client.GuildChannels(ctx, "201")
	require.NoError(err)
	require.Len(channels, 1)
	assert.Equal("Synthetic discussion", channels[0].Topic)

	active, err := client.ActiveThreads(ctx, "201")
	require.NoError(err)
	require.Len(active, 1)
	assert.Equal("active-thread", active[0].Name)
	assert.False(active[0].ThreadMetadata.Archived)

	before := mustParseTime(t, "2026-07-18T00:00:00Z")
	publicPage, err := client.ArchivedThreads(ctx, "301", false, before)
	require.NoError(err)
	require.Len(publicPage.Threads, 1)
	assert.Equal("public-thread", publicPage.Threads[0].Name)
	assert.True(publicPage.HasMore)
	assert.Equal(mustParseTime(t, "2026-07-17T12:00:00Z"), publicPage.NextBefore)

	privatePage, err := client.ArchivedThreads(ctx, "301", true, before)
	require.NoError(err)
	require.Len(privatePage.Threads, 1)
	assert.Equal("private-thread", privatePage.Threads[0].Name)

	members, err := client.GuildMembers(ctx, "201", "101")
	require.NoError(err)
	require.Len(members.Members, 1)
	assert.Equal("Test Alice", members.Members[0].Nick)
	assert.False(members.HasMore)
	assert.Empty(members.NextAfter)

	messages, err := client.Messages(ctx, "301", MessageQuery{Before: "600", Limit: 100})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("hello", messages[0].Content)
	assert.JSONEq(`{"id":"501","channel_id":"301","guild_id":"201","author":{"id":"102","username":"alice","global_name":"Alice"},"content":"hello","timestamp":"2026-07-18T12:01:00Z","type":0,"future_field":{"retained":true}}`, string(messages[0].Raw))

	message, err := client.Message(ctx, "301", "501")
	require.NoError(err)
	assert.Equal("hello, edited", message.Content)
	require.NotNil(message.EditedTimestamp)

	request := fake.firstRequest()
	require.NotNil(request)
	assert.Equal("Bot test-bot-token", request.Header.Get("Authorization"))
	assert.Equal(UserAgent, request.Header.Get("User-Agent"))
	assert.Equal("application/json", request.Header.Get("Accept"))
	assert.Equal([]string{
		"/api/v10/users/@me",
		"/api/v10/users/@me/guilds?limit=200",
		"/api/v10/users/@me/guilds?after=1199&limit=200",
		"/api/v10/guilds/201",
		"/api/v10/guilds/201/channels",
		"/api/v10/guilds/201/threads/active",
		"/api/v10/channels/301/threads/archived/public?before=2026-07-18T00%3A00%3A00Z&limit=100",
		"/api/v10/channels/301/users/@me/threads/archived/private?before=2026-07-18T00%3A00%3A00Z&limit=100",
		"/api/v10/guilds/201/members?after=101&limit=1000",
		"/api/v10/channels/301/messages?before=600&limit=100",
		"/api/v10/channels/301/messages/501",
	}, fake.requestPaths())
}

func TestMessageQueryValidation(t *testing.T) {
	tests := []struct {
		name  string
		query MessageQuery
	}{
		{name: "multiple cursors", query: MessageQuery{Before: "1", After: "2"}},
		{name: "around plus cursor", query: MessageQuery{Around: "1", Before: "2"}},
		{name: "negative limit", query: MessageQuery{Limit: -1}},
		{name: "limit above Discord maximum", query: MessageQuery{Limit: 101}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDiscord(t)
			server := fake.server()
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL+"/api/v10", "test-token")
			require.NoError(t, err)

			_, err = client.Messages(context.Background(), "301", tt.query)

			require.Error(t, err)
			assert.Empty(t, fake.requestPaths())
		})
	}
}

func TestClientDecodesDiscordAPIError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusForbidden, map[string]any{
			"code": 50013, "message": "https://attacker.example/error?signature=api-error-marker",
		})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret-token")
	require.NoError(err)

	_, err = client.GuildChannels(context.Background(), "201")

	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(50013, apiErr.Code)
	assert.NotContains(err.Error(), "secret-token")
	if strings.Contains(fmt.Sprintf("%#v", apiErr), "api-error-marker") {
		require.Fail("Discord API error retained an untrusted upstream message")
	}
}

func TestClientSanitizesSuccessfulResponseDecodeErrors(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusOK, map[string]any{
			"id": "501", "timestamp": "https://attacker.example/file?signature=decode-marker&token=token-marker",
		})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret-token")
	require.NoError(err)

	_, err = client.Message(context.Background(), "301", "501")

	require.Error(err)
	if strings.Contains(err.Error(), "decode-marker") || strings.Contains(err.Error(), "token-marker") || strings.Contains(err.Error(), "secret-token") {
		require.Fail("Discord response decode error exposed confidential input")
	}
	require.ErrorIs(err, ErrDecodeResponse)
}

func TestClientSerializesRoutesLearnedToShareBucket(t *testing.T) {
	require := require.New(t)
	var enabled atomic.Bool
	var inFlight atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Ratelimit-Bucket", "shared-message-bucket")
		if enabled.Load() {
			current := inFlight.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			inFlight.Add(-1)
		}
		if request.URL.Path == "/channels/301/messages" {
			writeDiscordJSON(w, http.StatusOK, []any{})
			return
		}
		writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "501", "channel_id": "301", "author": map[string]any{"id": "102"}, "timestamp": "2026-07-18T12:00:00Z"})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	_, err = client.Messages(context.Background(), "301", MessageQuery{Limit: 1})
	require.NoError(err)
	_, err = client.Message(context.Background(), "301", "501")
	require.NoError(err)
	enabled.Store(true)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, callErr := client.Messages(context.Background(), "301", MessageQuery{Limit: 1})
		errs <- callErr
	}()
	go func() {
		defer wg.Done()
		_, callErr := client.Message(context.Background(), "301", "501")
		errs <- callErr
	}()
	wg.Wait()
	close(errs)
	for callErr := range errs {
		require.NoError(callErr)
	}
	assert.Equal(t, int32(1), maximum.Load())
}

func TestClientHonorsGlobal429AcrossRoutes(t *testing.T) {
	require := require.New(t)
	globalSet := make(chan time.Time, 1)
	var meCalls atomic.Int32
	var guildAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/@me":
			if meCalls.Add(1) == 1 {
				now := time.Now()
				globalSet <- now
				writeDiscordJSON(w, http.StatusTooManyRequests, map[string]any{"message": "rate limited", "retry_after": 0.08, "global": true})
				return
			}
			writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "101"})
		case "/guilds/201":
			guildAt.Store(time.Now().UnixNano())
			writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "201"})
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	meDone := make(chan error, 1)
	go func() {
		_, callErr := client.Me(context.Background())
		meDone <- callErr
	}()
	started := <-globalSet
	require.Eventually(func() bool {
		client.limits.mu.Lock()
		defer client.limits.mu.Unlock()
		return client.limits.globalUntil.After(time.Now())
	}, 500*time.Millisecond, time.Millisecond)
	_, err = client.Guild(context.Background(), "201")
	require.NoError(err)
	require.NoError(<-meDone)
	assert.GreaterOrEqual(t, time.Unix(0, guildAt.Load()).Sub(started), 65*time.Millisecond)
}

func TestClientHonorsHeaderSignaledGlobal429AcrossRoutes(t *testing.T) {
	tests := []struct {
		name        string
		headerName  string
		headerValue string
		body        string
	}{
		{
			name:        "global header with malformed JSON",
			headerName:  "X-Ratelimit-Global",
			headerValue: "  TrUe  ",
			body:        `{"malformed"`,
		},
		{
			name:        "scope header without JSON global field",
			headerName:  "X-Ratelimit-Scope",
			headerValue: "  GLOBAL  ",
			body:        `{"message":"limited"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			limited := make(chan struct{}, 1)
			var meCalls atomic.Int32
			var guildAt atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/users/@me":
					if meCalls.Add(1) == 1 {
						w.Header().Set("Retry-After", "0.15")
						w.Header().Set(tt.headerName, tt.headerValue)
						w.WriteHeader(http.StatusTooManyRequests)
						if _, err := w.Write([]byte(tt.body)); err != nil {
							panic(fmt.Errorf("write synthetic 429 response: %w", err))
						}
						limited <- struct{}{}
						return
					}
					writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "101"})
				case "/guilds/201":
					guildAt.Store(time.Now().UnixNano())
					writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "201"})
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "test-token")
			require.NoError(err)

			meDone := make(chan error, 1)
			go func() {
				_, callErr := client.Me(context.Background())
				meDone <- callErr
			}()
			<-limited
			require.Eventually(func() bool {
				client.limits.mu.Lock()
				defer client.limits.mu.Unlock()
				if client.limits.globalUntil.After(time.Now()) {
					return true
				}
				for _, route := range client.limits.routes {
					if route.readyAt.After(time.Now()) {
						return true
					}
				}
				return false
			}, 500*time.Millisecond, time.Millisecond)
			started := time.Now()

			_, err = client.Guild(context.Background(), "201")

			require.NoError(err)
			require.NoError(<-meDone)
			assert.GreaterOrEqual(t, time.Unix(0, guildAt.Load()).Sub(started), 100*time.Millisecond)
		})
	}
}

func TestClientRetainsGlobalPauseAfterRetryExhaustion(t *testing.T) {
	require := require.New(t)
	var guildAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/users/@me" {
			writeDiscordJSON(w, http.StatusTooManyRequests, map[string]any{"message": "limited", "retry_after": 0.04, "global": true})
			return
		}
		guildAt.Store(time.Now().UnixNano())
		writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "201"})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	_, err = client.Me(context.Background())
	require.Error(err)
	exhaustedAt := time.Now()
	_, err = client.Guild(context.Background(), "201")

	require.NoError(err)
	assert.GreaterOrEqual(t, time.Unix(0, guildAt.Load()).Sub(exhaustedAt), 30*time.Millisecond)
}

func TestClientRetainsHeaderSignaledGlobalPauseAfterRetryExhaustion(t *testing.T) {
	require := require.New(t)
	var guildAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/users/@me" {
			w.Header().Set("Retry-After", "0.04")
			w.Header().Set("X-Ratelimit-Scope", " global ")
			writeDiscordJSON(w, http.StatusTooManyRequests, map[string]any{"message": "limited"})
			return
		}
		guildAt.Store(time.Now().UnixNano())
		writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "201"})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(err)

	_, err = client.Me(context.Background())
	require.Error(err)
	exhaustedAt := time.Now()
	_, err = client.Guild(context.Background(), "201")

	require.NoError(err)
	assert.GreaterOrEqual(t, time.Unix(0, guildAt.Load()).Sub(exhaustedAt), 30*time.Millisecond)
}

func TestClientHonorsRetryAfterHeaderAndJSON(t *testing.T) {
	tests := []struct {
		name         string
		header       string
		body         map[string]any
		minimumDelay time.Duration
	}{
		{name: "header", header: "0.04", body: map[string]any{"message": "limited"}, minimumDelay: 35 * time.Millisecond},
		{name: "json", body: map[string]any{"message": "limited", "retry_after": 0.04}, minimumDelay: 35 * time.Millisecond},
		{name: "longer value wins", header: "0.01", body: map[string]any{"message": "limited", "retry_after": 0.04}, minimumDelay: 35 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if tt.header != "" {
						w.Header().Set("Retry-After", tt.header)
					}
					writeDiscordJSON(w, http.StatusTooManyRequests, tt.body)
					return
				}
				writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "101"})
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(server.URL, "test-token")
			require.NoError(err)

			started := time.Now()
			_, err = client.Me(context.Background())

			require.NoError(err)
			assert.GreaterOrEqual(time.Since(started), tt.minimumDelay)
			assert.Equal(int32(2), calls.Load())
		})
	}
}

func TestClientCancellationDuringRateLimitWaits(t *testing.T) {
	t.Run("429 retry", func(t *testing.T) {
		limited := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeDiscordJSON(w, http.StatusTooManyRequests, map[string]any{"message": "limited", "retry_after": 10})
			close(limited)
		}))
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "test-token")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, callErr := client.Me(ctx)
			done <- callErr
		}()
		<-limited
		cancel()

		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(500 * time.Millisecond):
			require.Fail(t, "request did not observe cancellation during retry wait")
		}
	})

	t.Run("bucket reset wait", func(t *testing.T) {
		firstDone := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Ratelimit-Bucket", "me-bucket")
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.Header().Set("X-Ratelimit-Reset-After", "10")
			writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "101"})
			select {
			case <-firstDone:
			default:
				close(firstDone)
			}
		}))
		t.Cleanup(server.Close)
		client, err := NewClient(server.URL, "test-token")
		require.NoError(t, err)
		_, err = client.Me(context.Background())
		require.NoError(t, err)
		<-firstDone
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err = client.Me(ctx)

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestClientBoundsServerErrorRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		writeDiscordJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "unavailable"})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token")
	require.NoError(t, err)

	_, err = client.Me(context.Background())

	require.Error(t, err)
	assert.Equal(t, int32(maxAttempts), calls.Load())
}

func TestClientRejectsRedirects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		redirectedAuthorization = request.Header.Get("Authorization")
		writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "101"})
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/collect?signature=do-not-log")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret-token")
	require.NoError(err)

	_, err = client.Me(context.Background())

	require.Error(err)
	assert.Empty(redirectedAuthorization)
	assert.NotContains(err.Error(), "secret-token")
	assert.NotContains(err.Error(), "do-not-log")
	assert.ErrorIs(err, ErrRedirect)
}

func TestNewClientRejectsUnsafeBaseURL(t *testing.T) {
	tests := []string{"", "discord.example/api/v10", "http://discord.example/api/v10", "https://user:pass@discord.example/api/v10?token=secret"}
	for _, baseURL := range tests {
		t.Run(fmt.Sprintf("%q", baseURL), func(t *testing.T) {
			_, err := NewClient(baseURL, "test-token")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}
