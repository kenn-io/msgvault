package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const maxRetries = 8

// TokenFunc returns a bearer token for a Graph API request.
type TokenFunc func(context.Context) (string, error)

// Client is a minimal Microsoft Graph REST client supporting paging and
// Retry-After back-off.
type Client struct {
	baseURL string
	token   TokenFunc
	http    *http.Client
	limiter *rate.Limiter
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
	reqURL, err := c.resolveRequestURL(rawURL)
	if err != nil {
		return nil, err
	}
	for attempt := range maxRetries {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for graph rate limit: %w", err)
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
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("graph GET %s: read body: %w", reqURL, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("graph GET %s: close body: %w", reqURL, closeErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
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
			return nil, fmt.Errorf("graph GET %s: status %d: %s", reqURL, resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("graph GET %s: exhausted %d retries", reqURL, maxRetries)
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

// retryAfter parses a Retry-After header value (seconds) or falls back to
// exponential back-off capped at 60 s.
func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	d := min(time.Duration(1<<uint(attempt))*time.Second, 60*time.Second)
	return d
}

// GetRaw fetches url and returns the raw response bytes. url should be a
// path-relative string (e.g. "/me/chats/.../hostedContents/1/$value"); it is
// prefixed with the client's baseURL automatically by the underlying get method.
func (c *Client) GetRaw(ctx context.Context, url string) ([]byte, error) {
	return c.get(ctx, url)
}

// BaseURL returns the client's configured base URL (scheme + host, no trailing slash).
// Importers use this to rewrite absolute graph.microsoft.com URLs to the configured
// host (supporting both production and httptest servers).
func (c *Client) BaseURL() string {
	return c.baseURL
}

// getJSON fetches url and unmarshals the JSON body into out.
func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	body, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// pageThrough follows @odata.nextLink, decoding each page into []T, calling fn.
// Returns the terminal @odata.deltaLink (empty for non-delta endpoints).
func pageThrough[T any](ctx context.Context, c *Client, startURL string, fn func([]T)) (string, error) {
	delta, _, err := pageThroughLimit(ctx, c, startURL, 0, fn)
	return delta, err
}

// pageThroughLimit is pageThrough with an optional item cap. When limit is
// positive, it stops before fetching a nextLink once enough items have been
// delivered and reports whether unread items/pages remain.
func pageThroughLimit[T any](ctx context.Context, c *Client, startURL string, limit int, fn func([]T)) (string, bool, error) {
	url := startURL
	delivered := 0
	for {
		var page listResponse[T]
		if err := c.getJSON(ctx, url, &page); err != nil {
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

func (c *Client) ListChats(ctx context.Context) ([]Chat, error) {
	var out []Chat
	_, err := pageThrough[Chat](ctx, c, "/me/chats?$top=50", func(p []Chat) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListJoinedTeams(ctx context.Context) ([]JoinedTeam, error) {
	var out []JoinedTeam
	_, err := pageThrough[JoinedTeam](ctx, c, "/me/joinedTeams", func(p []JoinedTeam) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListChannels(ctx context.Context, teamID string) ([]Channel, error) {
	var out []Channel
	_, err := pageThrough[Channel](ctx, c, "/teams/"+teamID+"/channels", func(p []Channel) { out = append(out, p...) })
	return out, err
}

// ListChatMessages: sinceISO empty = full backfill; non-empty = incremental.
func (c *Client) ListChatMessages(ctx context.Context, chatID, sinceISO string, limit int) ([]ChatMessage, bool, error) {
	url := "/me/chats/" + chatID + "/messages?$top=50"
	if sinceISO != "" {
		url += "&$filter=lastModifiedDateTime%20ge%20" + sinceISO + "&$orderby=lastModifiedDateTime%20desc"
	}
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, url, limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

// ChannelMessagesDelta drives the delta endpoint (or a stored deltaLink) to
// completion, returning all messages and the new deltaLink.
func (c *Client) ChannelMessagesDelta(ctx context.Context, teamID, channelID, deltaLink string, limit int) ([]ChatMessage, string, bool, error) {
	start := deltaLink
	if start == "" {
		start = "/teams/" + teamID + "/channels/" + channelID + "/messages/delta"
	}
	var out []ChatMessage
	newDelta, truncated, err := pageThroughLimit[ChatMessage](ctx, c, start, limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, newDelta, truncated, err
}

func (c *Client) ListChannelMessages(ctx context.Context, teamID, channelID string, limit int) ([]ChatMessage, bool, error) {
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages?$top=50", limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

func (c *Client) ListReplies(ctx context.Context, teamID, channelID, messageID string, limit int) ([]ChatMessage, bool, error) {
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages/"+messageID+"/replies", limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

// GetUser fetches a user by object ID from the Graph /users endpoint,
// selecting the fields needed for participant email resolution.
func (c *Client) GetUser(ctx context.Context, id string) (*GraphUser, error) {
	var u GraphUser
	if err := c.getJSON(ctx, "/users/"+id+"?$select=id,mail,userPrincipalName,displayName", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListChatMembers returns all members of the given chat.
func (c *Client) ListChatMembers(ctx context.Context, chatID string) ([]ChatMember, error) {
	var out []ChatMember
	_, err := pageThrough[ChatMember](ctx, c, "/chats/"+chatID+"/members", func(p []ChatMember) { out = append(out, p...) })
	return out, err
}
