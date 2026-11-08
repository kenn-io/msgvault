package googlereviews

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxResponseBytes = 2 << 20

// Google only calls the fixed official reviews endpoint. The supplied transport
// must already carry the account-checked credential snapshot.
type Google struct {
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

func NewGoogle(client *http.Client) *Google {
	checked := *client
	checked.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	checked.Timeout = 20 * time.Second
	return &Google{client: &checked, sleep: waitRetry}
}
func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (g *Google) List(ctx context.Context, parent, pageToken string, pageSize int) (Page, error) {
	var result Page
	if !validLocation(parent) || pageSize < 0 || pageSize > 50 || len(pageToken) > 4096 || !utf8.ValidString(pageToken) {
		return result, ErrInvalid
	}
	if pageSize == 0 {
		pageSize = 20
	}
	query := url.Values{"pageSize": {strconv.Itoa(pageSize)}, "orderBy": {"updateTime desc"}}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	err := g.do(ctx, http.MethodGet, parent+"/reviews?"+query.Encode(), nil, &result)
	if err == nil {
		if len(result.Reviews) > 50 || len(result.NextPageToken) > 4096 {
			return Page{}, ErrUnavailable
		}
		for i := range result.Reviews {
			if !validID(result.Reviews[i].ReviewID) {
				return Page{}, ErrUnavailable
			}
			expected := parent + "/reviews/" + result.Reviews[i].ReviewID
			if result.Reviews[i].Name != "" && result.Reviews[i].Name != expected {
				return Page{}, ErrUnavailable
			}
			result.Reviews[i].Name = expected
		}
	}
	return result, err
}
func (g *Google) Get(ctx context.Context, name string) (Review, error) {
	var result Review
	if !validReviewName(name) {
		return result, ErrInvalid
	}
	err := g.do(ctx, http.MethodGet, name, nil, &result)
	if err == nil {
		parts := strings.Split(name, "/")
		if result.ReviewID != parts[5] || (result.Name != "" && result.Name != name) {
			return Review{}, ErrUnavailable
		}
		result.Name = name
	}
	return result, err
}
func (g *Google) UpdateReply(ctx context.Context, name, comment string) (Reply, error) {
	var result Reply
	if !validReviewName(name) || strings.TrimSpace(comment) == "" || len(comment) > 4096 || !utf8.ValidString(comment) {
		return result, ErrInvalid
	}
	data, err := json.Marshal(struct {
		Comment string `json:"comment"`
	}{comment})
	if err != nil {
		return result, ErrInvalid
	}
	err = g.do(ctx, http.MethodPut, name+"/reply", data, &result)
	if err == nil && result.Comment != comment {
		return Reply{}, ErrUnknown
	}
	return result, err
}
func (g *Google) do(ctx context.Context, method, path string, body []byte, out any) error {
	attempts := 3
	if method == http.MethodPut {
		attempts = 1
	}
	for attempt := range attempts {
		request, err := http.NewRequestWithContext(ctx, method, "https://mybusiness.googleapis.com/v4/"+path, bytes.NewReader(body))
		if err != nil {
			return ErrInvalid
		}
		request.Header.Set("Accept", "application/json")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		resp, err := g.client.Do(request)
		if err != nil {
			if method == http.MethodPut {
				return ErrUnknown
			}
			return ErrUnavailable
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if readErr != nil || len(data) > maxResponseBytes || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, out) != nil {
				if method == http.MethodPut {
					return ErrUnknown
				}
				return ErrUnavailable
			}
			return nil
		}
		if method == http.MethodPut && resp.StatusCode >= 500 {
			return ErrUnknown
		}
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusInternalServerError || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
		if retry && attempt+1 < attempts {
			delay := retryDelay(resp.Header.Get("Retry-After"), attempt, time.Now())
			if err := g.sleep(ctx, delay); err != nil {
				return ErrUnavailable
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return ErrCredentials
		case http.StatusForbidden:
			return ErrPermission
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusTooManyRequests:
			return ErrQuota
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return ErrTemporary
		case http.StatusBadRequest:
			return ErrRejected
		default:
			return ErrUnavailable
		}
	}
	return ErrUnavailable
}

func retryDelay(header string, attempt int, now time.Time) time.Duration {
	fallback := time.Duration(100*(1<<attempt)+rand.IntN(100)) * time.Millisecond // #nosec G404 -- Retry jitter is not a security token or credential.
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds > 0 {
			return time.Duration(min(seconds, 5)) * time.Second
		}
		return fallback
	}
	if at, err := http.ParseTime(header); err == nil && at.After(now) {
		return min(at.Sub(now), 5*time.Second)
	}
	return fallback
}
