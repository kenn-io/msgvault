package gitlab

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/forge"
)

const (
	// perPage is the most GitLab's REST lists return at once.
	perPage = 100
	// shortWait is the longest a request waits for a spent rate limit to
	// reset, as forge.API waits out a refusal; a later reset pauses the run.
	shortWait = time.Minute
	// unknownWait is how long a refusal that names no reset is waited out.
	unknownWait = time.Minute
)

// ErrMissingScope means the token lacks a scope a sync needs.
var ErrMissingScope = errors.New("the GitLab token lacks a scope")

// client calls GitLab's REST and GraphQL APIs with one token. GitLab
// serves REST under /api/v4 and GraphQL at /api/graphql of the instance.
type client struct {
	rest  *forge.API
	graph *forge.API
	now   func() time.Time

	mu    sync.Mutex
	limit rateLimit
	// started is when GitLab answered this sync's first request, by its
	// Date header.
	started time.Time
}

// rateLimit is the limit as the last response reported it.
type rateLimit struct {
	remaining int
	reset     time.Time
}

func newClient(base *url.URL, token, userAgent string, httpClient *http.Client) *client {
	c := &client{now: time.Now, limit: rateLimit{remaining: -1}}
	api := func(path ...string) *forge.API {
		return &forge.API{
			Base:   base.JoinPath(path...),
			Client: httpClient,
			Authorize: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+token)
			},
			UserAgent: userAgent,
			Limited:   c.limited,
			Observe:   c.observe,
		}
	}
	c.rest, c.graph = api("api", "v4"), api("api")
	return c
}

// observe keeps the RateLimit-Remaining and RateLimit-Reset headers GitLab
// sends with most responses; some, such as the Projects API's on
// GitLab.com, carry none. It also notes when the sync's first response
// came.
func (c *client) observe(resp *http.Response) {
	if at, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		c.mu.Lock()
		if c.started.IsZero() {
			c.started = at
		}
		c.mu.Unlock()
	}
	remaining, err1 := strconv.Atoi(resp.Header.Get("Ratelimit-Remaining"))
	reset, err2 := strconv.ParseInt(resp.Header.Get("Ratelimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	c.mu.Lock()
	c.limit = rateLimit{remaining: remaining, reset: time.Unix(reset, 0)}
	c.mu.Unlock()
}

// limited recognizes GitLab's rate-limit refusal, a 429, and when the limit
// resets: Retry-After, else RateLimit-ResetTime, else RateLimit-Reset.
func (c *client) limited(resp *http.Response) (time.Time, bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return c.now().Add(time.Duration(seconds) * time.Second), true
	}
	if at, err := http.ParseTime(resp.Header.Get("Ratelimit-Resettime")); err == nil {
		return at, true
	}
	if at, err := strconv.ParseInt(resp.Header.Get("Ratelimit-Reset"), 10, 64); err == nil {
		return time.Unix(at, 0), true
	}
	return c.now().Add(unknownWait), true
}

// startedAt is when this sync's first request was answered, by GitLab's
// clock, or by this machine's when GitLab sent no date.
func (c *client) startedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started.IsZero() {
		c.started = c.now().UTC().Truncate(time.Second)
	}
	return c.started
}

// pace waits before a request the last response said the limit has no room
// for, until the limit resets, or pauses the run when that is later than
// shortWait.
func (c *client) pace(ctx context.Context) error {
	c.mu.Lock()
	limit := c.limit
	c.mu.Unlock()
	if limit.remaining != 0 {
		return nil
	}
	wait := limit.reset.Sub(c.now())
	if wait <= 0 {
		return nil
	}
	if wait > shortWait {
		return &forge.RateLimitError{Resource: "api", Reset: limit.reset}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// get sends a REST GET, decodes the response into out and returns its
// headers.
func (c *client) get(ctx context.Context, path string, query url.Values, out any) (http.Header, error) {
	if err := c.pace(ctx); err != nil {
		return nil, err
	}
	resp, err := c.rest.GetJSON(ctx, path, query, nil, out)
	if err != nil {
		return nil, err
	}
	return resp.Header, nil
}

// forbiddenIsUnavailable reads a 403 as a thread or project the token may
// not read, unless GitLab says the token lacks a scope: that is the
// token's problem, not the thread's.
func forbiddenIsUnavailable(err error) error {
	refused, ok := errors.AsType[*forge.HTTPError](err)
	if !ok || refused.Status != http.StatusForbidden || strings.Contains(refused.Message, "insufficient_scope") {
		return err
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

// graphQLError is a GraphQL response's errors.
type graphQLError struct {
	operation string
	messages  []string
}

func (e *graphQLError) Error() string {
	return fmt.Sprintf("GitLab %s: %s", e.operation, strings.Join(e.messages, "; "))
}

// graphQL runs a query and decodes its data into out. Any error GitLab
// reports fails the query: the queries here ask only for what they need.
func (c *client) graphQL(ctx context.Context, request graphQLRequest, out any) error {
	if err := c.pace(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode GitLab %s query: %w", request.OperationName, err)
	}
	target, err := c.graph.URL("graphql", nil)
	if err != nil {
		return err
	}
	resp, err := c.graph.Do(ctx, http.MethodPost, target, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Data   jsontext.Value `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.UnmarshalRead(resp.Body, &envelope); err != nil {
		return fmt.Errorf("decode GitLab %s response: %w", request.OperationName, err)
	}
	if len(envelope.Errors) > 0 {
		failure := &graphQLError{operation: request.OperationName}
		for _, e := range envelope.Errors {
			failure.messages = append(failure.messages, e.Message)
		}
		return failure
	}
	if len(envelope.Data) == 0 || envelope.Data.Kind() == 'n' {
		return fmt.Errorf("GitLab %s returned no data", request.OperationName)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode GitLab %s data: %w", request.OperationName, err)
	}
	return nil
}

// user is a user as GitLab's REST responses embed it.
type user struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	WebURL      string `json:"web_url"`
	PublicEmail string `json:"public_email"`
}

// currentUser is GET /user.
type currentUser struct {
	user

	Bot bool `json:"bot"`
}

// account returns the token's account and checks that the token may read
// what a sync reads: the API (read_api: issues, merge requests, to-dos)
// and the user's own events (read_user). A token with api has both.
func (c *client) account(ctx context.Context) (forge.Account, error) {
	var me currentUser
	if _, err := c.get(ctx, "/user", nil, &me); err != nil {
		return forge.Account{}, fmt.Errorf("read the GitLab account: %w", err)
	}
	if me.ID <= 0 || me.Username == "" {
		return forge.Account{}, errors.New("GitLab returned no account for the token")
	}
	if err := c.checkScopes(ctx); err != nil {
		return forge.Account{}, err
	}
	return forge.Account{
		Kind: forge.AuthorAccount, ID: strconv.FormatInt(me.ID, 10), Login: me.Username, URL: me.WebURL, Bot: me.Bot,
	}, nil
}

// checkScopes reads the token's own scopes. A token GitLab doesn't
// describe this way, such as an OAuth token, or a fine-grained token, whose
// permissions are not scopes, is used as it is: a request it may not make
// fails naming the scope it lacks.
func (c *client) checkScopes(ctx context.Context) error {
	var token struct {
		Scopes []string `json:"scopes"`
	}
	_, err := c.get(ctx, "/personal_access_tokens/self", nil, &token)
	if refused, ok := errors.AsType[*forge.HTTPError](err); ok && (refused.Status == http.StatusForbidden || refused.Status == http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the GitLab token's scopes: %w", err)
	}
	if len(token.Scopes) == 0 || slices.Contains(token.Scopes, "api") {
		return nil
	}
	var missing []string
	for _, scope := range []string{"read_api", "read_user"} {
		if !slices.Contains(token.Scopes, scope) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s (or use api)", ErrMissingScope, strings.Join(missing, " and "))
	}
	return nil
}
