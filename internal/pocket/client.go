package pocket

import (
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
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

type Client struct {
	baseURL, mcpURL, key string
	http                 *http.Client
	wait                 func(context.Context, time.Duration) error
}

func NewClient(baseURL, mcpURL, apiKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if mcpURL == "" {
		mcpURL = DefaultMCPEndpoint
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), mcpURL: mcpURL, key: apiKey,
		http: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, wait: waitForRetry}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	for attempt := range 5 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid request endpoint", ErrContract)
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		res, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("pocket request failed")
		}
		if res.StatusCode == http.StatusOK {
			raw, err := io.ReadAll(io.LimitReader(res.Body, maxEvidenceBytes+1))
			_ = res.Body.Close()
			if err != nil {
				return nil, errors.New("pocket response read failed")
			}
			if len(raw) > maxEvidenceBytes {
				return nil, fmt.Errorf("%w: response exceeds 64 MiB", ErrContract)
			}
			return raw, nil
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 && res.StatusCode < 600 {
			if attempt == 4 {
				return nil, fmt.Errorf("pocket request exhausted five attempts (HTTP %d)", res.StatusCode)
			}
			if err := c.wait(ctx, httpretry.RetryAfter(res.Header.Get("Retry-After"), attempt, 30*time.Second)); err != nil {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("pocket request rejected (HTTP %d); check API key and account access", res.StatusCode)
	}
	return nil, errors.New("pocket request exhausted five attempts")
}

type envelope struct {
	Success    *bool          `json:"success"`
	Data       jsontext.Value `json:"data"`
	Pagination *struct {
		HasMore    *bool `json:"has_more"`
		Page       *int  `json:"page"`
		Total      *int  `json:"total"`
		TotalPages *int  `json:"total_pages"`
	} `json:"pagination"`
}

func decodeEnvelope(raw []byte) (envelope, error) {
	var e envelope
	if json.Unmarshal(raw, &e) != nil || e.Success == nil || !*e.Success || len(e.Data) == 0 || string(e.Data) == "null" {
		return e, fmt.Errorf("%w: unsuccessful or incomplete envelope", ErrContract)
	}
	return e, nil
}

func (c *Client) ListRecordings(ctx context.Context, page int) (Page, error) {
	raw, err := c.get(ctx, "/public/recordings?limit=100&page="+strconv.Itoa(page))
	if err != nil {
		return Page{}, err
	}
	e, err := decodeEnvelope(raw)
	if err != nil {
		return Page{}, err
	}
	p := e.Pagination
	if p == nil || p.HasMore == nil || p.Page == nil || *p.Page != page || page < 1 || p.Total == nil || *p.Total < 0 || p.TotalPages == nil || *p.TotalPages < 0 || *p.HasMore && *p.TotalPages <= page || !*p.HasMore && *p.TotalPages > page {
		return Page{}, fmt.Errorf("%w: invalid pagination", ErrContract)
	}
	var items []jsontext.Value
	if json.Unmarshal(e.Data, &items) != nil || items == nil {
		return Page{}, fmt.Errorf("%w: recordings must be an array", ErrContract)
	}
	out := Page{Page: page, Total: *p.Total, HasMore: *p.HasMore}
	if len(items) > 100 || len(items) == 0 && out.HasMore {
		return Page{}, fmt.Errorf("%w: invalid page size", ErrContract)
	}
	for _, item := range items {
		rec, err := DecodeRecording(item)
		if err != nil {
			return Page{}, err
		}
		out.Recordings = append(out.Recordings, rec)
	}
	return out, nil
}

func (c *Client) Recording(ctx context.Context, id string) (Recording, error) {
	raw, err := c.get(ctx, "/public/recordings/"+url.PathEscape(id)+"?include_summarizations=true&include_transcript=true")
	if err != nil {
		return Recording{}, err
	}
	e, err := decodeEnvelope(raw)
	if err != nil {
		return Recording{}, err
	}
	rec, err := DecodeRecording(e.Data)
	if err != nil {
		return rec, err
	}
	if rec.ID != id {
		return rec, fmt.Errorf("%w: recording ID mismatch", ErrContract)
	}
	return rec, nil
}
