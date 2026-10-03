// Package omi archives Omi conversations through the read-only Developer API.
package omi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
	"golang.org/x/time/rate"
)

const SourceType = "omi"
const RawFormat = "omi_json"
const DefaultBaseURL = "https://api.omi.me"
const PageSize = 200 // Upstream clamps the list endpoint to 200 records.

// TranscriptListsPerHour is Omi's per-key budget for transcript list requests.
const TranscriptListsPerHour = 25

// NormalizeBaseURL accepts a backend root, including a reverse-proxy prefix.
func NormalizeBaseURL(value string) (string, error) {
	if value == "" {
		value = DefaultBaseURL
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("omi base_url must be an HTTP(S) backend root without credentials, query, or fragment")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("omi base_url must use HTTPS for non-loopback hosts")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1/dev") {
		return "", errors.New("omi base_url must be the backend root; omit /v1/dev")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// retryDelay honors a 429's Retry-After for up to an hour, because Omi's
// transcript budget is a fixed hourly window and earlier retries are refused.
func retryDelay(resp *http.Response, attempt int) time.Duration {
	maximum := httpretry.ProviderMaxRetryAfter
	if resp.StatusCode == http.StatusTooManyRequests {
		maximum = time.Hour
	}
	return httpretry.RetryAfter(resp.Header.Get("Retry-After"), attempt, maximum)
}

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey,
		http:    &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		limiter: rate.NewLimiter(rate.Every(time.Hour/TranscriptListsPerHour), TranscriptListsPerHour),
	}
}

type ListParams struct {
	Limit         int
	Offset        int
	CreatedBefore time.Time
}

type Conversation struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Structured struct {
		Title string `json:"title"`
	} `json:"structured"`
	Raw jsontext.Value `json:"-"`
}

// ListConversations preserves each full provider object, including unknown fields.
func (c *Client) ListConversations(ctx context.Context, p ListParams) ([]Conversation, error) {
	baseURL, err := NormalizeBaseURL(c.baseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return nil, errors.New("omi api_key is required (Developer API key with conversations:read)")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = PageSize
	}
	q := url.Values{"limit": {strconv.Itoa(min(limit, PageSize))}, "offset": {strconv.Itoa(p.Offset)}, "include_transcript": {"true"}}
	if !p.CreatedBefore.IsZero() {
		q.Set("end_date", p.CreatedBefore.UTC().Format(time.RFC3339Nano))
	}
	endpoint := baseURL + "/v1/dev/user/conversations?" + q.Encode()
	for attempt := range 8 {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for Omi API rate limit: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("omi list conversations: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("omi read response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("omi close response: %w", closeErr)
		}
		if len(body) > 64<<20 {
			return nil, errors.New("omi response exceeds 64 MiB")
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			var items []jsontext.Value
			if err := json.Unmarshal(body, &items); err != nil || items == nil {
				return nil, errors.New("omi list conversations: expected a JSON array")
			}
			result := make([]Conversation, 0, len(items))
			for _, raw := range items {
				var item Conversation
				if err := json.Unmarshal(raw, &item); err != nil {
					return nil, fmt.Errorf("omi decode conversation: %w", err)
				}
				if strings.TrimSpace(item.ID) == "" {
					return nil, errors.New("omi conversation has no ID")
				}
				item.Raw = raw
				result = append(result, item)
			}
			return result, nil
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, errors.New("omi rejected api_key (401): use a Developer API key (omi_dev_...), not an MCP key")
		case resp.StatusCode == http.StatusForbidden:
			return nil, errors.New("omi denied conversation access (403): check conversations:read scope and account access")
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if attempt == 7 {
				break
			}
			timer := time.NewTimer(retryDelay(resp, attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		default:
			// Do not echo provider error bodies: they can contain private content.
			return nil, fmt.Errorf("omi list conversations: HTTP %d", resp.StatusCode)
		}
	}
	return nil, errors.New("omi list conversations: exhausted retries")
}
