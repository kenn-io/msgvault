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

// ErrGone classifies an expired delta token: 410 Gone, or a syncStateNotFound
// error. The caller must restart the delta walk without a token.
var ErrGone = errors.New("graph delta token expired")

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
	return c.do(ctx, http.MethodGet, rawURL, nil, maxBytes)
}

// do sends one request with retries. A GET succeeds on 200 only. Any other
// method succeeds on any 2xx, because Graph answers a move with 201 and a
// permanentDelete with 204. A non-nil body is sent as JSON.
func (c *Client) do(ctx context.Context, method, rawURL string, reqBody []byte, maxBytes int64) ([]byte, error) {
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
			if err := sleepCtx(ctx, httpretry.RetryAfter(retryAfter, attempt-1, maxRetryAfter)); err != nil {
				return nil, err
			}
		}
		retryAfter = ""
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for graph rate limit: %w", err)
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
			continue
		}
		if closeErr != nil {
			return nil, fmt.Errorf("graph %s %s: close body: %w", method, reqURL, closeErr)
		}
		expired := false
		if resp.StatusCode >= http.StatusBadRequest {
			var graphError struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			expired = json.Unmarshal(body, &graphError) == nil && graphError.Error.Code == "syncStateNotFound"
		}
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
		case resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("graph %s %s: status %d: %s: %w", method, reqURL, resp.StatusCode, string(body), ErrForbidden)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("graph %s %s: status %d", method, reqURL, resp.StatusCode)
			retryAfter = resp.Header.Get("Retry-After")
			continue
		default:
			return nil, fmt.Errorf("graph %s %s: status %d: %s", method, reqURL, resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("graph %s %s: exhausted %d retries: %w", method, reqURL, maxRetries, lastErr)
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
	var reqBody []byte
	if body != nil {
		var err error
		if reqBody, err = json.Marshal(body); err != nil {
			return fmt.Errorf("graph POST %s: encode body: %w", url, err)
		}
	}
	_, err := c.do(ctx, http.MethodPost, url, reqBody, 0)
	return err
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
	url := startURL
	delivered := 0
	for {
		var page ListResponse[T]
		if err := c.GetJSON(ctx, url, &page); err != nil {
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
