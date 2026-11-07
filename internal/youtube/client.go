package youtube

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxAPIBytes = 16 << 20

var ErrQuota = errors.New("YouTube quota exhausted")

// APIError contains only HTTP status and a safe machine-readable reason, or
// unknown when the response body cannot be read. Provider messages and bodies
// must never enter logs or sync status.
type APIError struct {
	StatusCode int
	Reason     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("YouTube API HTTP %d (%s)", e.StatusCode, e.Reason)
}
func (e *APIError) Is(target error) bool {
	return target == ErrQuota && (e.Reason == "quotaExceeded" || e.Reason == "dailyLimitExceeded")
}

type Client struct {
	baseURL, refreshEndpoint             string
	clientID, clientSecret, refreshToken string
	api                                  *http.Client
	mu                                   sync.Mutex
	accessToken                          string
	expires                              time.Time
}

func NewClient(clientID, clientSecret, refreshToken string) (*Client, error) {
	for _, v := range []string{clientID, clientSecret, refreshToken} {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("YouTube OAuth credentials missing or invalid")
		}
	}
	return &Client{baseURL: "https://www.googleapis.com/youtube/v3", refreshEndpoint: "https://oauth2.googleapis.com/token", clientID: clientID, clientSecret: clientSecret, refreshToken: refreshToken, api: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func safeTransportError(ctx context.Context) error {
	if ctx.Err() != nil {
		return fmt.Errorf("YouTube request: %w", ctx.Err())
	}
	return errors.New("YouTube request failed")
}

func readBounded(resp *http.Response) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	if err != nil {
		return nil, errors.New("read YouTube response failed")
	}
	if len(b) > maxAPIBytes {
		return nil, errors.New("YouTube response exceeds 16 MiB")
	}
	return b, nil
}

func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Now().Add(time.Minute).Before(c.expires) {
		return c.accessToken, nil
	}
	form := url.Values{"client_id": {c.clientID}, "client_secret": {c.clientSecret}, "refresh_token": {c.refreshToken}, "grant_type": {"refresh_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.refreshEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("construct YouTube token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.api.Do(req)
	if err != nil {
		return "", safeTransportError(ctx)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readBounded(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("YouTube token refresh HTTP %d", resp.StatusCode)
	}
	var value struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(b, &value) != nil || value.AccessToken == "" || strings.ContainsAny(value.AccessToken, "\r\n") || value.ExpiresIn <= 0 {
		return "", errors.New("invalid YouTube token response")
	}
	c.accessToken = value.AccessToken
	c.expires = time.Now().Add(time.Duration(min(value.ExpiresIn, 86400)) * time.Second)
	return c.accessToken, nil
}

func safeReason(body []byte) string {
	var value struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &value) != nil {
		return "unknown"
	}
	// Channel-wide quota takes precedence over per-resource reasons regardless
	// of provider error order; otherwise a quota failure could skip a page.
	for _, e := range value.Error.Errors {
		if e.Reason == "quotaExceeded" || e.Reason == "dailyLimitExceeded" {
			return e.Reason
		}
	}
	for _, e := range value.Error.Errors {
		switch e.Reason {
		case "quotaExceeded", "dailyLimitExceeded", "commentsDisabled", "commentNotFound", "videoNotFound", "invalidPageToken", "forbidden":
			return e.Reason
		}
	}
	return "unknown"
}

// request never retries a mutation, including ambiguous transport outcomes.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return nil, errors.New("invalid YouTube API path")
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("construct YouTube API request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.api.Do(req)
	if err != nil {
		return nil, safeTransportError(ctx)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		b, readErr := readBounded(resp)
		if readErr != nil {
			return nil, &APIError{StatusCode: resp.StatusCode, Reason: "unknown"}
		}
		return nil, &APIError{resp.StatusCode, safeReason(b)}
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := readBounded(resp)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid YouTube API JSON response")
	}
	return nil
}

func (c *Client) ownedChannel(ctx context.Context, channelID string) (string, string, error) {
	if !validID(channelID) {
		return "", "", errors.New("invalid YouTube channel ID")
	}
	var result page[struct {
		ID      string `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
		ContentDetails struct {
			RelatedPlaylists struct {
				Uploads string `json:"uploads"`
			} `json:"relatedPlaylists"`
		} `json:"contentDetails"`
	}]
	if err := c.get(ctx, "/channels", url.Values{"mine": {"true"}, "part": {"id,snippet,contentDetails"}}, &result); err != nil {
		return "", "", err
	}
	for _, ch := range result.Items {
		if ch.ID == channelID && validID(ch.ContentDetails.RelatedPlaylists.Uploads) {
			return ch.ContentDetails.RelatedPlaylists.Uploads, ch.Snippet.Title, nil
		}
	}
	return "", "", errors.New("YouTube credentials do not own the configured channel")
}
