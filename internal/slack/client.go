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
	"search.messages":       2,
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
	// Strict method allowlist: reqURL is base + method, and the G704
	// suppressions below rest on method being one of these package
	// constants. Refusing unknown methods enforces that at runtime instead
	// of by convention (and no new method can dodge tier classification).
	tier, ok := methodTiers[method]
	if !ok {
		return fmt.Errorf("slack client: method %q is not allowlisted", method)
	}
	reqURL := c.baseURL + "/" + method
	body := params.Encode()
	for attempt := range maxRetries {
		if err := c.limiters[tier].Wait(ctx); err != nil {
			return fmt.Errorf("wait for slack rate limit: %w", err)
		}
		// reqURL is the constant Slack API base (or a test-injected httptest
		// URL) plus an allowlisted package-constant method name (enforced
		// above) — no remote or user input reaches it. gosec's taint
		// analysis flags it via the env-var-driven live-probe test.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body)) // #nosec G704
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req) // #nosec G704 -- see reqURL above
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
	Oldest    string // lower ts bound ("" = beginning)
	Latest    string // upper ts bound ("" = now)
	// Inclusive makes the Oldest/Latest bounds inclusive (the edit rescan
	// must re-read the cursor message itself; default exclusive bounds are
	// what cursor-advancing walks need).
	Inclusive bool
}

// HistoryPage fetches one page of top-level channel history. Slack pages
// newest→oldest without a cursor; the importer requests oldest-bounded
// windows and walks pages via NextCursor.
func (c *Client) HistoryPage(ctx context.Context, p HistoryParams) (*HistoryPage, error) {
	return c.historyPageWithLimit(ctx, p, historyPageLimit)
}

// historyPageWithLimit is HistoryPage with an explicit page size (the
// importer sizes pages to the remaining --limit budget; the live throttle
// probe uses tiny pages).
func (c *Client) historyPageWithLimit(ctx context.Context, p HistoryParams, limit int) (*HistoryPage, error) {
	params := url.Values{
		"channel":   {p.ChannelID},
		"limit":     {strconv.Itoa(limit)},
		"inclusive": {strconv.FormatBool(p.Inclusive)},
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

// RepliesPage fetches one page of a thread's replies. rootTS must be the
// thread ROOT's ts for full-thread results: probed live, a reply's ts
// serves ONLY that reply (no bound or limit expands it), while a root
// anchor serves the whole thread with the parent first — included even
// below an oldest bound. Callers must skip or idempotently re-upsert the
// included root, and re-anchor via a returned reply's thread_ts when they
// only had a reply ts to start from (see drainPendingThreads).
func (c *Client) RepliesPage(ctx context.Context, channelID, rootTS, cursor, oldest string) (*HistoryPage, error) {
	return c.repliesPageWithLimit(ctx, channelID, rootTS, cursor, oldest, historyPageLimit)
}

// searchPageLimit is the result page size for search.messages (its maximum).
const searchPageLimit = 100

// maxSearchPages is the server-side page-number ceiling on search.messages.
// Requests beyond it are silently CLAMPED to page 1 (probed live), so the
// pager must both bound itself here and verify the echoed page number.
const maxSearchPages = 100

// SearchMatch is one search.messages hit, reduced to the fields the sweep
// consumes. Search results are never archived directly (they are not native
// message objects); they are pointers for canonical conversations.replies
// fetches.
type SearchMatch struct {
	ChannelID string
	TS        string
	// RootTS is the thread root ts parsed from the permalink's thread_ts
	// query parameter ("" when unparseable — the hit then becomes a solo
	// drain entry anchored at the reply itself, which the drain re-anchors
	// to the true root from the fetched reply's own thread_ts).
	RootTS string
}

// SearchPage is one page of search.messages results.
type SearchPage struct {
	Matches []SearchMatch
	// Page is the ECHOED page number: when it differs from the requested
	// page, the server clamped the request (past its page ceiling) and the
	// walk must stop.
	Page  int
	Pages int
	Total int
}

// SearchMessagesPage runs one page of a search.messages query, always
// timestamp-ascending (the sweep watermark requires stable ascending order;
// probed stable across full walks with no duplicates).
func (c *Client) SearchMessagesPage(ctx context.Context, query string, page int) (*SearchPage, error) {
	params := url.Values{
		"query":    {query},
		"count":    {strconv.Itoa(searchPageLimit)},
		"page":     {strconv.Itoa(page)},
		"sort":     {"timestamp"},
		"sort_dir": {"asc"},
	}
	var out struct {
		apiResponse

		Messages struct {
			Total  int `json:"total"`
			Paging struct {
				Page  int `json:"page"`
				Pages int `json:"pages"`
			} `json:"paging"`
			Matches []struct {
				TS      string `json:"ts"`
				Channel struct {
					ID string `json:"id"`
				} `json:"channel"`
				Permalink string `json:"permalink"`
			} `json:"matches"`
		} `json:"messages"`
	}
	if err := c.call(ctx, "search.messages", params, &out); err != nil {
		return nil, err
	}
	sp := &SearchPage{
		Page:  out.Messages.Paging.Page,
		Pages: out.Messages.Paging.Pages,
		Total: out.Messages.Total,
	}
	for _, m := range out.Messages.Matches {
		sp.Matches = append(sp.Matches, SearchMatch{
			ChannelID: m.Channel.ID,
			TS:        m.TS,
			RootTS:    permalinkThreadTS(m.Permalink),
		})
	}
	return sp, nil
}

// ValidateSearchScope verifies the token can call search.messages (user
// scope search:read), which the reply sweep depends on. Run at add-slack
// time so an under-scoped token fails setup with instructions instead of
// failing every future sync's sweep.
func (c *Client) ValidateSearchScope(ctx context.Context) error {
	if _, err := c.SearchMessagesPage(ctx, "msgvault scope check", 1); err != nil {
		if errors.Is(err, ErrAuth) {
			return fmt.Errorf("token cannot use search.messages, which reply archiving requires — add the search:read user scope, reinstall the app, and retry with the new token: %w", err)
		}
		return fmt.Errorf("verify search.messages access: %w", err)
	}
	return nil
}

// permalinkThreadTS extracts the thread_ts query parameter from a message
// permalink ("" when absent or unparseable). Used only as a grouping
// optimization — never as the source of truth for thread membership.
func permalinkThreadTS(permalink string) string {
	u, err := url.Parse(permalink)
	if err != nil {
		return ""
	}
	return u.Query().Get("thread_ts")
}

// repliesPageWithLimit is RepliesPage with an explicit page size (sized to
// the importer's remaining --limit budget).
func (c *Client) repliesPageWithLimit(ctx context.Context, channelID, rootTS, cursor, oldest string, limit int) (*HistoryPage, error) {
	params := url.Values{
		"channel": {channelID},
		"ts":      {rootTS},
		"limit":   {strconv.Itoa(limit)},
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
