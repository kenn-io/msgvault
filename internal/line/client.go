package line

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/httpretry"
)

const (
	defaultAPIURL  = "https://api.line.me"
	defaultDataURL = "https://api-data.line.me"
	requestTimeout = 30 * time.Second
	maxJSONBytes   = 1 << 20
)

// APIError deliberately excludes provider bodies and credentials.
// RetryAfter is an absolute retry deadline (zero unless status is 429).
type APIError struct {
	StatusCode int
	RetryAfter time.Time
}

func (e *APIError) Error() string { return fmt.Sprintf("line: API returned HTTP %d", e.StatusCode) }

type BotInfo struct {
	UserID      string `json:"userId"`
	BasicID     string `json:"basicId"`
	DisplayName string `json:"displayName"`
}
type Profile struct {
	UserID        string `json:"userId"`
	DisplayName   string `json:"displayName"`
	PictureURL    string `json:"pictureUrl"`
	Language      string `json:"language"`
	StatusMessage string `json:"statusMessage"`
}
type GroupSummary struct {
	GroupID    string `json:"groupId"`
	GroupName  string `json:"groupName"`
	PictureURL string `json:"pictureUrl"`
}
type WebhookTestResult struct {
	Success    bool   `json:"success"`
	Timestamp  string `json:"timestamp"`
	StatusCode int    `json:"statusCode"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
}

type ClientOption func(*Client)

// WithBaseURLs overrides the fixed LINE hosts for a local fake API.
// Invalid URLs fail requests before credentials are sent.
func WithBaseURLs(apiURL, dataURL string) ClientOption {
	return func(c *Client) {
		c.apiURL = strings.TrimRight(apiURL, "/")
		c.dataURL = strings.TrimRight(dataURL, "/")
	}
}
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		if client != nil {
			cloned := *client
			c.http = &cloned
		}
	}
}

// WithClock supplies the clock used for token expiration and retry deadlines.
func WithClock(now func() time.Time) ClientOption {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
	}
}

// Client is safe for concurrent use. The token gate serializes minting while
// letting a waiting caller cancel without waiting for another HTTP request.
type Client struct {
	channelID, secret string
	apiURL, dataURL   string
	http              *http.Client
	now               func() time.Time
	gate              chan struct{}
	token             string
	refreshAt         time.Time
}

func NewClient(channelID, secret string, options ...ClientOption) *Client {
	c := &Client{channelID: channelID, secret: secret, apiURL: defaultAPIURL, dataURL: defaultDataURL, http: &http.Client{}, now: time.Now, gate: make(chan struct{}, 1)}
	for _, option := range options {
		option(c)
	}
	// Clone even an injected client: do not mutate caller settings. Never allow
	// redirects to forward a bearer header or replay a secret-bearing POST.
	cloned := *c.http
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cloned.Timeout <= 0 || cloned.Timeout > requestTimeout {
		cloned.Timeout = requestTimeout
	}
	c.http = &cloned
	return c
}

func validBase(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func (c *Client) accessToken(ctx context.Context, rejected string) (string, error) {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if rejected != "" && c.token == rejected {
		c.token = ""
	}
	if c.token != "" && c.now().Before(c.refreshAt) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.channelID}, "client_secret": {c.secret}}
	response, err := c.send(ctx, http.MethodPost, c.apiURL, "/oauth2/v3/token", []byte(form.Encode()), "application/x-www-form-urlencoded", "")
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", c.statusError(response)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := decodeJSON(ctx, response.Body, &token); err != nil {
		return "", err
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > math.MaxInt32 || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", errors.New("line: invalid token response")
	}
	c.token = token.AccessToken
	c.refreshAt = c.now().Add(time.Duration(token.ExpiresIn)*time.Second - time.Minute)
	return c.token, nil
}

func (c *Client) send(ctx context.Context, method, base, path string, body []byte, contentType, token string) (*http.Response, error) {
	return c.sendWithHTTPClient(ctx, method, base, path, body, contentType, token, c.http)
}

func (c *Client) sendWithHTTPClient(ctx context.Context, method, base, path string, body []byte, contentType, token string, client *http.Client) (*http.Response, error) {
	if !validBase(base) {
		return nil, errors.New("line: invalid API base URL")
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("line: cannot create request")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		// url.Error may embed a credential-bearing URL or transport detail. Only
		// caller cancellation is propagated; never include transport error text.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("line: request timed out: %w", context.DeadlineExceeded)
		}
		return nil, errors.New("line: HTTP request failed")
	}
	return response, nil
}

func (c *Client) authorized(ctx context.Context, method, base, path string, body []byte) (*http.Response, error) {
	return c.authorizedWithHTTPClient(ctx, method, base, path, body, c.http)
}

func (c *Client) authorizedWithHTTPClient(ctx context.Context, method, base, path string, body []byte, client *http.Client) (*http.Response, error) {
	token, err := c.accessToken(ctx, "")
	if err != nil {
		return nil, err
	}
	for attempt := range 2 {
		response, err := c.sendWithHTTPClient(ctx, method, base, path, body, "application/json", token, client)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || attempt == 1 {
			return response, nil
		}
		_ = response.Body.Close()
		token, err = c.accessToken(ctx, token)
		if err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func (c *Client) statusError(response *http.Response) *APIError {
	err := &APIError{StatusCode: response.StatusCode}
	if response.StatusCode != http.StatusTooManyRequests {
		return err
	}
	now := c.now()
	// Missing/malformed Retry-After still yields a bounded retry deadline.
	err.RetryAfter = now.Add(30 * time.Second)
	if delay, ok := httpretry.ParseRetryAfter(response.Header.Get("Retry-After"), 0, now); ok {
		err.RetryAfter = now.Add(delay)
	}
	return err
}

func decodeJSON(ctx context.Context, body io.Reader, out any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxJSONBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("line: cannot read API response")
	}
	if len(raw) > maxJSONBytes {
		return errors.New("line: API response exceeds limit")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("line: invalid API JSON response")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("line: invalid API JSON response")
	}
	return nil
}

func (c *Client) jsonRequest(ctx context.Context, method, path string, body []byte, out any) error {
	response, err := c.authorized(ctx, method, c.apiURL, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return c.statusError(response)
	}
	return decodeJSON(ctx, response.Body, out)
}

func (c *Client) BotInfo(ctx context.Context) (BotInfo, error) {
	var result BotInfo
	err := c.jsonRequest(ctx, http.MethodGet, "/v2/bot/info", nil, &result)
	return result, err
}

func (c *Client) Profile(ctx context.Context, source Source, userID string) (Profile, error) {
	var result Profile
	if userID == "" {
		return result, errors.New("line: missing profile user ID")
	}
	var path string
	switch source.Type {
	case "user":
		path = "/v2/bot/profile/" + url.PathEscape(userID)
	case "group":
		if source.GroupID != "" {
			path = "/v2/bot/group/" + url.PathEscape(source.GroupID) + "/member/" + url.PathEscape(userID)
		}
	case "room":
		if source.RoomID != "" {
			path = "/v2/bot/room/" + url.PathEscape(source.RoomID) + "/member/" + url.PathEscape(userID)
		}
	}
	if path == "" {
		return result, errors.New("line: invalid profile source")
	}
	err := c.jsonRequest(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c *Client) GroupSummary(ctx context.Context, groupID string) (GroupSummary, error) {
	var result GroupSummary
	if groupID == "" {
		return result, errors.New("line: missing group ID")
	}
	err := c.jsonRequest(ctx, http.MethodGet, "/v2/bot/group/"+url.PathEscape(groupID)+"/summary", nil, &result)
	return result, err
}

// Content transfers ownership of Body to the caller for 200, 202, 404 and 410.
// Its download deadline uses the default attachment cap; processing uses the configured cap.
func (c *Client) Content(ctx context.Context, messageID string) (*http.Response, error) {
	return c.content(ctx, messageID, attachmentpolicy.DefaultChatMaxBytes)
}

func (c *Client) content(ctx context.Context, messageID string, maxBytes int64) (*http.Response, error) {
	if messageID == "" {
		return nil, errors.New("line: missing message ID")
	}
	if maxBytes <= 0 {
		return nil, errors.New("line: invalid media size cap")
	}
	mediaHTTP := *c.http
	mediaHTTP.Timeout = attachmentpolicy.DownloadTimeout(maxBytes)
	response, err := c.authorizedWithHTTPClient(ctx, http.MethodGet, c.dataURL, "/v2/bot/message/"+url.PathEscape(messageID)+"/content", nil, &mediaHTTP)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNotFound, http.StatusGone:
		return response, nil
	default:
		defer func() { _ = response.Body.Close() }()
		return nil, c.statusError(response)
	}
}

func webhookBody(endpoint string) ([]byte, error) {
	return json.Marshal(struct {
		Endpoint string `json:"endpoint"`
	}{endpoint})
}

func (c *Client) SetWebhook(ctx context.Context, endpoint string) error {
	body, err := webhookBody(endpoint)
	if err != nil {
		return fmt.Errorf("encode LINE webhook endpoint: %w", err)
	}
	var result jsontext.Value
	return c.jsonRequest(ctx, http.MethodPut, "/v2/bot/channel/webhook/endpoint", body, &result)
}
func (c *Client) TestWebhook(ctx context.Context, endpoint string) (WebhookTestResult, error) {
	var result WebhookTestResult
	body, err := webhookBody(endpoint)
	if err != nil {
		return result, fmt.Errorf("encode LINE webhook endpoint: %w", err)
	}
	err = c.jsonRequest(ctx, http.MethodPost, "/v2/bot/channel/webhook/test", body, &result)
	return result, err
}
