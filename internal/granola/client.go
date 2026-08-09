package granola

import (
	"context"
	"encoding/json"
	"errors"
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

// maxPageSize is the API's page_size ceiling.
const maxPageSize = 30

// Client is a minimal Granola public-API client with token-bucket rate
// limiting and Retry-After back-off. The documented limits are a sustained
// 5 req/s with a burst capacity of 25 per 5 seconds.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient creates a Client. baseURL is injected so tests can point at
// httptest servers; pass DefaultBaseURL for production. apiKey is the
// "grn_…" personal or workspace key from the desktop app's settings.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(5), 25),
	}
}

// get fetches path (with query already encoded), respecting the rate limiter
// and retrying on 429/5xx with Retry-After or exponential back-off.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	reqURL := c.baseURL + path
	for attempt := range maxRetries {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("wait for granola rate limit: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("granola GET %s: read body: %w", reqURL, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("granola GET %s: close body: %w", reqURL, closeErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, errors.New("granola API rejected the key (401): check [[granola]] api_key in config.toml (a grn_… key from the desktop app's settings; requires a Business plan)")
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
			return nil, fmt.Errorf("granola GET %s: status %d: %s", reqURL, resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("granola GET %s: exhausted %d retries", reqURL, maxRetries)
}

// retryAfter parses a Retry-After header value (seconds) or falls back to
// exponential back-off capped at 60 s.
func retryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return min(time.Duration(1<<uint(attempt))*time.Second, 60*time.Second)
}

// ListNotesParams filters GET /v1/notes. Zero-value fields are omitted.
type ListNotesParams struct {
	CreatedAfter  time.Time
	CreatedBefore time.Time
	UpdatedAfter  time.Time
	FolderID      string
	Cursor        string
	PageSize      int
}

// ListNotes fetches one page of note summaries.
func (c *Client) ListNotes(ctx context.Context, p ListNotesParams) (*ListNotesOutput, error) {
	q := url.Values{}
	if !p.CreatedAfter.IsZero() {
		q.Set("created_after", p.CreatedAfter.UTC().Format(time.RFC3339))
	}
	if !p.CreatedBefore.IsZero() {
		q.Set("created_before", p.CreatedBefore.UTC().Format(time.RFC3339))
	}
	if !p.UpdatedAfter.IsZero() {
		q.Set("updated_after", p.UpdatedAfter.UTC().Format(time.RFC3339Nano))
	}
	if p.FolderID != "" {
		q.Set("folder_id", p.FolderID)
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	if p.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(min(p.PageSize, maxPageSize)))
	}
	path := "/v1/notes"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var out ListNotesOutput
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("granola list notes: decode: %w", err)
	}
	return &out, nil
}

// GetNote fetches a full note including its transcript. The verbatim
// response body is preserved in Note.Raw.
func (c *Client) GetNote(ctx context.Context, noteID string) (*Note, error) {
	body, err := c.get(ctx, "/v1/notes/"+url.PathEscape(noteID)+"?include=transcript")
	if err != nil {
		return nil, err
	}
	var n Note
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("granola get note %s: decode: %w", noteID, err)
	}
	n.Raw = json.RawMessage(body)
	return &n, nil
}
