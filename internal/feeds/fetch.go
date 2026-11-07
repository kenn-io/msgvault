package feeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

const (
	// DefaultMaxFeedBytes bounds one feed document.
	DefaultMaxFeedBytes = int64(16 << 20)
	defaultTimeout      = 60 * time.Second
	acceptHeader        = "application/rss+xml, application/atom+xml, application/feed+json, " +
		"application/xml;q=0.9, text/xml;q=0.9, application/json;q=0.8, */*;q=0.5"
)

var (
	ErrNotAFeed       = errors.New("not an RSS, Atom or JSON feed")
	ErrFeedTooLarge   = errors.New("feed is larger than the size limit")
	errUnexpectedHTTP = errors.New("unexpected HTTP status")
)

// Validators are the HTTP cache validators of the last fetched copy. Sending
// them back lets an unchanged feed answer 304 Not Modified.
type Validators struct {
	ETag         string
	LastModified string
}

// Fetched is one fetch's outcome. Feed is nil when NotModified.
type Fetched struct {
	NotModified bool
	Feed        *gofeed.Feed
	Validators  Validators
}

// Fetcher downloads and parses feeds.
type Fetcher struct {
	Client    *http.Client
	UserAgent string
	MaxBytes  int64
}

func NewFetcher(userAgent string) *Fetcher {
	return &Fetcher{
		Client:    &http.Client{Timeout: defaultTimeout},
		UserAgent: userAgent,
		MaxBytes:  DefaultMaxFeedBytes,
	}
}

// Fetch downloads feedURL, sending validators as a conditional request, and
// parses an RSS, Atom or JSON feed.
func (f *Fetcher) Fetch(ctx context.Context, feedURL string, validators Validators) (Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return Fetched{}, fmt.Errorf("build feed request: %w", err)
	}
	req.Header.Set("Accept", acceptHeader)
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	if validators.ETag != "" {
		req.Header.Set("If-None-Match", validators.ETag)
	}
	if validators.LastModified != "" {
		req.Header.Set("If-Modified-Since", validators.LastModified)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return Fetched{NotModified: true, Validators: responseValidators(resp, validators)}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Fetched{}, fmt.Errorf("%w %d", errUnexpectedHTTP, resp.StatusCode)
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFeedBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Fetched{}, fmt.Errorf("read feed: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return Fetched{}, ErrFeedTooLarge
	}
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return Fetched{}, fmt.Errorf("%w: %w", ErrNotAFeed, err)
	}
	return Fetched{Feed: feed, Validators: responseValidators(resp, Validators{})}, nil
}

// responseValidators keeps a validator the response repeats or replaces, and
// falls back to the sent one when a 304 omits it.
func responseValidators(resp *http.Response, sent Validators) Validators {
	out := sent
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		out.ETag = etag
	}
	if modified := strings.TrimSpace(resp.Header.Get("Last-Modified")); modified != "" {
		out.LastModified = modified
	}
	return out
}
