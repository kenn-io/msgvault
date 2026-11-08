package forge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

const (
	// maxRetryWait is the longest a request waits for a rate limit or a
	// retry; a longer wait pauses the run instead.
	maxRetryWait = time.Minute
	// retryAttempts bounds how often one request is sent.
	retryAttempts = 4
	// maxErrorBodyBytes bounds the response text an error quotes.
	maxErrorBodyBytes = 512
	// maxResponseBytes bounds one API response.
	maxResponseBytes = 64 << 20
)

// API sends one forge's API requests with an account's token. It sends the
// token only to the forge's own origin, retries server errors and short rate
// limits, and returns a RateLimitError for a limit that resets later, so a
// sync pauses instead of blocking.
type API struct {
	// Base is the API's base URL; paths resolve against it.
	Base   *url.URL
	Client *http.Client
	// Authorize adds the token to a request.
	Authorize func(*http.Request)
	UserAgent string
	// Limited reports whether a response was refused for its rate, and when
	// the limit resets; a zero time means unknown.
	Limited func(*http.Response) (time.Time, bool)
	// Observe sees every response's headers, such as a forge's rate-limit
	// counters.
	Observe func(*http.Response)

	now func() time.Time
}

// URL resolves path against the API's base. An absolute URL, such as a
// pagination link the forge returned, must be on the API's own origin.
func (a *API) URL(path string, query url.Values) (string, error) {
	var target *url.URL
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		parsed, err := url.Parse(path)
		if err != nil || parsed.Scheme != a.Base.Scheme || !strings.EqualFold(parsed.Host, a.Base.Host) {
			return "", fmt.Errorf("the forge returned a URL outside its API: %s", path)
		}
		target = parsed
	} else {
		target = a.Base.JoinPath(path)
	}
	if len(query) > 0 {
		merged := target.Query()
		maps.Copy(merged, query)
		target.RawQuery = merged.Encode()
	}
	return target.String(), nil
}

// Do sends a request and returns a 2xx or 304 response, with its body open.
// A 401 is ErrUnauthorized, a 404 or 410 ErrUnavailable, and a rate limit
// that doesn't reset within maxRetryWait a RateLimitError.
func (a *API) Do(ctx context.Context, method, target string, body []byte, header http.Header) (*http.Response, error) {
	now := a.now
	if now == nil {
		now = time.Now
	}
	resp, err := httpretry.Do(ctx, retryAttempts, maxRetryWait, func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		maps.Copy(req.Header, header)
		if a.UserAgent != "" {
			req.Header.Set("User-Agent", a.UserAgent)
		}
		if a.Authorize != nil {
			a.Authorize(req)
		}
		resp, err := a.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("forge request: %w", err)
		}
		if a.Observe != nil {
			a.Observe(resp)
		}
		return resp, nil
	}, func(resp *http.Response) bool {
		if reset, limited := a.limited(resp); limited {
			// A limit that resets soon is waited out; a later one pauses
			// the run.
			return !reset.IsZero() && reset.Sub(now()) <= maxRetryWait
		}
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError
	})
	if err != nil {
		return nil, err
	}
	if reset, limited := a.limited(resp); limited {
		drain(resp)
		return nil, &RateLimitError{Resource: "api", Reset: reset}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer drain(resp)
	return nil, StatusError(resp)
}

func (a *API) limited(resp *http.Response) (time.Time, bool) {
	if a.Limited == nil {
		return time.Time{}, false
	}
	return a.Limited(resp)
}

// Response is what a decoded GET reports besides its body.
type Response struct {
	Status int
	Header http.Header
}

// GetJSON sends a GET and decodes a 200 response into out; a 304 leaves out
// untouched.
func (a *API) GetJSON(ctx context.Context, path string, query url.Values, header http.Header, out any) (Response, error) {
	target, err := a.URL(path, query)
	if err != nil {
		return Response{}, err
	}
	resp, err := a.Do(ctx, http.MethodGet, target, nil, header)
	if err != nil {
		return Response{}, err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusOK {
		if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), out); err != nil {
			return Response{}, fmt.Errorf("decode %s: %w", redactQuery(target), err)
		}
	}
	return Response{Status: resp.StatusCode, Header: resp.Header}, nil
}

// HTTPError is a forge API's refusal of a request. A 401 is
// ErrUnauthorized and a 404 or 410 ErrUnavailable, through errors.Is.
type HTTPError struct {
	Status int
	// Request is the method and path, never the query or the token.
	Request string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: %d %s: %s", e.Request, e.Status, http.StatusText(e.Status), e.Message)
}

func (e *HTTPError) Unwrap() error {
	switch e.Status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound, http.StatusGone:
		return ErrUnavailable
	default:
		return nil
	}
}

// StatusError reads a refused response into an *HTTPError.
func StatusError(resp *http.Response) error {
	text, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var payload struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(text))
	if json.Unmarshal(text, &payload) == nil && payload.Message != "" {
		message = payload.Message
	}
	return &HTTPError{Status: resp.StatusCode, Request: resp.Request.Method + " " + resp.Request.URL.Path, Message: message}
}

// NextLink returns the rel="next" URL of a Link header, or "".
func NextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

func redactQuery(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "request"
	}
	return parsed.Path
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
