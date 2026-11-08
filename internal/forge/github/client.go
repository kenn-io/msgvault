// Package github is the GitHub forge: it finds the threads an account takes
// part in through its notifications and a search for threads that involve
// it, and fetches each thread's posts over GraphQL.
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

	"go.kenn.io/msgvault/internal/forge"
)

const (
	apiVersion = "2022-11-28"
	// reserveFraction of each hourly rate limit is left for the account's
	// other tools, which share it: a sync pauses there.
	reserveFraction = 0.2
	// secondaryPause is how long a sync waits after a secondary rate limit
	// that names no time. GitHub asks for at least a minute; the run pauses
	// rather than retry within it.
	secondaryPause = 2 * time.Minute
)

// ErrClassicTokenRequired means the token cannot read notifications: GitHub
// serves them only to classic personal access tokens with the
// notifications or repo scope.
var ErrClassicTokenRequired = errors.New("GitHub notifications need a classic personal access token with the notifications or repo scope")

// client calls GitHub's REST and GraphQL APIs with one token.
type client struct {
	rest *forge.API
	now  func() time.Time

	mu     sync.Mutex
	limits map[string]rateLimit
}

// rateLimit is one rate-limit resource as the last response reported it.
type rateLimit struct {
	remaining int
	limit     int
	reset     time.Time
}

func newClient(apiBase *url.URL, token, userAgent string, httpClient *http.Client) *client {
	c := &client{now: time.Now, limits: map[string]rateLimit{}}
	c.rest = &forge.API{
		Base:   apiBase,
		Client: httpClient,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", apiVersion)
		},
		UserAgent: userAgent,
		Limited:   c.limited,
		Observe:   c.observe,
	}
	return c
}

// resource is the rate-limit resource a request draws on.
func resource(path string) string {
	if strings.HasSuffix(path, "/graphql") {
		return "graphql"
	}
	return "core"
}

// reserve pauses before a request would draw an hourly limit into the
// share left for the account's other tools.
func (c *client) reserve(resource string) error {
	c.mu.Lock()
	limit, known := c.limits[resource]
	c.mu.Unlock()
	if !known || c.now().After(limit.reset) {
		return nil
	}
	if float64(limit.remaining) <= reserveFraction*float64(limit.limit) {
		return &forge.RateLimitError{Resource: resource, Reset: limit.reset}
	}
	return nil
}

func (c *client) observe(resp *http.Response) {
	remaining, err1 := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	limit, err2 := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	reset, err3 := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	name := resp.Header.Get("X-RateLimit-Resource")
	if name == "" {
		name = resource(resp.Request.URL.Path)
	}
	c.mu.Lock()
	c.limits[name] = rateLimit{remaining: remaining, limit: limit, reset: time.Unix(reset, 0)}
	c.mu.Unlock()
}

// limited recognizes GitHub's rate-limit refusals: a 403 or 429 with no
// requests remaining (primary), or one that GitHub's message,
// documentation link or Retry-After marks as a secondary limit.
func (c *client) limited(resp *http.Response) (time.Time, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return c.now().Add(time.Duration(retryAfter) * time.Second), true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(reset, 0), true
		}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var payload struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}
	_ = json.Unmarshal(body, &payload)
	if strings.Contains(strings.ToLower(payload.Message), "secondary rate limit") ||
		strings.Contains(payload.DocumentationURL, "secondary-rate-limits") ||
		resp.StatusCode == http.StatusTooManyRequests {
		return c.now().Add(secondaryPause), true
	}
	return time.Time{}, false
}

// getJSON sends a REST GET within the reserve. A 403 that is no rate limit
// means the token may not read the resource.
func (c *client) getJSON(ctx context.Context, path string, query url.Values, header http.Header, out any) (forge.Response, error) {
	if err := c.reserve("core"); err != nil {
		return forge.Response{}, err
	}
	resp, err := c.rest.GetJSON(ctx, path, query, header, out)
	return resp, forbiddenIsUnavailable(err)
}

// forbiddenIsUnavailable reads a 403 that is no rate limit as a resource
// the token may not read, or, for a fine-grained token, as the classic
// token GitHub's notifications need.
func forbiddenIsUnavailable(err error) error {
	refused, ok := errors.AsType[*forge.HTTPError](err)
	if !ok || refused.Status != http.StatusForbidden {
		return err
	}
	if strings.Contains(refused.Message, "personal access token") {
		return ErrClassicTokenRequired
	}
	return fmt.Errorf("%w: %w", forge.ErrUnavailable, err)
}

// graphQLRequest is one GraphQL call; its operation name says in errors
// which query failed.
type graphQLRequest struct {
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
}

// graphQLProblem is one entry of a GraphQL response's errors.
type graphQLProblem struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// graphQLResponseError is a GraphQL response's errors, with the data that
// came back beside them.
type graphQLResponseError struct {
	operation string
	errors    []graphQLProblem
}

func (e *graphQLResponseError) Error() string {
	messages := make([]string, 0, len(e.errors))
	for _, err := range e.errors {
		messages = append(messages, err.Message)
	}
	return fmt.Sprintf("GitHub %s: %s", e.operation, strings.Join(messages, "; "))
}

// missingAt reports whether every error says an object at one of these
// paths is missing or forbidden: the paths a caller may treat as gone.
func (e *graphQLResponseError) missingAt(paths ...string) bool {
	for _, err := range e.errors {
		if err.Type != "NOT_FOUND" && err.Type != "FORBIDDEN" {
			return false
		}
		path := make([]string, 0, len(err.Path))
		for _, part := range err.Path {
			path = append(path, fmt.Sprint(part))
		}
		joined := strings.Join(path, ".")
		found := false
		for _, allowed := range paths {
			if joined == allowed {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// graphQL runs a query within the reserve and decodes its data into out.
// Errors come back as *graphQLResponseError, with out decoded from whatever data
// arrived, so a caller decides which missing objects it can do without.
func (c *client) graphQL(ctx context.Context, request graphQLRequest, out any) error {
	if err := c.reserve("graphql"); err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode GitHub %s query: %w", request.OperationName, err)
	}
	target, err := c.rest.URL("/graphql", nil)
	if err != nil {
		return err
	}
	resp, err := c.rest.Do(ctx, http.MethodPost, target, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return forbiddenIsUnavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Data   jsontext.Value   `json:"data"`
		Errors []graphQLProblem `json:"errors"`
	}
	if err := json.UnmarshalRead(resp.Body, &envelope); err != nil {
		return fmt.Errorf("decode GitHub %s response: %w", request.OperationName, err)
	}
	for _, e := range envelope.Errors {
		if e.Type == "RATE_LIMITED" {
			return &forge.RateLimitError{Resource: "graphql", Reset: c.now().Add(secondaryPause)}
		}
	}
	if len(envelope.Data) > 0 && envelope.Data.Kind() != 'n' {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode GitHub %s data: %w", request.OperationName, err)
		}
	}
	if len(envelope.Errors) > 0 {
		return &graphQLResponseError{operation: request.OperationName, errors: envelope.Errors}
	}
	return nil
}

// account returns the token's account and checks that it reads
// notifications. It reports whether the token may read users' public
// emails, which links GitHub accounts to the people in the archive.
func (c *client) account(ctx context.Context) (forge.Account, bool, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		URL   string `json:"html_url"`
	}
	resp, err := c.getJSON(ctx, "/user", nil, nil, &user)
	if err != nil {
		return forge.Account{}, false, err
	}
	scopes, classic := resp.Header["X-Oauth-Scopes"]
	if !classic {
		return forge.Account{}, false, ErrClassicTokenRequired
	}
	granted := map[string]bool{}
	for _, header := range scopes {
		for scope := range strings.SplitSeq(header, ",") {
			granted[strings.TrimSpace(scope)] = true
		}
	}
	if !granted["notifications"] && !granted["repo"] {
		return forge.Account{}, false, ErrClassicTokenRequired
	}
	if user.ID == 0 || user.Login == "" {
		return forge.Account{}, false, errors.New("GitHub returned no account for the token")
	}
	emails := granted["user"] || granted["read:user"] || granted["user:email"]
	return forge.Account{
		Kind: forge.AuthorAccount, ID: strconv.FormatInt(user.ID, 10), Login: user.Login, URL: user.URL,
	}, emails, nil
}
