package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the versioned Discord REST API root.
	DefaultBaseURL = "https://discord.com/api/v10"
	// UserAgent identifies this deliberately narrow Discord integration.
	UserAgent = "msgvault-discord-import/1.0"

	maxAttempts      = 5
	maxResponseBytes = 32 << 20
	guildPageLimit   = 200
	threadPageLimit  = 100
)

var (
	// ErrDecodeResponse classifies malformed successful Discord responses
	// without retaining attacker-controlled decoder details.
	ErrDecodeResponse = errors.New("discord API response could not be decoded")
	// ErrRedirect classifies a refused Discord API redirect.
	ErrRedirect = errors.New("discord API redirects are not allowed")
)

// APIError is a decoded Discord REST error. Error deliberately omits the
// upstream message body so attacker-controlled URLs cannot enter diagnostics;
// callers can classify expected conditions through StatusCode and Code.
type APIError struct {
	Operation  string
	StatusCode int
	Code       int
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("discord %s: HTTP %d (Discord code %d)", e.Operation, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("discord %s: HTTP %d", e.Operation, e.StatusCode)
}

// Client is a minimal, read-only Discord REST client.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
	limits  *rateLimitState
}

var _ API = (*Client)(nil)

// Format prevents diagnostic formatting from reflecting the bot token.
func (c *Client) Format(state fmt.State, _ rune) {
	if c == nil {
		_, _ = io.WriteString(state, "Discord REST client <nil>")
		return
	}
	baseURL := "unconfigured"
	if c.baseURL != nil {
		baseURL = c.baseURL.String()
	}
	_, _ = io.WriteString(state, "Discord REST client ("+baseURL+")")
}

// NewClient creates a REST client bound to one exact Discord API root. Plain
// HTTP is accepted only for loopback httptest servers.
func NewClient(baseURL, botToken string) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("invalid Discord API base URL")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("discord API base URL must use HTTPS")
	}
	if botToken == "" {
		return nil, errors.New("discord bot token is empty")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")

	return &Client{
		baseURL: parsed,
		token:   botToken,
		http: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return ErrRedirect
			},
		},
		limits: newRateLimitState(),
	}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type discordRoute struct {
	operation string
	path      string
	bucketKey string
	major     string
}

func (c *Client) Me(ctx context.Context) (User, error) {
	var out User
	err := c.getJSON(ctx, discordRoute{"current bot", "/users/@me", "GET /users/@me", "global"}, nil, &out)
	return out, err
}

func (c *Client) Guilds(ctx context.Context) ([]Guild, error) {
	var guilds []Guild
	after := ""
	for {
		query := url.Values{"limit": {strconv.Itoa(guildPageLimit)}}
		if after != "" {
			query.Set("after", after)
		}
		var page []Guild
		err := c.getJSON(ctx, discordRoute{"accessible guilds", "/users/@me/guilds", "GET /users/@me/guilds", "global"}, query, &page)
		if err != nil {
			return nil, err
		}
		guilds = append(guilds, page...)
		if len(page) < guildPageLimit {
			return guilds, nil
		}
		next := page[len(page)-1].ID
		if next == "" || next == after {
			return nil, errors.New("discord accessible guilds: pagination cursor did not advance")
		}
		after = next
	}
}

func (c *Client) Guild(ctx context.Context, guildID string) (Guild, error) {
	var out Guild
	id, err := snowflakePathValue("guild ID", guildID)
	if err != nil {
		return out, err
	}
	err = c.getJSON(ctx, discordRoute{"guild detail", "/guilds/" + id, "GET /guilds/:guild", guildID}, nil, &out)
	return out, err
}

func (c *Client) GuildChannels(ctx context.Context, guildID string) ([]Channel, error) {
	var out []Channel
	id, err := snowflakePathValue("guild ID", guildID)
	if err != nil {
		return nil, err
	}
	err = c.getJSON(ctx, discordRoute{"guild channels", "/guilds/" + id + "/channels", "GET /guilds/:guild/channels", guildID}, nil, &out)
	return out, err
}

func (c *Client) ActiveThreads(ctx context.Context, guildID string) ([]Channel, error) {
	var response struct {
		Threads []Channel `json:"threads"`
	}
	id, err := snowflakePathValue("guild ID", guildID)
	if err != nil {
		return nil, err
	}
	err = c.getJSON(ctx, discordRoute{"active threads", "/guilds/" + id + "/threads/active", "GET /guilds/:guild/threads/active", guildID}, nil, &response)
	return response.Threads, err
}

func (c *Client) ArchivedThreads(ctx context.Context, channelID string, private bool, before ArchiveCursor) (ThreadPage, error) {
	var out ThreadPage
	id, err := snowflakePathValue("channel ID", channelID)
	if err != nil {
		return out, err
	}
	endpoint := "/channels/" + id + "/threads/archived/public"
	operation := "public archived threads"
	routeKey := "GET /channels/:channel/threads/archived/public"
	if private {
		endpoint = "/channels/" + id + "/users/@me/threads/archived/private"
		operation = "private archived threads"
		routeKey = "GET /channels/:channel/users/@me/threads/archived/private"
	}
	query := url.Values{"limit": {strconv.Itoa(threadPageLimit)}}
	if private {
		if before.BeforeID != "" {
			cursor, err := snowflakePathValue("private archive cursor", before.BeforeID)
			if err != nil {
				return out, err
			}
			query.Set("before", cursor)
		}
	} else if !before.BeforeTime.IsZero() {
		query.Set("before", before.BeforeTime.UTC().Format(time.RFC3339Nano))
	}
	var response struct {
		Threads []Channel `json:"threads"`
		HasMore bool      `json:"has_more"`
	}
	err = c.getJSON(ctx, discordRoute{operation, endpoint, routeKey, channelID}, query, &response)
	if err != nil {
		return out, err
	}
	out.Threads = response.Threads
	out.HasMore = response.HasMore
	if response.HasMore {
		if len(response.Threads) == 0 {
			return ThreadPage{}, fmt.Errorf("%w: discord %s response has more pages but no archive cursor", ErrMalformedCatalog, operation)
		}
		last := response.Threads[len(response.Threads)-1]
		if private {
			if _, err := snowflakePathValue("private archive cursor", last.ID); err != nil {
				return ThreadPage{}, fmt.Errorf("%w: discord %s response has more pages but no thread cursor: %w", ErrMalformedCatalog, operation, err)
			}
			out.NextBeforeID = last.ID
		} else if last.ThreadMetadata == nil || last.ThreadMetadata.ArchiveTimestamp.IsZero() {
			return ThreadPage{}, fmt.Errorf("%w: discord %s response has more pages but no archive cursor", ErrMalformedCatalog, operation)
		} else {
			out.NextBeforeTime = last.ThreadMetadata.ArchiveTimestamp
		}
	}
	return out, nil
}

func (c *Client) Messages(ctx context.Context, channelID string, messageQuery MessageQuery) ([]Message, error) {
	var out []Message
	id, err := snowflakePathValue("channel ID", channelID)
	if err != nil {
		return nil, err
	}
	query, err := messageQuery.values()
	if err != nil {
		return nil, err
	}
	err = c.getJSON(ctx, discordRoute{"channel messages", "/channels/" + id + "/messages", "GET /channels/:channel/messages", channelID}, query, &out)
	return out, err
}

func (c *Client) Message(ctx context.Context, channelID, messageID string) (Message, error) {
	var out Message
	channel, err := snowflakePathValue("channel ID", channelID)
	if err != nil {
		return out, err
	}
	message, err := snowflakePathValue("message ID", messageID)
	if err != nil {
		return out, err
	}
	err = c.getJSON(ctx, discordRoute{"message detail", "/channels/" + channel + "/messages/" + message, "GET /channels/:channel/messages/:message", channelID}, nil, &out)
	return out, err
}

func (q MessageQuery) values() (url.Values, error) {
	cursors := 0
	values := url.Values{}
	for _, cursor := range []struct {
		name  string
		value string
	}{{"around", q.Around}, {"before", q.Before}, {"after", q.After}} {
		name, value := cursor.name, cursor.value
		if value == "" {
			continue
		}
		if _, err := snowflakePathValue("message "+name+" cursor", value); err != nil {
			return nil, err
		}
		cursors++
		values.Set(name, value)
	}
	if cursors > 1 {
		return nil, errors.New("discord message query accepts only one of around, before, or after")
	}
	if q.Limit < 0 || q.Limit > 100 {
		return nil, errors.New("discord message query limit must be between 0 and 100")
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	return values, nil
}

func snowflakePathValue(name, value string) (string, error) {
	parsed, err := ParseSnowflake(value)
	if err != nil || parsed == 0 {
		return "", fmt.Errorf("invalid Discord %s", name)
	}
	return value, nil
}

func (c *Client) getJSON(ctx context.Context, route discordRoute, query url.Values, out any) error {
	body, err := c.get(ctx, route, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("discord %s: %w", route.operation, ErrDecodeResponse)
	}
	return nil
}

func (c *Client) get(ctx context.Context, route discordRoute, query url.Values) ([]byte, error) {
	requestURL := c.baseURL.String() + route.path
	if len(query) != 0 {
		requestURL += "?" + query.Encode()
	}
	var lastError error
	for attempt := range maxAttempts {
		lease, err := c.limits.acquire(ctx, route.bucketKey, route.major)
		if err != nil {
			return nil, fmt.Errorf("discord %s: wait for rate limit: %w", route.operation, err)
		}
		body, status, headers, err := c.do(ctx, requestURL, route.operation)
		c.limits.observe(lease, headers)
		retryable := err == nil && (status == http.StatusTooManyRequests || status >= 500)
		if retryable {
			delay, global := retryDelay(headers, body, attempt)
			c.limits.pause(lease.route, delay, global)
		}
		lease.release()
		if err != nil {
			return nil, err
		}
		if status >= 200 && status < 300 {
			return body, nil
		}

		apiErr := decodeAPIError(route.operation, status, body)
		lastError = apiErr
		if status != http.StatusTooManyRequests && status < 500 {
			return nil, apiErr
		}
		if attempt == maxAttempts-1 {
			break
		}
	}
	return nil, lastError
}

func (c *Client) do(ctx context.Context, requestURL, operation string) ([]byte, int, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("discord %s: create request: %w", operation, err)
	}
	request.Header.Set("Authorization", "Bot "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", UserAgent)

	response, err := c.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, 0, nil, fmt.Errorf("discord %s: %w", operation, ErrRedirect)
		}
		return nil, 0, nil, fmt.Errorf("discord %s: request failed: %w", operation, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, 0, nil, fmt.Errorf("discord %s: read response: %w", operation, readErr)
	}
	if closeErr != nil {
		return nil, 0, nil, fmt.Errorf("discord %s: close response: %w", operation, closeErr)
	}
	if len(body) > maxResponseBytes {
		return nil, 0, nil, fmt.Errorf("discord %s: response exceeds %d bytes", operation, maxResponseBytes)
	}
	return body, response.StatusCode, response.Header, nil
}

func decodeAPIError(operation string, status int, body []byte) *APIError {
	apiErr := &APIError{Operation: operation, StatusCode: status}
	var payload struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &payload) == nil {
		apiErr.Code = payload.Code
	}
	return apiErr
}

func retryDelay(headers http.Header, body []byte, attempt int) (time.Duration, bool) {
	headerDelay, headerOK := secondsDuration(headers.Get("Retry-After"))
	var payload struct {
		RetryAfter json.Number `json:"retry_after"`
		Global     bool        `json:"global"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	decodeOK := decoder.Decode(&payload) == nil
	jsonOK := decodeOK && payload.RetryAfter != ""
	jsonDelay, durationOK := secondsDuration(string(payload.RetryAfter))
	jsonOK = jsonOK && durationOK
	global := decodeOK && payload.Global ||
		strings.EqualFold(strings.TrimSpace(headers.Get("X-Ratelimit-Global")), "true") ||
		strings.EqualFold(strings.TrimSpace(headers.Get("X-Ratelimit-Scope")), "global")

	switch {
	case headerOK && jsonOK:
		return max(headerDelay, jsonDelay), global
	case headerOK:
		return headerDelay, global
	case jsonOK:
		return jsonDelay, global
	default:
		return min(100*time.Millisecond*time.Duration(1<<attempt), 2*time.Second), global
	}
}

func secondsDuration(value string) (time.Duration, bool) {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}

type contextMutex chan struct{}

func newContextMutex() contextMutex {
	mutex := make(contextMutex, 1)
	mutex <- struct{}{}
	return mutex
}

func (m contextMutex) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m:
		return nil
	}
}

func (m contextMutex) unlock() {
	m <- struct{}{}
}

type routeLimit struct {
	lock    contextMutex
	bucket  string
	major   string
	readyAt time.Time
}

type bucketLimit struct {
	lock    contextMutex
	readyAt time.Time
}

type rateLimitState struct {
	mu          sync.Mutex
	routes      map[string]*routeLimit
	buckets     map[string]*bucketLimit
	globalUntil time.Time
}

func newRateLimitState() *rateLimitState {
	return &rateLimitState{routes: make(map[string]*routeLimit), buckets: make(map[string]*bucketLimit)}
}

type rateLimitLease struct {
	route  *routeLimit
	bucket *bucketLimit
}

func (l *rateLimitState) acquire(ctx context.Context, routeKey, major string) (*rateLimitLease, error) {
	lookupKey := routeKey + "|" + major
	l.mu.Lock()
	route := l.routes[lookupKey]
	if route == nil {
		route = &routeLimit{lock: newContextMutex(), major: major}
		l.routes[lookupKey] = route
	}
	l.mu.Unlock()

	if err := route.lock.lock(ctx); err != nil {
		return nil, err
	}
	lease := &rateLimitLease{route: route}
	if err := l.waitGlobal(ctx); err != nil {
		route.lock.unlock()
		return nil, err
	}

	l.mu.Lock()
	bucket := l.buckets[route.bucket]
	l.mu.Unlock()
	if bucket != nil {
		if err := bucket.lock.lock(ctx); err != nil {
			route.lock.unlock()
			return nil, err
		}
		lease.bucket = bucket
	}
	if err := l.waitReady(ctx, route, bucket); err != nil {
		lease.release()
		return nil, err
	}
	return lease, nil
}

func (l *rateLimitState) waitGlobal(ctx context.Context) error {
	for {
		l.mu.Lock()
		until := l.globalUntil
		l.mu.Unlock()
		if !time.Now().Before(until) {
			return nil
		}
		if err := waitUntil(ctx, until); err != nil {
			return err
		}
	}
}

func (l *rateLimitState) waitReady(ctx context.Context, route *routeLimit, bucket *bucketLimit) error {
	for {
		if err := l.waitGlobal(ctx); err != nil {
			return err
		}
		l.mu.Lock()
		until := route.readyAt
		if bucket != nil && bucket.readyAt.After(until) {
			until = bucket.readyAt
		}
		l.mu.Unlock()
		if !time.Now().Before(until) {
			return nil
		}
		if err := waitUntil(ctx, until); err != nil {
			return err
		}
	}
}

func waitUntil(ctx context.Context, until time.Time) error {
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (lease *rateLimitLease) release() {
	if lease.bucket != nil {
		lease.bucket.lock.unlock()
	}
	lease.route.lock.unlock()
}

func (l *rateLimitState) observe(lease *rateLimitLease, headers http.Header) {
	bucketID := headers.Get("X-Ratelimit-Bucket")
	remaining, remainingErr := strconv.Atoi(headers.Get("X-Ratelimit-Remaining"))
	resetAt, hasReset := headerResetAt(headers)

	l.mu.Lock()
	defer l.mu.Unlock()
	if bucketID != "" {
		bucketKey := bucketID + "|" + lease.route.major
		bucket := l.buckets[bucketKey]
		if bucket == nil {
			bucket = &bucketLimit{lock: newContextMutex()}
			l.buckets[bucketKey] = bucket
		}
		lease.route.bucket = bucketKey
		if remainingErr == nil && remaining <= 0 && hasReset && resetAt.After(bucket.readyAt) {
			bucket.readyAt = resetAt
		}
	}
	if remainingErr == nil && remaining <= 0 && hasReset && resetAt.After(lease.route.readyAt) {
		lease.route.readyAt = resetAt
	}
}

func headerResetAt(headers http.Header) (time.Time, bool) {
	if delay, ok := secondsDuration(headers.Get("X-Ratelimit-Reset-After")); ok {
		return time.Now().Add(delay), true
	}
	resetSeconds, err := strconv.ParseFloat(headers.Get("X-Ratelimit-Reset"), 64)
	if err != nil || resetSeconds < 0 {
		return time.Time{}, false
	}
	seconds, fraction := math.Modf(resetSeconds)
	return time.Unix(int64(seconds), int64(fraction*float64(time.Second))), true
}

func (l *rateLimitState) pause(route *routeLimit, delay time.Duration, global bool) {
	until := time.Now().Add(delay)
	l.mu.Lock()
	defer l.mu.Unlock()
	if global {
		if until.After(l.globalUntil) {
			l.globalUntil = until
		}
		return
	}
	if until.After(route.readyAt) {
		route.readyAt = until
	}
	if bucket := l.buckets[route.bucket]; bucket != nil && until.After(bucket.readyAt) {
		bucket.readyAt = until
	}
}
