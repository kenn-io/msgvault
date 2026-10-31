package line

import (
	"bytes"
	"context"
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
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// RequestTimeout bounds one Messaging API request other than a media download.
const RequestTimeout = 30 * time.Second

const (
	defaultAPIURL  = "https://api.line.me"
	defaultDataURL = "https://api-data.line.me"
	maxJSONBytes   = 1 << 20
)

// APIError deliberately excludes provider bodies and credentials.
// RetryAfter is a deadline for 429 responses and 5xx with Retry-After.
type APIError struct {
	StatusCode int
	RetryAfter time.Time
}

func (e *APIError) Error() string { return fmt.Sprintf("line: API returned HTTP %d", e.StatusCode) }

type BotInfo struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}
type Profile struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
}
type GroupSummary struct {
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
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
			c.http = client
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
	if cloned.Timeout <= 0 || cloned.Timeout > RequestTimeout {
		cloned.Timeout = RequestTimeout
	}
	c.http = &cloned
	return c
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
	config := clientcredentials.Config{ClientID: c.channelID, ClientSecret: c.secret, TokenURL: c.apiURL + "/oauth2/v3/token", AuthStyle: oauth2.AuthStyleInParams}
	token, err := config.Token(context.WithValue(ctx, oauth2.HTTPClient, c.http))
	if err != nil {
		return "", c.tokenError(ctx, err)
	}
	lifetime, ok := token.Extra("expires_in").(float64)
	if !ok || lifetime <= 0 || lifetime > math.MaxInt32 || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", errors.New("line: invalid token response")
	}
	c.token = token.AccessToken
	c.refreshAt = c.now().Add(time.Duration(lifetime)*time.Second - time.Minute)
	return c.token, nil
}

// OAuth errors can contain provider bodies or credential-bearing URLs.
func (c *Client) tokenError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("line: request timed out: %w", context.DeadlineExceeded)
	}
	if response, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		return c.statusError(response.Response)
	}
	return errors.New("line: token request failed")
}

func (c *Client) send(ctx context.Context, method, base, path string, body []byte, contentType, token string) (*http.Response, error) {
	return c.sendWithHTTPClient(ctx, method, base, path, body, contentType, token, c.http, "")
}

func (c *Client) sendWithHTTPClient(ctx context.Context, method, base, path string, body []byte, contentType, token string, client *http.Client, retryKey string) (*http.Response, error) {
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
	if retryKey != "" {
		request.Header.Set("X-Line-Retry-Key", retryKey)
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
	return c.authorizedWithHTTPClient(ctx, method, base, path, body, c.http, "")
}

func (c *Client) authorizedWithHTTPClient(ctx context.Context, method, base, path string, body []byte, client *http.Client, retryKey string) (*http.Response, error) {
	token, err := c.accessToken(ctx, "")
	if err != nil {
		return nil, err
	}
	for attempt := range 2 {
		response, err := c.sendWithHTTPClient(ctx, method, base, path, body, "application/json", token, client, retryKey)
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
	if response.StatusCode != http.StatusTooManyRequests && (response.StatusCode < 500 || response.Header.Get("Retry-After") == "") {
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
// Its download deadline uses the configured attachment cap.
func (c *Client) Content(ctx context.Context, messageID string, maxBytes int64) (*http.Response, error) {
	if messageID == "" {
		return nil, errors.New("line: missing message ID")
	}
	if maxBytes <= 0 {
		return nil, errors.New("line: invalid media size cap")
	}
	mediaHTTP := *c.http
	mediaHTTP.Timeout = attachmentpolicy.DownloadTimeout(maxBytes)
	response, err := c.authorizedWithHTTPClient(ctx, http.MethodGet, c.dataURL, "/v2/bot/message/"+url.PathEscape(messageID)+"/content", nil, &mediaHTTP, "")
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
