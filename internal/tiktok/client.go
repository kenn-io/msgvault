package tiktok

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://business-api.tiktok.com/open_api/v1.3"
const maxResponseBytes = 8 << 20

var ErrRateLimited = errors.New("TikTok rate limit; retry after the five-minute cooldown")

// APIError deliberately excludes provider response text, which can echo secrets.
type APIError struct{ Code, Status int }

func (e *APIError) Error() string {
	return fmt.Sprintf("TikTok API rejected request (HTTP %d, code %d)", e.Status, e.Code)
}

type Client struct {
	http    *http.Client
	baseURL string
	token   func(context.Context) (string, error)
	limiter *Limiter
	wait    func(context.Context, time.Duration) error
}

// Close releases idle API and media connections after a bounded sync or delivery.
func (c *Client) Close() { c.http.CloseIdleConnections() }

func NewClient(token string) (*Client, error) {
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("TikTok access token is empty or invalid")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns: 10, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, baseURL: baseURL,
		token: func(context.Context) (string, error) { return token, nil }, limiter: NewLimiter(""), wait: waitContext}, nil
}

func waitContext(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Dispatch hooks are scoped to an operation, including nested token refreshes.
// They run only after preflight succeeds and immediately before HTTP dispatch.
type dispatchHookKey struct{}

func withDispatchHook(ctx context.Context, hook func() error) context.Context {
	prior, _ := ctx.Value(dispatchHookKey{}).(func() error)
	return context.WithValue(ctx, dispatchHookKey{}, func() error {
		if prior != nil {
			if err := prior(); err != nil {
				return err
			}
		}
		return hook()
	})
}

func (c *Client) request(ctx context.Context, path string, query url.Values, payload, result any, auth, retry bool) error {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	attempts := 1
	if retry {
		attempts = 3
	}
	for attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		token := ""
		if auth {
			token, err = c.token(ctx)
			if err != nil {
				return err
			}
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		method := http.MethodGet
		if payload != nil {
			method = http.MethodPost
		}
		target := c.baseURL + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return errors.New("invalid TikTok API request")
		}
		if auth {
			req.Header.Set("Access-Token", token)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if hook, ok := ctx.Value(dispatchHookKey{}).(func() error); ok {
			if err := hook(); err != nil {
				return err
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A refresh token can rotate even when its response is lost.
			if attempt+1 == attempts {
				return errors.New("TikTok API transport failed")
			}
		} else {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
			_ = resp.Body.Close()
			if readErr != nil || len(data) > maxResponseBytes {
				return errors.New("TikTok API response unreadable or exceeds size limit")
			}
			var envelope struct {
				Code *int           `json:"code"`
				Data jsontext.Value `json:"data"`
			}
			decodeErr := json.Unmarshal(data, &envelope)
			code := 0
			if envelope.Code != nil {
				code = *envelope.Code
			}
			if resp.StatusCode == http.StatusTooManyRequests || code == 40100 {
				return errors.Join(ErrRateLimited, c.limiter.Block(ctx))
			}
			// Retry safe reads only. Refresh calls have attempts=1.
			if resp.StatusCode < 500 || attempt+1 >= attempts {
				if resp.StatusCode < 200 || resp.StatusCode >= 300 || (envelope.Code != nil && code != 0) {
					return &APIError{Status: resp.StatusCode, Code: code}
				}
				if decodeErr != nil || envelope.Code == nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
					return errors.New("invalid TikTok API response envelope")
				}
				if err := json.Unmarshal(envelope.Data, result, json.StringifyNumbers(false)); err != nil {
					return errors.New("invalid TikTok API response data")
				}
				return nil
			}
		}
		if err := c.wait(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
			return err
		}
	}
	return errors.New("TikTok API retries exhausted")
}

func (c *Client) Account(ctx context.Context, account string) (Account, error) {
	var result Account
	err := c.request(ctx, "/business/get/", url.Values{"business_id": {account}, "fields": {`["username","display_name"]`}}, nil, &result, true, true)
	if err == nil && result.Username == "" && result.DisplayName == "" {
		err = errors.New("TikTok account profile is empty")
	}
	if err == nil && result.ID != "" && string(result.ID) != account {
		err = errors.New("TikTok account identity mismatch")
	}
	return result, err
}

func (c *Client) Videos(ctx context.Context, account, cursor string) (VideoPage, error) {
	var page VideoPage
	err := c.request(ctx, "/business/video/list/", url.Values{"business_id": {account}, "cursor": {initialCursor(cursor)}, "max_count": {"20"}, "fields": {`["item_id","caption","create_time","share_url"]`}}, nil, &page, true, true)
	if err == nil && len(page.Videos) > 20 {
		err = errors.New("TikTok video page exceeds requested size")
	}
	return page, err
}

func initialCursor(cursor string) string {
	if cursor == "" {
		return "0"
	}
	return cursor
}

// Comments reads either top-level comments or all replies to one comment.
func (c *Client) Comments(ctx context.Context, account, video, parent, cursor string) (CommentPage, error) {
	path := "/business/comment/list/"
	query := url.Values{"business_id": {account}, "video_id": {video}, "cursor": {initialCursor(cursor)}, "max_count": {"30"}, "status": {"ALL"}}
	if parent != "" {
		path = "/business/comment/reply/list/"
		query.Set("comment_id", parent)
	} else {
		query.Set("include_replies", "false")
	}
	var page CommentPage
	err := c.request(ctx, path, query, nil, &page, true, true)
	if err == nil && len(page.Comments) > 30 {
		err = errors.New("TikTok comment page exceeds requested size")
	}
	return page, err
}
