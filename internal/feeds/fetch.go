package feeds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"go.kenn.io/msgvault/internal/netguard"
)

const (
	// DefaultMaxFeedBytes bounds one feed document.
	DefaultMaxFeedBytes = int64(16 << 20)
	defaultTimeout      = 60 * time.Second
	dialTimeout         = 10 * time.Second
	maxRedirects        = 5
	acceptHeader        = "application/rss+xml, application/atom+xml, application/feed+json, " +
		"application/xml;q=0.9, text/xml;q=0.9, application/json;q=0.8, */*;q=0.5"
)

var (
	ErrNotAFeed       = errors.New("not an RSS, Atom or JSON feed")
	ErrFeedTooLarge   = errors.New("feed is larger than the size limit")
	errUnexpectedHTTP = errors.New("unexpected HTTP status")
	// ErrPrivateDestination means a feed on the public internet led to a
	// private, loopback or reserved address, usually through a redirect.
	ErrPrivateDestination = errors.New("feed leads to a private or reserved address")
	errTooManyRedirects   = errors.New("feed redirects too many times")
)

// Validators are the HTTP cache validators of the last fetched copy. Sending
// them back lets an unchanged feed answer 304 Not Modified.
type Validators struct {
	ETag         string
	LastModified string
}

func (v Validators) empty() bool {
	return v.ETag == "" && v.LastModified == ""
}

// Fetched is one fetch's outcome. Feed is nil when NotModified. URL is the
// address the document came from after redirects, the base its relative
// links resolve against.
type Fetched struct {
	NotModified bool
	Feed        *gofeed.Feed
	URL         string
	Validators  Validators
}

// Fetcher downloads and parses feeds. A feed whose URL names a public host
// may only reach public addresses, redirects included, so a publisher can't
// point msgvault at the user's own machine or network. A feed whose URL
// names a private address or local host name was chosen as such and may
// reach private addresses.
type Fetcher struct {
	UserAgent string
	MaxBytes  int64
	Timeout   time.Duration
	// LookupNetIP and DialContext are the network; tests replace them.
	LookupNetIP func(ctx context.Context, host string) ([]netip.Addr, error)
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

func NewFetcher(userAgent string) *Fetcher {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &Fetcher{
		UserAgent: userAgent,
		MaxBytes:  DefaultMaxFeedBytes,
		Timeout:   defaultTimeout,
		LookupNetIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		DialContext: dialer.DialContext,
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
	resp, err := f.client(privateHost(req.URL.Hostname())).Do(req)
	if err != nil {
		return Fetched{}, fmt.Errorf("fetch feed: %w", withoutFeedURL(err, feedURL))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		// Without validators there is no copy the answer could refer to.
		if validators.empty() {
			return Fetched{}, fmt.Errorf("%w %d to an unconditional request", errUnexpectedHTTP, resp.StatusCode)
		}
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
	if err := checkBaseExpansion(body); err != nil {
		return Fetched{}, err
	}
	feed, err := parseFeed(body)
	if err != nil {
		return Fetched{}, fmt.Errorf("%w: %w", ErrNotAFeed, err)
	}
	return Fetched{Feed: feed, URL: resp.Request.URL.String(), Validators: responseValidators(resp, Validators{})}, nil
}

// client returns an HTTP client whose every connection, redirects included,
// is checked against the feed's network. It uses no proxy and no cookies,
// and closes each connection after its response, since the client is
// built for one fetch and nothing would reuse or close an idle one.
func (f *Fetcher) client(allowPrivate bool) *http.Client {
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         f.guardedDial(allowPrivate),
			TLSHandshakeTimeout: dialTimeout,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
	}
}

// guardedDial resolves the host itself and, unless private addresses are
// allowed, refuses a host with any private, loopback or reserved address
// before connecting to one of its checked addresses.
func (f *Fetcher) guardedDial(allowPrivate bool) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("feed address %q: %w", address, err)
		}
		addrs, err := f.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		if !allowPrivate && slices.ContainsFunc(addrs, netguard.ProhibitedIP) {
			return nil, fmt.Errorf("%w: %s", ErrPrivateDestination, host)
		}
		var failures []error
		for _, addr := range addrs {
			conn, err := f.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
		}
		return nil, errors.Join(failures...)
	}
}

func (f *Fetcher) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{literal}, nil
	}
	addrs, err := f.LookupNetIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve feed host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve feed host %s: no addresses", host)
	}
	return addrs, nil
}

// privateHost reports whether a feed URL's host names a private, loopback
// or reserved address, or a local name such as localhost or nas.local, so
// the user chose a feed on their own network.
func privateHost(host string) bool {
	host = strings.Trim(host, "[]")
	if literal, err := netip.ParseAddr(host); err == nil {
		return netguard.ProhibitedIP(literal)
	}
	return netguard.ProhibitedHostname(host)
}

// withoutFeedURL drops the feed's own address from a transport error: every
// report of a failed fetch names the feed beside it. An address a redirect
// led to differs from the feed's, so it stays.
func withoutFeedURL(err error, feedURL string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.URL == feedURL {
		return urlErr.Err
	}
	return err
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
