package beeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

const (
	maxRetries = 8
	// DefaultBaseURL is the Beeper Desktop API loopback address.
	DefaultBaseURL = "http://localhost:23373"
	// defaultQPS bounds request rate against the live Beeper Desktop app. The
	// API is loopback-only, so this protects the user's running client rather
	// than any remote quota.
	defaultQPS = 20
)

// ErrNotFound reports a 404 from the Beeper Desktop API (e.g. a message or
// chat that no longer exists). Callers distinguish it from transient errors.
var ErrNotFound = errors.New("not found")

// ErrAssetTooLarge reports an asset exceeding the caller's size cap; the cap
// is enforced while reading so oversized bodies are never fully buffered.
var ErrAssetTooLarge = errors.New("asset exceeds size cap")

// TokenFunc returns a bearer token for a Beeper Desktop API request.
type TokenFunc func(context.Context) (string, error)

// Client is a read-only Beeper Desktop API client. It exposes only GET
// endpoints by construction, so the archiver can never mutate Beeper state.
type Client struct {
	baseURL string
	token   TokenFunc
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient creates a Client. baseURL is injected so tests can point at
// httptest servers. qps controls the token-bucket rate limit (default 20).
func NewClient(baseURL string, token TokenFunc, qps float64) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if qps <= 0 {
		qps = defaultQPS
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(qps), 1),
	}
}

// get fetches path, respecting the rate limiter and retrying on 429/5xx with
// Retry-After or exponential back-off.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	return c.fetch(ctx, path, 0)
}

// fetch is get with an optional response-size cap (0 = unlimited). Bodies are
// read through a limited reader, so an over-cap asset costs at most
// maxBytes+1 of memory regardless of what the server sends.
func (c *Client) fetch(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	reqURL := c.baseURL + path
	for attempt := range maxRetries {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for beeper rate limit: %w", err)
		}
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				return nil, fmt.Errorf("connect to Beeper Desktop at %s (is Beeper Desktop running?): %w", c.baseURL, err)
			}
			return nil, err
		}
		if maxBytes > 0 && resp.ContentLength > maxBytes {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("beeper GET %s: %d bytes: %w", reqURL, resp.ContentLength, ErrAssetTooLarge)
		}
		reader := io.Reader(resp.Body)
		if maxBytes > 0 {
			reader = io.LimitReader(resp.Body, maxBytes+1)
		}
		body, readErr := io.ReadAll(reader)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("beeper GET %s: read body: %w", reqURL, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("beeper GET %s: close body: %w", reqURL, closeErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if maxBytes > 0 && int64(len(body)) > maxBytes {
				return nil, fmt.Errorf("beeper GET %s: %w", reqURL, ErrAssetTooLarge)
			}
			return body, nil
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, fmt.Errorf("beeper GET %s: unauthorized (401): the access token was rejected; mint a new token in Beeper Desktop (Settings > Developer) and re-run 'msgvault add-beeper'", reqURL)
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("beeper GET %s: %w", reqURL, ErrNotFound)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			continue
		default:
			return nil, fmt.Errorf("beeper GET %s: status %d: %s", reqURL, resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("beeper GET %s: exhausted %d retries", reqURL, maxRetries)
}

// retryAfter parses a Retry-After header value (seconds) or falls back to
// exponential back-off capped at 60 s.
func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return min(time.Duration(1<<uint(attempt))*time.Second, 60*time.Second)
}

// getJSON fetches path and unmarshals the JSON body into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	body, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// ListAccounts returns the chat accounts connected to Beeper Desktop.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var out []Account
	if err := c.getJSON(ctx, "/v1/accounts", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchChatsParams filters a chat search page.
type SearchChatsParams struct {
	AccountID         string
	Cursor            string // opaque; paired with direction=before
	LastActivityAfter time.Time
}

// SearchChats fetches one page of chats for an account, ordered by last
// activity (most recent first). Page onward with direction=before using
// OldestCursor.
func (c *Client) SearchChats(ctx context.Context, p SearchChatsParams) (*SearchChatsOutput, error) {
	q := url.Values{}
	q.Set("limit", "200")
	q.Set("type", "any")
	if p.AccountID != "" {
		q.Add("accountIDs", p.AccountID)
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
		q.Set("direction", "before")
	}
	if !p.LastActivityAfter.IsZero() {
		q.Set("lastActivityAfter", p.LastActivityAfter.UTC().Format(time.RFC3339))
	}
	var out SearchChatsOutput
	if err := c.getJSON(ctx, "/v1/chats/search?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AllChats pages through every chat matching p, invoking fn per chat.
func (c *Client) AllChats(ctx context.Context, p SearchChatsParams, fn func(Chat) error) error {
	for {
		page, err := c.SearchChats(ctx, p)
		if err != nil {
			return err
		}
		for _, ch := range page.Items {
			if err := fn(ch); err != nil {
				return err
			}
		}
		if !page.HasMore || page.OldestCursor == "" {
			return nil
		}
		p.Cursor = page.OldestCursor
	}
}

// GetChat fetches one chat with its full participant list
// (maxParticipantCount=-1 returns all members).
func (c *Client) GetChat(ctx context.Context, chatID string) (*Chat, error) {
	var out Chat
	path := "/v1/chats/" + url.PathEscape(chatID) + "?maxParticipantCount=-1"
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMessagesPage fetches one page (~20 items) of a chat's messages. cursor
// is opaque; direction is "before" (older) or "after" (newer). Both empty
// fetch the newest page. Page-at-a-time is deliberate: the importer owns the
// loop so it can checkpoint between pages.
func (c *Client) ListMessagesPage(ctx context.Context, chatID, cursor, direction string) (*ListMessagesOutput, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
		if direction != "" {
			q.Set("direction", direction)
		}
	}
	path := "/v1/chats/" + url.PathEscape(chatID) + "/messages"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out ListMessagesOutput
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAssetBytes downloads an asset (attachment media) by its mxc:// style
// URL via the assets/serve endpoint. Reads are capped at maxBytes (see
// ErrAssetTooLarge); asset sizes are untrusted remote metadata.
func (c *Client) GetAssetBytes(ctx context.Context, assetURL string, maxBytes int64) ([]byte, error) {
	return c.fetch(ctx, "/v1/assets/serve?url="+url.QueryEscape(assetURL), maxBytes)
}

// GetMessage fetches a single message by ID (used to refresh reaction targets
// and to verify the sync-state anchor probe).
func (c *Client) GetMessage(ctx context.Context, chatID, messageID string) (*Message, error) {
	var out Message
	path := "/v1/chats/" + url.PathEscape(chatID) + "/messages/" + url.PathEscape(messageID)
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
