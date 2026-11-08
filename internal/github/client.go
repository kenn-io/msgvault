// Package github archives the GitHub conversations an account takes part in:
// issues, pull requests with their reviews, discussions, and the releases
// of watched repositories. Each GitHub account is one "github" source. Its
// syncs find changed threads through the notifications API and an
// involvement search, then fetch each thread's content over GraphQL and
// write every post through the provider-neutral message import.
package github

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is GitHub's REST API; GraphQL is served at /graphql.
	DefaultBaseURL = "https://api.github.com"

	apiVersion = "2022-11-28"
	// maxErrorBodyBytes bounds the response text an error quotes.
	maxErrorBodyBytes = 512
	// maxRetryWait is the longest a request waits for a rate limit to reset
	// before giving the run back; longer waits end the run instead.
	maxRetryWait = time.Minute
	// maxRateLimitRetries bounds how often one request waits out a short
	// rate limit, such as a secondary limit's Retry-After.
	maxRateLimitRetries = 3
	// reserveFraction of each hourly rate limit is left for the account's
	// other tools, which share it: a backfill stops there and resumes on a
	// later sync instead of draining the account's limit.
	reserveFraction = 0.2
)

var (
	// ErrUnauthorized means GitHub rejected the token.
	ErrUnauthorized = errors.New("GitHub rejected the token; add the account again with msgvault add-github")
	// ErrClassicTokenRequired means the token cannot read notifications:
	// GitHub serves them only to classic personal access tokens.
	ErrClassicTokenRequired = errors.New("GitHub notifications need a classic personal access token with the notifications or repo scope")
	// ErrUnavailable means a thread is gone or the token may not read it.
	ErrUnavailable = errors.New("not available to this token")
)

// RateLimitError means a rate limit reached the reserve left for the
// account's other tools, or GitHub refused a request for its rate.
type RateLimitError struct {
	Resource string
	Reset    time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub %s rate limit reached; it resets at %s",
		e.Resource, e.Reset.UTC().Format(time.RFC3339))
}

// Client calls GitHub's REST and GraphQL APIs with one token.
type Client struct {
	http      *http.Client
	baseURL   string
	token     string
	userAgent string
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error

	mu     sync.Mutex
	limits map[string]rateLimit
}

// rateLimit is one rate-limit resource as the last response reported it.
type rateLimit struct {
	remaining int
	limit     int
	reset     time.Time
}

// NewClient calls the API at baseURL, or DefaultBaseURL when it is empty.
func NewClient(baseURL, token, userAgent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		http:      &http.Client{Timeout: 60 * time.Second},
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		userAgent: userAgent,
		now:       time.Now,
		sleep:     sleepContext,
		limits:    map[string]rateLimit{},
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Viewer is the account a token belongs to.
type Viewer struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name"`
}

// Viewer returns the token's account and checks that the token can read
// notifications: a classic token with the notifications or repo scope.
func (c *Client) Viewer(ctx context.Context) (Viewer, error) {
	var viewer Viewer
	resp, err := c.getJSON(ctx, "/user", nil, nil, &viewer)
	if err != nil {
		return Viewer{}, err
	}
	scopes, classic := resp.Header["X-Oauth-Scopes"]
	if !classic || !hasNotificationScope(strings.Join(scopes, ",")) {
		return Viewer{}, ErrClassicTokenRequired
	}
	if viewer.ID == 0 || viewer.Login == "" {
		return Viewer{}, errors.New("GitHub returned no account for the token")
	}
	return viewer, nil
}

func hasNotificationScope(header string) bool {
	for scope := range strings.SplitSeq(header, ",") {
		switch strings.TrimSpace(scope) {
		case "notifications", "repo":
			return true
		}
	}
	return false
}

// getJSON sends a REST GET and decodes a 200 response into out. Other
// statuses are errors, except 304, which returns the response with out
// untouched. path is an API path or an absolute URL GitHub returned, such
// as a pagination link; an absolute URL must be on the API's own host, so
// the token never leaves it.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, header http.Header, out any) (*http.Response, error) {
	target := c.baseURL + path
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		if !c.sameOrigin(path) {
			return nil, fmt.Errorf("GitHub returned a URL outside its API: %s", path)
		}
		target = path
	}
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	resp, err := c.do(ctx, "core", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		for key, values := range header {
			req.Header[key] = values
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.UnmarshalRead(resp.Body, out); err != nil {
			return nil, fmt.Errorf("decode GitHub %s: %w", path, err)
		}
		return resp, nil
	case http.StatusNotModified:
		return resp, nil
	default:
		return nil, statusError(resp, path)
	}
}

func (c *Client) sameOrigin(raw string) bool {
	target, err1 := url.Parse(raw)
	base, err2 := url.Parse(c.baseURL)
	return err1 == nil && err2 == nil && target.Scheme == base.Scheme && strings.EqualFold(target.Host, base.Host)
}

// graphQLRequest is one GraphQL call. OperationName names the query so
// that errors and logs say which one failed.
type graphQLRequest struct {
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
}

type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// graphQL runs a query and decodes its data into out. When every error
// says an object is missing or forbidden, the data that came back is
// decoded, with the missing objects null; without data the query returns
// ErrUnavailable.
func (c *Client) graphQL(ctx context.Context, request graphQLRequest, out any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode GitHub %s query: %w", request.OperationName, err)
	}
	resp, err := c.do(ctx, "graphql", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/graphql", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(resp, request.OperationName)
	}
	var envelope struct {
		Data   jsontext.Value `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if err := json.UnmarshalRead(resp.Body, &envelope); err != nil {
		return fmt.Errorf("decode GitHub %s response: %w", request.OperationName, err)
	}
	if len(envelope.Errors) > 0 {
		err := graphQLErrors(request.OperationName, envelope.Errors)
		if !errors.Is(err, ErrUnavailable) || len(envelope.Data) == 0 || envelope.Data.Kind() == 'n' {
			return err
		}
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode GitHub %s data: %w", request.OperationName, err)
	}
	return nil
}

func graphQLErrors(operation string, errs []graphQLError) error {
	unavailable := true
	messages := make([]string, 0, len(errs))
	for _, e := range errs {
		switch e.Type {
		case "NOT_FOUND", "FORBIDDEN":
		case "RATE_LIMITED":
			return &RateLimitError{Resource: "graphql"}
		default:
			unavailable = false
		}
		messages = append(messages, e.Message)
	}
	err := fmt.Errorf("GitHub %s: %s", operation, strings.Join(messages, "; "))
	if unavailable {
		return errors.Join(ErrUnavailable, err)
	}
	return err
}

// do sends a request of a rate-limit resource. It waits for a rate limit
// that resets within maxRetryWait and returns a RateLimitError for one that
// resets later, so a sync gives the run back instead of blocking.
func (c *Client) do(ctx context.Context, resource string, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := c.awaitReserve(ctx, resource); err != nil {
			return nil, err
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		if c.userAgent != "" {
			req.Header.Set("User-Agent", c.userAgent)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GitHub request: %w", err)
		}
		c.recordLimit(resource, resp.Header)
		limited := c.rateLimited(resource, resp)
		if limited == nil {
			return resp, nil
		}
		drain(resp)
		wait := limited.Reset.Sub(c.now())
		if wait > maxRetryWait || attempt >= maxRateLimitRetries {
			return nil, limited
		}
		if err := c.sleep(ctx, max(wait, time.Second)); err != nil {
			return nil, err
		}
	}
}

// awaitReserve stops before a request would eat into the reserve of an
// hourly limit, and waits out a limit that resets soon.
func (c *Client) awaitReserve(ctx context.Context, resource string) error {
	c.mu.Lock()
	limit, known := c.limits[resource]
	c.mu.Unlock()
	if !known || float64(limit.remaining) >= reserveFraction*float64(limit.limit) {
		return nil
	}
	wait := limit.reset.Sub(c.now())
	if wait <= 0 {
		return nil
	}
	if wait > maxRetryWait {
		return &RateLimitError{Resource: resource, Reset: limit.reset}
	}
	if err := c.sleep(ctx, wait); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.limits, resource)
	c.mu.Unlock()
	return nil
}

func (c *Client) recordLimit(resource string, header http.Header) {
	remaining, err1 := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	limit, err2 := strconv.Atoi(header.Get("X-RateLimit-Limit"))
	reset, err3 := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	c.mu.Lock()
	c.limits[resource] = rateLimit{remaining: remaining, limit: limit, reset: time.Unix(reset, 0)}
	c.mu.Unlock()
}

// rateLimited recognizes GitHub's primary and secondary rate-limit
// responses: a 403 or 429 with Retry-After or no requests remaining.
func (c *Client) rateLimited(resource string, resp *http.Response) *RateLimitError {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return &RateLimitError{Resource: resource, Reset: c.now().Add(time.Duration(seconds) * time.Second)}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return &RateLimitError{Resource: resource, Reset: time.Unix(reset, 0)}
		}
	}
	return nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// statusError describes a failed response. Error text names the call,
// never the token.
func statusError(resp *http.Response, call string) error {
	text, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var payload struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(text))
	if json.Unmarshal(text, &payload) == nil && payload.Message != "" {
		message = payload.Message
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: GitHub %s: %s", ErrUnavailable, call, message)
	case http.StatusForbidden:
		if strings.Contains(message, "personal access token") {
			return ErrClassicTokenRequired
		}
		return fmt.Errorf("%w: GitHub %s: %s", ErrUnavailable, call, message)
	default:
		return fmt.Errorf("GitHub %s: %s: %s", call, resp.Status, message)
	}
}
