package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientReadAPI(t *testing.T) {
	fake := newFakeDiscord(t)
	server := fake.server()
	t.Cleanup(server.Close)

	client, err := NewClient(server.URL+"/api/v10", "test-bot-token")
	require.NoError(t, err)
	ctx := context.Background()

	me, err := client.Me(ctx)
	require.NoError(t, err)
	assert.Equal(t, "101", me.ID)
	assert.Equal(t, "Archive Bot", me.GlobalName)
	assert.True(t, me.Bot)

	guilds, err := client.Guilds(ctx)
	require.NoError(t, err)
	assert.Len(t, guilds, 201)
	assert.Equal(t, "1200", guilds[len(guilds)-1].ID)

	guild, err := client.Guild(ctx, "201")
	require.NoError(t, err)
	assert.Equal(t, "Test Guild", guild.Name)

	channels, err := client.GuildChannels(ctx, "201")
	require.NoError(t, err)
	require.Len(t, channels, 1)
	assert.Equal(t, "Synthetic discussion", channels[0].Topic)

	active, err := client.ActiveThreads(ctx, "201")
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "active-thread", active[0].Name)
	assert.False(t, active[0].ThreadMetadata.Archived)

	before := mustParseTime(t, "2026-07-18T00:00:00Z")
	publicPage, err := client.ArchivedThreads(ctx, "301", false, before)
	require.NoError(t, err)
	require.Len(t, publicPage.Threads, 1)
	assert.Equal(t, "public-thread", publicPage.Threads[0].Name)
	assert.True(t, publicPage.HasMore)
	assert.Equal(t, mustParseTime(t, "2026-07-17T12:00:00Z"), publicPage.NextBefore)

	privatePage, err := client.ArchivedThreads(ctx, "301", true, before)
	require.NoError(t, err)
	require.Len(t, privatePage.Threads, 1)
	assert.Equal(t, "private-thread", privatePage.Threads[0].Name)

	members, err := client.GuildMembers(ctx, "201", "101")
	require.NoError(t, err)
	require.Len(t, members.Members, 1)
	assert.Equal(t, "Test Alice", members.Members[0].Nick)
	assert.False(t, members.HasMore)
	assert.Empty(t, members.NextAfter)

	messages, err := client.Messages(ctx, "301", MessageQuery{Before: "600", Limit: 100})
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello", messages[0].Content)
	assert.JSONEq(t, `{"id":"501","channel_id":"301","guild_id":"201","author":{"id":"102","username":"alice","global_name":"Alice"},"content":"hello","timestamp":"2026-07-18T12:01:00Z","type":0,"future_field":{"retained":true}}`, string(messages[0].Raw))

	message, err := client.Message(ctx, "301", "501")
	require.NoError(t, err)
	assert.Equal(t, "hello, edited", message.Content)
	require.NotNil(t, message.EditedTimestamp)

	request := fake.firstRequest()
	require.NotNil(t, request)
	assert.Equal(t, "Bot test-bot-token", request.Header.Get("Authorization"))
	assert.Equal(t, UserAgent, request.Header.Get("User-Agent"))
	assert.Equal(t, "application/json", request.Header.Get("Accept"))
	assert.Equal(t, []string{
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusForbidden, map[string]any{"code": 50013, "message": "Missing Permissions"})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "secret-token")
	require.NoError(t, err)

	_, err = client.GuildChannels(context.Background(), "201")

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(t, 50013, apiErr.Code)
	assert.Equal(t, "Missing Permissions", apiErr.Message)
	assert.NotContains(t, err.Error(), "secret-token")
}

func TestClientSerializesRoutesLearnedToShareBucket(t *testing.T) {
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
	require.NoError(t, err)

	_, err = client.Messages(context.Background(), "301", MessageQuery{Limit: 1})
	require.NoError(t, err)
	_, err = client.Message(context.Background(), "301", "501")
	require.NoError(t, err)
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
		require.NoError(t, callErr)
	}
	assert.Equal(t, int32(1), maximum.Load())
}

func TestClientHonorsGlobal429AcrossRoutes(t *testing.T) {
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
	require.NoError(t, err)

	meDone := make(chan error, 1)
	go func() {
		_, callErr := client.Me(context.Background())
		meDone <- callErr
	}()
	started := <-globalSet
	require.Eventually(t, func() bool {
		client.limits.mu.Lock()
		defer client.limits.mu.Unlock()
		return client.limits.globalUntil.After(time.Now())
	}, 500*time.Millisecond, time.Millisecond)
	_, err = client.Guild(context.Background(), "201")
	require.NoError(t, err)
	require.NoError(t, <-meDone)
	assert.GreaterOrEqual(t, time.Unix(0, guildAt.Load()).Sub(started), 65*time.Millisecond)
}

func TestClientRetainsGlobalPauseAfterRetryExhaustion(t *testing.T) {
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
	require.NoError(t, err)

	_, err = client.Me(context.Background())
	require.Error(t, err)
	exhaustedAt := time.Now()
	_, err = client.Guild(context.Background(), "201")

	require.NoError(t, err)
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
			require.NoError(t, err)

			started := time.Now()
			_, err = client.Me(context.Background())

			require.NoError(t, err)
			assert.GreaterOrEqual(t, time.Since(started), tt.minimumDelay)
			assert.Equal(t, int32(2), calls.Load())
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

func TestClientRejectsCrossOriginPaginationAndRedirects(t *testing.T) {
	t.Run("pagination URL", func(t *testing.T) {
		client, err := NewClient("https://discord.example/api/v10", "test-token")
		require.NoError(t, err)

		_, err = client.resolvePaginationURL("https://attacker.example/api/v10/users/@me/guilds?after=secret")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	})

	t.Run("redirect", func(t *testing.T) {
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
		require.NoError(t, err)

		_, err = client.Me(context.Background())

		require.Error(t, err)
		assert.Empty(t, redirectedAuthorization)
		assert.NotContains(t, err.Error(), "secret-token")
		assert.NotContains(t, err.Error(), "do-not-log")
		assert.ErrorIs(t, err, ErrRedirect)
	})
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
