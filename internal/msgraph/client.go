// Package msgraph is the Microsoft Graph REST transport shared by the Teams and
// mail connectors: bearer auth, a token-bucket rate limit, Retry-After back-off
// and @odata paging.
package msgraph

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/time/rate"
)

// ErrTooLarge classifies a response body that exceeds the caller's byte cap.
var ErrTooLarge = errors.New("graph response exceeds the configured size cap")

// ErrNotFound classifies a 404 response.
var ErrNotFound = errors.New("graph resource not found")

// ErrForbidden classifies a 403 response, for example a token that lacks the
// scope a write needs.
var ErrForbidden = errors.New("graph request forbidden")

// ErrUnauthorized classifies a 401 response, for example a revoked token.
var ErrUnauthorized = errors.New("graph request unauthorized")

// ThrottledError reports that Graph still answered 429 after every retry.
// RetryAfter is the delay that Graph asked for last.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("graph throttled the request; retry after %s", e.RetryAfter)
}

// StatusError carries the HTTP status of a response that no other error
// classifies, for example a 400 validation failure.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("graph status %d", e.StatusCode)
}

// ErrPreconditionFailed classifies a write that failed its If-Match check: a
// 412, or a 400 ErrorInvalidChangeKey for a malformed change key.
var ErrPreconditionFailed = errors.New("graph precondition failed")

// ErrGone classifies an expired delta token: 410 Gone, or a syncStateNotFound
// error. The caller must restart the delta walk without a token.
var ErrGone = errors.New("graph delta token expired")

// ErrNotSent marks a request that failed before it was sent, for example
// because the rate limiter's wait would outlast the deadline.
var ErrNotSent = errors.New("graph request not sent")

const (
	maxRetries    = 8
	maxRetryAfter = httpretry.ProviderMaxRetryAfter
)

// TokenFunc returns a bearer token for a Graph API request.
type TokenFunc func(context.Context) (string, error)

// Client is a minimal Microsoft Graph REST client supporting paging and
// Retry-After back-off.
type Client struct {
	baseURL string
	token   TokenFunc
	http    *http.Client
	limiter *rate.Limiter

	// Headers are extra request headers, for example the mail connector's
	// Prefer header. They are set after the defaults and can replace them.
	Headers map[string]string
}

// NewClient creates a Client. baseURL is injected so tests can point at
// httptest servers. qps controls the token-bucket rate limit (default 5).
func NewClient(baseURL string, token TokenFunc, qps float64) *Client {
	if qps <= 0 {
		qps = 5
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(qps), 1),
	}
}

// get fetches rawURL, respecting the rate limiter and retrying on 429/5xx with
// Retry-After or exponential back-off.
func (c *Client) get(ctx context.Context, rawURL string) ([]byte, error) {
	return c.getLimited(ctx, rawURL, 0)
}

func (c *Client) getLimited(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	return c.do(ctx, http.MethodGet, rawURL, nil, maxBytes, "", retryAll)
}

// do sends one request with retries. A GET succeeds on 200 only. Any other
// method succeeds on any 2xx, because Graph answers a move with 201 and a
// permanentDelete with 204. A non-nil body is sent as JSON. A non-empty ifMatch
// is sent as If-Match. With retryThrottle, only a 429 is retried, because
// Graph applied nothing; a network error or a 5xx can follow an applied
// write. With retryNone, nothing is retried.
func (c *Client) do(ctx context.Context, method, rawURL string, reqBody []byte, maxBytes int64, ifMatch string, mode retryMode) ([]byte, error) {
	once := mode != retryAll
	reqURL, err := c.resolveRequestURL(rawURL)
	if err != nil {
		return nil, err
	}
	ok := func(code int) bool {
		if method == http.MethodGet {
			return code == http.StatusOK
		}
		return code >= 200 && code < 300
	}
	var lastErr error
	var retryAfter string
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := httpretry.RetryAfter(retryAfter, attempt-1, maxRetryAfter)
			// A wait that outlasts the caller's deadline would end as a
			// cancellation and lose the throttling status.
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
				return nil, fmt.Errorf("graph %s %s: retry wait exceeds the deadline: %w", method, reqURL, lastErr)
			}
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, notSent(err, attempt, mode)
			}
		}
		retryAfter = ""
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for graph rate limit: %w", notSent(err, attempt, mode))
		}
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		var reqReader io.Reader
		if reqBody != nil {
			reqReader = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, reqReader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr.IsNotFound {
				return nil, err
			}
			if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
				return nil, err
			}
			if once {
				return nil, err
			}
			lastErr = err
			continue
		}
		if ok(resp.StatusCode) && maxBytes > 0 && resp.ContentLength > maxBytes {
			_ = resp.Body.Close()
			return nil, ErrTooLarge
		}
		reader := io.Reader(resp.Body)
		if maxBytes > 0 {
			reader = io.LimitReader(resp.Body, maxBytes+1)
		}
		body, readErr := io.ReadAll(reader)
		closeErr := resp.Body.Close()
		if readErr != nil {
			// A connection that breaks mid-body is transient, like a 5xx.
			lastErr = fmt.Errorf("graph %s %s: read body: %w", method, reqURL, readErr)
			if once {
				return nil, lastErr
			}
			continue
		}
		if closeErr != nil {
			return nil, fmt.Errorf("graph %s %s: close body: %w", method, reqURL, closeErr)
		}
		var graphError struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if resp.StatusCode >= http.StatusBadRequest {
			_ = json.Unmarshal(body, &graphError)
		}
		expired := graphError.Error.Code == "syncStateNotFound"
		switch {
		case ok(resp.StatusCode):
			if maxBytes > 0 && int64(len(body)) > maxBytes {
				return nil, ErrTooLarge
			}
			return body, nil
		case resp.StatusCode == http.StatusGone || expired:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrGone)
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrNotFound)
		case resp.StatusCode == http.StatusPreconditionFailed || graphError.Error.Code == "ErrorInvalidChangeKey":
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrPreconditionFailed)
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrUnauthorized)
		case resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrForbidden)
		case resp.StatusCode >= 500 && once:
			return nil, fmt.Errorf("graph %s %s: status %d: %s", method, reqURL, resp.StatusCode, string(body))
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("graph %s %s: status %d", method, reqURL, resp.StatusCode)
			retryAfter = resp.Header.Get("Retry-After")
			if resp.StatusCode == http.StatusTooManyRequests {
				lastErr = fmt.Errorf("%w: %w", lastErr, &ThrottledError{
					RetryAfter: httpretry.RetryAfter(retryAfter, attempt, maxRetryAfter),
				})
				if mode == retryNone {
					return nil, lastErr
				}
			}
			continue
		default:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), &StatusError{StatusCode: resp.StatusCode})
		}
	}
	return nil, fmt.Errorf("graph %s %s: exhausted %d retries: %w", method, reqURL, maxRetries, lastErr)
}

// notSent adds ErrNotSent to a failure before a send when no attempt could
// have applied the request: none was sent yet, or each one was a 429, which
// retryThrottle alone repeats.
func notSent(err error, attempt int, mode retryMode) error {
	if attempt == 0 || mode == retryThrottle {
		return errors.Join(err, ErrNotSent)
	}
	return err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) resolveRequestURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("graph GET %q: parse URL: %w", rawURL, err)
	}
	if !u.IsAbs() {
		return c.baseURL + rawURL, nil
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("graph base URL %q: %w", c.baseURL, err)
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return "", fmt.Errorf("graph GET %s: off-origin absolute URL", rawURL)
	}
	return u.String(), nil
}

// GetRaw fetches url and returns the raw response bytes. url should be a
// path-relative string (e.g. "/me/messages/{id}/$value"); it is
// prefixed with the client's baseURL automatically by the underlying get method.
func (c *Client) GetRaw(ctx context.Context, url string) ([]byte, error) {
	return c.get(ctx, url)
}

// GetRawWithTimeout fetches raw bytes with a per-attempt deadline, including
// reading the body. Other requests on the client retain their usual timeout.
func (c *Client) GetRawWithTimeout(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	client := *c
	httpClient := *c.http
	httpClient.Timeout = timeout
	client.http = &httpClient
	return client.GetRaw(ctx, url)
}

// GetRawLimited fetches raw bytes while enforcing a response-byte cap.
func (c *Client) GetRawLimited(ctx context.Context, url string, maxBytes int64) ([]byte, error) {
	return c.getLimited(ctx, url, maxBytes)
}

// BaseURL returns the client's configured base URL (scheme + host, no trailing slash).
// Importers use this to rewrite absolute graph.microsoft.com URLs to the configured
// host (supporting both production and httptest servers).
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Post sends body as JSON to url and discards the response body. A nil body
// sends an empty request.
func (c *Client) Post(ctx context.Context, url string, body any) error {
	_, err := c.Send(ctx, http.MethodPost, url, body, "")
	return err
}

// Send sends body as JSON with method and returns the response body. A nil
// body sends an empty request. A non-empty ifMatch is sent as If-Match.
func (c *Client) Send(ctx context.Context, method, url string, body any, ifMatch string) ([]byte, error) {
	return c.send(ctx, method, url, body, ifMatch, retryAll)
}

// SendOnce is Send for a write that must not repeat: a create, or a
// conditional update whose repeat would fail its own If-Match. It retries
// only a 429.
func (c *Client) SendOnce(ctx context.Context, method, url string, body any, ifMatch string) ([]byte, error) {
	return c.send(ctx, method, url, body, ifMatch, retryThrottle)
}

// SendNoRetry is SendOnce without the 429 retry, for an unconditional write
// whose caller must check the resource again before repeating it, such as a
// DELETE, which Graph cannot make conditional.
func (c *Client) SendNoRetry(ctx context.Context, method, url string, body any, ifMatch string) ([]byte, error) {
	return c.send(ctx, method, url, body, ifMatch, retryNone)
}

// retryMode says which failures do repeats.
type retryMode int

const (
	retryAll      retryMode = iota // 429, 5xx and network errors
	retryThrottle                  // only a 429
	retryNone                      // nothing
)

func (c *Client) send(ctx context.Context, method, url string, body any, ifMatch string, mode retryMode) ([]byte, error) {
	var reqBody []byte
	if body != nil {
		var err error
		if reqBody, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("graph %s %s: encode body: %w", method, url, err)
		}
	}
	return c.do(ctx, method, url, reqBody, 0, ifMatch, mode)
}

// GetJSON fetches url and unmarshals the JSON body into out.
func (c *Client) GetJSON(ctx context.Context, url string, out any) error {
	body, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// ListResponse is the Graph collection envelope.
type ListResponse[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// PageThrough follows @odata.nextLink, decoding each page into []T, calling fn.
// Returns the terminal @odata.deltaLink (empty for non-delta endpoints).
func PageThrough[T any](ctx context.Context, c *Client, startURL string, fn func([]T)) (string, error) {
	delta, _, err := PageThroughLimit(ctx, c, startURL, 0, fn)
	return delta, err
}

// PageThroughLimit is PageThrough with an optional item cap. When limit is
// positive, it stops before fetching a nextLink once enough items have been
// delivered and reports whether unread items/pages remain.
func PageThroughLimit[T any](ctx context.Context, c *Client, startURL string, limit int, fn func([]T)) (string, bool, error) {
	return pageThrough(ctx, startURL, limit, c.get, fn)
}

// PageThroughFunc is PageThrough with get fetching each page, for a caller
// that gates or meters every request.
func PageThroughFunc[T any](ctx context.Context, startURL string, get func(context.Context, string) ([]byte, error), fn func([]T)) (string, error) {
	delta, _, err := pageThrough(ctx, startURL, 0, get, fn)
	return delta, err
}

func pageThrough[T any](ctx context.Context, startURL string, limit int, get func(context.Context, string) ([]byte, error), fn func([]T)) (string, bool, error) {
	url := startURL
	delivered := 0
	for {
		body, err := get(ctx, url)
		if err != nil {
			return "", false, err
		}
		var page ListResponse[T]
		if err := json.Unmarshal(body, &page); err != nil {
			return "", false, err
		}
		values := page.Value
		if limit > 0 {
			remaining := limit - delivered
			if remaining <= 0 {
				return "", true, nil
			}
			if len(values) > remaining {
				fn(values[:remaining])
				return "", true, nil
			}
			if len(values) == remaining && page.NextLink != "" {
				fn(values)
				return "", true, nil
			}
		}
		fn(values)
		delivered += len(values)
		if page.NextLink != "" {
			url = page.NextLink
			continue
		}
		return page.DeltaLink, false, nil
	}
}
