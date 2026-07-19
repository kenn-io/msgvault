package slack

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
	"time"

	"golang.org/x/time/rate"
)

const (
	maxRetries = 8
	// DefaultBaseURL is the Slack Web API root. Injected so tests can point
	// at httptest servers.
	DefaultBaseURL = "https://slack.com/api"

	// historyPageLimit is the page size requested from conversations.history
	// and conversations.replies. Internal (non-distributed) apps are granted
	// up to 999; distributed non-Marketplace apps are clamped to 15 — the
	// client honors whatever the server returns either way.
	historyPageLimit = 999
	// listPageLimit is the page size for enumeration methods.
	listPageLimit = 200
)

// Web API method tiers (docs.slack.dev/apis/web-api/rate-limits). The
// limiters run slightly under the published per-minute budgets so scheduled
// daemon syncs never trip 429 in steady state; 429 + Retry-After remains the
// authoritative backstop.
var methodTiers = map[string]int{
	"auth.test":             4,
	"users.list":            2,
	"users.conversations":   3,
	"conversations.history": 3,
	"conversations.replies": 3,
	"conversations.members": 4,
}

// ErrNotFound reports a channel/thread/user that no longer exists.
var ErrNotFound = errors.New("not found")

// ErrAssetTooLarge reports a file exceeding the caller's size cap; the cap is
// enforced while reading so oversized bodies are never fully buffered.
var ErrAssetTooLarge = errors.New("file exceeds size cap")

// ErrAuth reports a rejected or under-scoped token; callers surface it with
// re-run-add-slack guidance instead of retrying.
var ErrAuth = errors.New("slack auth error")

// notFoundAPIErrors are Slack method errors that mean "the thing is gone",
// not "the request failed".
var notFoundAPIErrors = map[string]bool{
	"channel_not_found": true,
	"thread_not_found":  true,
	"message_not_found": true,
	"user_not_found":    true,
	"file_not_found":    true,
	"file_deleted":      true,
}

// authAPIErrors are Slack method errors that mean the token (not the
// request) is the problem.
var authAPIErrors = map[string]bool{
	"invalid_auth":       true,
	"not_authed":         true,
	"account_inactive":   true,
	"token_revoked":      true,
	"token_expired":      true,
	"missing_scope":      true,
	"no_permission":      true,
	"ekm_access_denied":  true,
	"org_login_required": true,
}

// Client is a read-only Slack Web API client: it exposes only read methods
// by construction, so the archiver can never mutate workspace state.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// limiters holds one token bucket per rate tier (see methodTiers).
	limiters map[int]*rate.Limiter
	// mediaTransport overrides the file-download transport (tests only; the
	// API base URL is injectable but file URLs are validated against the real
	// files.slack.com host).
	mediaTransport http.RoundTripper
}

// NewClient creates a Client. baseURL "" means the real Slack API.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	perMinute := func(n float64) *rate.Limiter {
		return rate.NewLimiter(rate.Limit(n/60), 2)
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		limiters: map[int]*rate.Limiter{
			2: perMinute(18),
			3: perMinute(45),
			4: perMinute(90),
		},
	}
}

// disableRateLimits removes the per-tier pacing (tests only).
func (c *Client) disableRateLimits() {
	for tier := range c.limiters {
		c.limiters[tier] = rate.NewLimiter(rate.Inf, 1)
	}
}

// call POSTs a Web API method with form params, decoding the JSON body into
// out (which must embed apiResponse via an ok/error check by the caller).
// 429 and 5xx are retried with Retry-After / exponential back-off.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	tier := methodTiers[method]
	if tier == 0 {
		tier = 3
	}
	reqURL := c.baseURL + "/" + method
	body := params.Encode()
	for attempt := range maxRetries {
		if err := c.limiters[tier].Wait(ctx); err != nil {
			return fmt.Errorf("wait for slack rate limit: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		data, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("slack %s: read body: %w", method, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("slack %s: close body: %w", method, closeErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("slack %s: status %d: %s", method, resp.StatusCode, truncate(data, 200))
		}

		var envelope apiResponse
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("slack %s: decode envelope: %w", method, err)
		}
		if !envelope.OK {
			return apiError(method, &envelope)
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("slack %s: decode response: %w", method, err)
			}
		}
		return nil
	}
	return fmt.Errorf("slack %s: exhausted %d retries", method, maxRetries)
}

// apiError maps a Slack method-level error string to a typed error.
func apiError(method string, envelope *apiResponse) error {
	switch {
	case notFoundAPIErrors[envelope.Error]:
		return fmt.Errorf("slack %s: %s: %w", method, envelope.Error, ErrNotFound)
	case authAPIErrors[envelope.Error]:
		detail := ""
		if envelope.Error == "missing_scope" {
			detail = fmt.Sprintf(" (needs scope %q, token has %q)", envelope.Needed, envelope.Provided)
		}
		return fmt.Errorf("slack %s: %s%s: re-run 'msgvault add-slack' with an updated token: %w",
			method, envelope.Error, detail, ErrAuth)
	default:
		return fmt.Errorf("slack %s: %s", method, envelope.Error)
	}
}

// retryAfter parses a Retry-After header (seconds) or falls back to
// exponential back-off capped at 60 s.
func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return min(time.Duration(1<<uint(attempt))*time.Second, 60*time.Second)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// AuthTest validates the token and identifies its workspace and user.
func (c *Client) AuthTest(ctx context.Context) (*AuthTestResult, error) {
	var out struct {
		apiResponse
		AuthTestResult
	}
	if err := c.call(ctx, "auth.test", url.Values{}, &out); err != nil {
		return nil, err
	}
	return &out.AuthTestResult, nil
}

// AllConversations pages through the user's conversation memberships of all
// four types, invoking fn per conversation.
func (c *Client) AllConversations(ctx context.Context, fn func(Conversation) error) error {
	cursor := ""
	for {
		params := url.Values{
			"types":            {"public_channel,private_channel,mpim,im"},
			"limit":            {strconv.Itoa(listPageLimit)},
			"exclude_archived": {"false"},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			apiResponse

			Channels []Conversation `json:"channels"`
		}
		if err := c.call(ctx, "users.conversations", params, &out); err != nil {
			return err
		}
		for i := range out.Channels {
			if err := fn(out.Channels[i]); err != nil {
				return err
			}
		}
		cursor = out.Metadata.NextCursor
		if cursor == "" {
			return nil
		}
	}
}

// AllUsers pages through the workspace member list, invoking fn per user.
func (c *Client) AllUsers(ctx context.Context, fn func(User) error) error {
	cursor := ""
	for {
		params := url.Values{"limit": {strconv.Itoa(listPageLimit)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			apiResponse

			Members []User `json:"members"`
		}
		if err := c.call(ctx, "users.list", params, &out); err != nil {
			return err
		}
		for i := range out.Members {
			if err := fn(out.Members[i]); err != nil {
				return err
			}
		}
		cursor = out.Metadata.NextCursor
		if cursor == "" {
			return nil
		}
	}
}

// AllMembers pages through a conversation's member user IDs.
func (c *Client) AllMembers(ctx context.Context, channelID string, fn func(userID string) error) error {
	cursor := ""
	for {
		params := url.Values{
			"channel": {channelID},
			"limit":   {strconv.Itoa(listPageLimit)},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			apiResponse

			Members []string `json:"members"`
		}
		if err := c.call(ctx, "conversations.members", params, &out); err != nil {
			return err
		}
		for _, id := range out.Members {
			if err := fn(id); err != nil {
				return err
			}
		}
		cursor = out.Metadata.NextCursor
		if cursor == "" {
			return nil
		}
	}
}

// HistoryPage is one page of conversations.history / conversations.replies.
type HistoryPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor string
}

// HistoryParams selects a slice of a conversation's history.
type HistoryParams struct {
	ChannelID string
	Cursor    string // opaque page cursor; mutually advancing with Oldest
	Oldest    string // exclusive lower ts bound ("" = beginning)
	Latest    string // exclusive upper ts bound ("" = now)
}

// HistoryPage fetches one page of top-level channel history. Slack pages
// newest→oldest without a cursor; the importer requests oldest-bounded
// windows and walks pages via NextCursor.
func (c *Client) HistoryPage(ctx context.Context, p HistoryParams) (*HistoryPage, error) {
	params := url.Values{
		"channel":   {p.ChannelID},
		"limit":     {strconv.Itoa(historyPageLimit)},
		"inclusive": {"false"},
	}
	if p.Cursor != "" {
		params.Set("cursor", p.Cursor)
	}
	if p.Oldest != "" {
		params.Set("oldest", p.Oldest)
	}
	if p.Latest != "" {
		params.Set("latest", p.Latest)
	}
	var out struct {
		apiResponse

		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
	}
	if err := c.call(ctx, "conversations.history", params, &out); err != nil {
		return nil, err
	}
	return &HistoryPage{Messages: out.Messages, HasMore: out.HasMore, NextCursor: out.Metadata.NextCursor}, nil
}

// RepliesPage fetches one page of a thread's replies (rootTS = the thread
// root's ts). The response includes the root itself as the first message of
// the first page; callers must skip or idempotently re-upsert it.
func (c *Client) RepliesPage(ctx context.Context, channelID, rootTS, cursor, oldest string) (*HistoryPage, error) {
	params := url.Values{
		"channel": {channelID},
		"ts":      {rootTS},
		"limit":   {strconv.Itoa(historyPageLimit)},
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	if oldest != "" {
		params.Set("oldest", oldest)
		params.Set("inclusive", "false")
	}
	var out struct {
		apiResponse

		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
	}
	if err := c.call(ctx, "conversations.replies", params, &out); err != nil {
		return nil, err
	}
	return &HistoryPage{Messages: out.Messages, HasMore: out.HasMore, NextCursor: out.Metadata.NextCursor}, nil
}
