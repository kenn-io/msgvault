package gcal

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v7"
	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/gmail"
)

const (
	defaultBaseURL = "https://www.googleapis.com/calendar/v3"
	maxRetries     = 12  // ~10 minutes of network outages, matching the Gmail client
	maxBackoff     = 600 // seconds
	// defaultMaxResults is the events.list page size. 2500 is the API max,
	// minimizing round-trips (and quota cost) on large calendars.
	defaultMaxResults = 2500
)

// Client implements Calendar reads and explicitly requested event writes.
type Client struct {
	httpClient  *http.Client
	rateLimiter *gmail.RateLimiter
	logger      *slog.Logger
	baseURL     string
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithRateLimiter overrides the default Calendar rate limiter.
func WithRateLimiter(rl *gmail.RateLimiter) ClientOption {
	return func(c *Client) {
		if rl != nil {
			c.rateLimiter = rl
		}
	}
}

// WithBaseURL overrides the API base URL. Useful for pointing the client at a
// test server or a proxy/regional endpoint.
func WithBaseURL(u string) ClientOption {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithHTTPClient overrides the HTTP client. The supplied client is used as-is,
// so the caller is responsible for attaching credentials when needed (the test
// server accepts any token).
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

type tokenSourceFailureError struct{ err error }

func (e *tokenSourceFailureError) Error() string { return e.err.Error() }
func (e *tokenSourceFailureError) Unwrap() error { return e.err }

type classifiedTokenSource struct{ source oauth2.TokenSource }

func (s classifiedTokenSource) Token() (*oauth2.Token, error) {
	token, err := s.source.Token()
	if err != nil {
		return nil, &tokenSourceFailureError{err: err}
	}
	return token, nil
}

// NewClient creates a Calendar client. The token source is wrapped with
// oauth2.NewClient so the bearer token auto-attaches and auto-refreshes; the
// client never touches token internals. The default rate limiter is sized for
// the Calendar API's per-user budget (600 req/min/user ≈ 10 req/s), not Gmail's.
func NewClient(tokenSource oauth2.TokenSource, opts ...ClientOption) *Client {
	if tokenSource != nil {
		tokenSource = classifiedTokenSource{source: tokenSource}
	}
	c := &Client{
		httpClient:  oauth2.NewClient(context.Background(), tokenSource),
		logger:      slog.Default(),
		baseURL:     defaultBaseURL,
		rateLimiter: gmail.NewRateLimiterWithCapacity(10, 8),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.rateLimiter == nil {
		c.rateLimiter = gmail.NewRateLimiterWithCapacity(10, 8)
	}
	return c
}

// Close releases resources held by the client.
func (c *Client) Close() error { return nil }

// NotFoundError indicates a 404 response (e.g. a deleted event on GetEvent).
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string { return "not found: " + e.Path }

// GoneError indicates an HTTP 410 — the Calendar analogue of Gmail's 404 on a
// stale historyId. It means the supplied syncToken has expired and the caller
// must clear the cursor and run a fresh full sync.
type GoneError struct{ Path string }

func (e *GoneError) Error() string { return "gone (410, sync token expired): " + e.Path }

// request performs a rate-limited HTTP request with retry/backoff, mirroring the
// Gmail client's loop. It retries network errors, 429, quota-403, and 5xx with
// full-jitter exponential backoff; it does not retry permission-403, 401, 404,
// 410, or other 4xx. The op selects the quota cost on the shared limiter.
func (c *Client) request(ctx context.Context, op gmail.Operation, path string) ([]byte, error) {
	if err := c.rateLimiter.Acquire(ctx, op); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	reqURL := c.baseURL + path

	policy := backoff.NewExponentialBackOff()
	// The former full-jitter ceiling starts at two seconds.
	policy.InitialInterval = time.Second
	policy.MaxInterval = maxBackoff * time.Second / 2
	policy.Multiplier = 2
	policy.RandomizationFactor = 1
	attempt := -1
	body, err := backoff.Retry(ctx, func() ([]byte, error) {
		attempt++

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, io.Reader(nil))
		if err != nil {
			return nil, backoff.Permanent(fmt.Errorf("create request: %w", err))
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// A token-source failure (e.g. a service account whose domain-wide
			// delegation lacks the Calendar scope → 401 unauthorized_client)
			// surfaces here as a transport error. A credential problem is not
			// a network blip: retrying it would hide the real cause behind ten
			// minutes of backoff, so fail immediately with Google's message.
			// A 429 or 5xx from the token endpoint itself is transient and
			// keeps the normal retry policy.
			var rerr *oauth2.RetrieveError
			if errors.As(err, &rerr) && !isTransientTokenError(rerr) {
				return nil, backoff.Permanent(fmt.Errorf("oauth token for calendar request: %w", err))
			}
			return nil, fmt.Errorf("http request: %w", err)
		}

		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			c.logger.Debug("calendar rate limited, backing off 30s", "path", path, "attempt", attempt)
			c.rateLimiter.Throttle(30 * time.Second)
			return nil, errors.New("rate limited (429)")

		case http.StatusForbidden:
			if isRateLimitError(respBody) {
				c.logger.Debug("calendar quota exceeded, backing off 60s", "path", path, "attempt", attempt)
				c.rateLimiter.Throttle(60 * time.Second)
				return nil, errors.New("quota exceeded (403)")
			}
			return nil, backoff.Permanent(fmt.Errorf("forbidden (403): %s", string(respBody)))

		case http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, fmt.Errorf("server error (%d)", resp.StatusCode)

		case http.StatusUnauthorized:
			return nil, backoff.Permanent(errors.New("unauthorized (401): token may be invalid"))

		case http.StatusGone:
			return nil, backoff.Permanent(&GoneError{Path: path})

		case http.StatusNotFound:
			return nil, backoff.Permanent(&NotFoundError{Path: path})

		default:
			return nil, backoff.Permanent(fmt.Errorf("request failed (%d): %s", resp.StatusCode, string(respBody)))
		}
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(maxRetries+1), backoff.WithMaxElapsedTime(0),
		backoff.WithNotify(func(_ error, delay time.Duration) {
			c.logger.Debug("retrying calendar request", "attempt", attempt+1, "backoff", delay, "path", path)
		}))
	if err == nil {
		return body, nil
	}
	retryErr := backoff.AsRetryError(err)
	if errors.Is(retryErr.Cause, backoff.ErrPermanent) {
		return nil, retryErr.LastErr
	}
	if !errors.Is(retryErr.Cause, backoff.ErrExhausted) {
		return nil, ctx.Err()
	}
	return nil, fmt.Errorf("max retries exceeded: %w", retryErr.LastErr)
}

// isTransientTokenError reports whether the token endpoint failed with a
// status worth retrying (429 or 5xx) rather than rejecting the credentials.
func isTransientTokenError(rerr *oauth2.RetrieveError) bool {
	if rerr.Response == nil {
		return false
	}
	code := rerr.Response.StatusCode
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// isRateLimitError reports whether a 403 body is a quota/rate-limit error
// (retryable) rather than a genuine permission error (terminal). Calendar uses
// the same error envelope as Gmail: error.errors[].reason / error.status.
func isRateLimitError(body []byte) bool {
	var parsed struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
				Domain string `json:"domain"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	if parsed.Error.Status == "RESOURCE_EXHAUSTED" {
		return true
	}
	for _, e := range parsed.Error.Errors {
		switch e.Reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "RATE_LIMIT_EXCEEDED":
			return true
		case "accessNotConfigured":
			// Google files "Calendar API is not enabled in this project" under
			// domain usageLimits too, but it is a configuration error that no
			// amount of backoff fixes — surface it immediately.
			return false
		}
		if e.Domain == "usageLimits" {
			return true
		}
	}
	return false
}

// ListCalendars returns one page of the account's calendar list.
func (c *Client) ListCalendars(ctx context.Context, pageToken string) (*CalendarListPage, error) {
	v := url.Values{}
	v.Set("maxResults", "250")
	v.Set("showHidden", "true")
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	body, err := c.request(ctx, gmail.OpCalendarListList, "/users/me/calendarList?"+v.Encode())
	if err != nil {
		return nil, fmt.Errorf("calendarList.list: %w", err)
	}

	var wire wireCalendarList
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode calendarList: %w", err)
	}
	page := &CalendarListPage{NextPageToken: wire.NextPageToken}
	for i := range wire.Items {
		page.Items = append(page.Items, wire.Items[i].toCalendar())
	}
	return page, nil
}

// ListEvents returns one page of events for a calendar. See EventsListParams.
func (c *Client) ListEvents(ctx context.Context, calendarID string, p EventsListParams) (*EventsPage, error) {
	v := url.Values{}
	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	v.Set("maxResults", strconv.Itoa(maxResults))
	v.Set("singleEvents", strconv.FormatBool(p.SingleEvents))
	if p.ShowDeleted {
		v.Set("showDeleted", "true")
	}
	if p.SyncToken != "" {
		// Incremental: syncToken is mutually exclusive with timeMin/timeMax.
		v.Set("syncToken", p.SyncToken)
	} else {
		if p.TimeMin != "" {
			v.Set("timeMin", p.TimeMin)
		}
		if p.TimeMax != "" {
			v.Set("timeMax", p.TimeMax)
		}
	}
	if p.PageToken != "" {
		v.Set("pageToken", p.PageToken)
	}

	path := "/calendars/" + url.PathEscape(calendarID) + "/events?" + v.Encode()
	body, err := c.request(ctx, gmail.OpEventsList, path)
	if err != nil {
		return nil, fmt.Errorf("events.list: %w", err)
	}

	return decodeEventsPage(body)
}

func decodeEventsPage(body []byte) (*EventsPage, error) {
	var wire wireEvents
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("decode events: %w", err)
	}
	page := &EventsPage{
		NextPageToken: wire.NextPageToken,
		NextSyncToken: wire.NextSyncToken,
		TimeZone:      wire.TimeZone,
	}
	for _, raw := range wire.Items {
		ev, err := decodeEvent(raw)
		if err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		page.Items = append(page.Items, ev)
	}
	return page, nil
}

// GetEvent fetches a single event by id.
func (c *Client) GetEvent(ctx context.Context, calendarID, eventID string) (*Event, error) {
	path := "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID)
	body, err := c.request(ctx, gmail.OpEventsGet, path)
	if err != nil {
		return nil, fmt.Errorf("events.get: %w", err)
	}
	ev, err := decodeEvent(body)
	if err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	return &ev, nil
}
