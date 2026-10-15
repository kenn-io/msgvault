// Package remoteimage fetches and archives explicitly consented remote mail images.
package remoteimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

// The proxy and archiver share destination validation and pinned transports.
// Callers own consent; a successful fetch must not imply permission to fetch
// another URL or enable automatic archiving.
const (
	remoteImageMaxRedirects = 3
	remoteImageMaxURLBytes  = 4096
	remoteImageUserAgent    = "msgvault-image-proxy"

	remoteImageErrInvalidURL      = "invalid_url"
	remoteImageErrProhibitedHost  = "prohibited_host"
	remoteImageErrProhibitedDest  = "prohibited_destination"
	remoteImageErrFetchFailed     = "fetch_failed"
	remoteImageErrTooManyRedirect = "too_many_redirects"
	remoteImageErrUpstream        = "upstream_error"
	remoteImageErrTooLarge        = "image_too_large"
	remoteImageErrUnsupportedType = "unsupported_type"
)

// MaxImageBytes caps each downloaded or locally served remote image at 10 MiB.
const MaxImageBytes = 10 << 20

// Timeout bounds each remote image fetch in the proxy and archiver.
const Timeout = 15 * time.Second

// prohibitedRemoteIP and prohibitedRemoteHostname retain the package-private
// wrappers used by the image proxy while the shared policy lives in netguard.
func prohibitedRemoteIP(addr netip.Addr) bool { return netguard.ProhibitedIP(addr) }

func prohibitedRemoteHostname(hostname string) bool { return netguard.ProhibitedHostname(hostname) }

// FetchError is a fetch failure mapped to an API error response.
// The fetched bytes are never echoed on error.
type FetchError struct {
	Status  int
	Code    string
	Message string
}

// Fetcher performs the hardened fetch. The resolver and dialer
// are injectable so tests can simulate DNS rebinding and private resolution
// without real DNS; production uses the default resolver and a plain dialer.
type Fetcher struct {
	LookupNetIP     func(ctx context.Context, host string) ([]netip.Addr, error)
	DialContext     func(ctx context.Context, network, address string) (net.Conn, error)
	MaxBytes        int64
	MaxRedirects    int
	PrivateOrigin   *url.URL // Only this configured origin may resolve to explicit private ranges or its own loopback.
	TryAllAddresses bool
	Accept          string
	UserAgent       string
}

func NewFetcher() *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &Fetcher{
		LookupNetIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolving remote image host %q: %w", host, err)
			}
			return addrs, nil
		},
		DialContext:  dialer.DialContext,
		MaxBytes:     MaxImageBytes,
		MaxRedirects: remoteImageMaxRedirects,
		Accept:       "image/*",
		UserAgent:    remoteImageUserAgent,
	}
}

// ValidateTarget checks every DNS answer and returns the addresses that connections must use.
func (f *Fetcher) ValidateTarget(
	ctx context.Context, rawURL string,
) (*url.URL, []netip.AddrPort, *FetchError) {
	var none []netip.AddrPort
	if len(rawURL) > remoteImageMaxURLBytes {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL is too long"}
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL could not be parsed"}
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL must use http or https"}
	}
	if target.User != nil {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL must not carry credentials"}
	}
	host := target.Hostname()
	if host == "" {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL has no host"}
	}
	port, err := netguard.TargetPort(target)
	if err != nil {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL has an invalid port"}
	}
	target.Scheme = scheme
	privateOrigin := f.PrivateOrigin != nil && strings.EqualFold(f.PrivateOrigin.Scheme, scheme) &&
		strings.EqualFold(strings.TrimSuffix(f.PrivateOrigin.Hostname(), "."), strings.TrimSuffix(host, ".")) &&
		netguard.EffectivePort(f.PrivateOrigin) == strconv.Itoa(int(port))
	loopbackOrigin := privateOrigin && netguard.IsLoopbackHost(f.PrivateOrigin.Hostname())
	allowed := func(addr netip.Addr) bool {
		if !addr.IsValid() || addr.Zone() != "" {
			return false
		}
		addr = addr.Unmap()
		if loopbackOrigin {
			return addr.IsLoopback()
		}
		return !prohibitedRemoteIP(addr) || (privateOrigin && netguard.ExplicitPrivateAddress(addr))
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		if !allowed(literal) {
			return nil, none, &FetchError{
				http.StatusBadRequest, remoteImageErrProhibitedHost,
				"Remote image URL targets a private or reserved address"}
		}
		return target, []netip.AddrPort{netip.AddrPortFrom(literal.Unmap(), port)}, nil
	}
	if prohibitedRemoteHostname(host) && !privateOrigin {
		return nil, none, &FetchError{
			http.StatusBadRequest, remoteImageErrProhibitedHost,
			"Remote image URL targets a reserved or private hostname"}
	}
	addrs, err := f.LookupNetIP(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, none, &FetchError{
			http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image host could not be resolved"}
	}
	pinned := make([]netip.AddrPort, 0, len(addrs))
	seen := make(map[netip.AddrPort]bool, len(addrs))
	for _, addr := range addrs {
		if !allowed(addr) {
			return nil, none, &FetchError{
				http.StatusBadGateway, remoteImageErrProhibitedDest,
				"Remote image host resolves to a private or reserved address"}
		}
		candidate := netip.AddrPortFrom(addr.Unmap(), port)
		if !seen[candidate] {
			seen[candidate] = true
			pinned = append(pinned, candidate)
		}
	}
	if !f.TryAllAddresses {
		pinned = pinned[:1]
	}
	return target, pinned, nil
}

// doPinned performs one GET against a single validated hop. The transport's
// DialContext ignores the address derived from the URL and dials the
// already-validated IP, so a rebinding resolver cannot swap the destination
// between validation and connection. TLS verification still uses the URL
// hostname. No cookies or stored credentials ever travel outbound.
func (f *Fetcher) doPinned(
	ctx context.Context, target *url.URL, pinned []netip.AddrPort,
) (*http.Response, error) {
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
			var failures []error
			for _, address := range pinned {
				conn, err := f.DialContext(dialCtx, network, address.String())
				if err == nil {
					return conn, nil
				}
				failures = append(failures, err)
			}
			return nil, errors.Join(failures...)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Redirects are followed manually in fetch so every hop is
			// re-validated and re-pinned.
			return http.ErrUseLastResponse
		},
	}
	// The user-supplied URL is intentionally fetched: this handler IS the
	// SSRF mitigation. ValidateTarget rejected private/reserved hosts and
	// resolved addresses, and the transport above dials only the pinned,
	// validated IP.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building remote image request: %w", err)
	}
	if f.Accept != "" {
		req.Header.Set("Accept", f.Accept)
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching remote image: %w", err)
	}
	return resp, nil
}

// Fetch retrieves one consented remote image, re-validating and re-pinning
// every redirect hop, and enforces the image/* content type and the byte
// cap before any byte is returned.
func (f *Fetcher) Fetch(
	ctx context.Context, rawURL string,
) (contentType string, body []byte, fetchErr *FetchError) {
	resp, ferr := f.Open(ctx, rawURL)
	if ferr != nil {
		return "", nil, ferr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil, &FetchError{
			http.StatusBadGateway, remoteImageErrUpstream,
			fmt.Sprintf("Remote image host returned status %d", resp.StatusCode)}
	}
	return f.readImageBody(resp)
}

// Open returns a credential-free response after validating and pinning each redirect hop.
func (f *Fetcher) Open(ctx context.Context, rawURL string) (*http.Response, *FetchError) {
	current := rawURL
	for hop := 0; ; hop++ {
		target, pinned, ferr := f.ValidateTarget(ctx, current)
		if ferr != nil {
			return nil, ferr
		}
		resp, err := f.doPinned(ctx, target, pinned)
		if err != nil {
			return nil, &FetchError{
				http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image could not be fetched"}
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hop >= f.MaxRedirects {
				return nil, &FetchError{
					http.StatusBadGateway, remoteImageErrTooManyRedirect, "Remote image redirected too many times"}
			}
			next, err := target.Parse(location)
			if location == "" || err != nil {
				return nil, &FetchError{
					http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image redirect target is invalid"}
			}
			current = next.String()
			continue
		default:
			return resp, nil
		}
	}
}

func (f *Fetcher) readImageBody(resp *http.Response) (string, []byte, *FetchError) {
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if base, _, found := strings.Cut(contentType, ";"); found {
		contentType = strings.TrimSpace(base)
	}
	if !strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "image/svg") {
		return "", nil, &FetchError{
			http.StatusUnsupportedMediaType, remoteImageErrUnsupportedType, "Remote content is not a permitted image type"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return "", nil, &FetchError{
			http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image body could not be read"}
	}
	if int64(len(body)) > f.MaxBytes {
		return "", nil, &FetchError{
			http.StatusBadGateway, remoteImageErrTooLarge, "Remote image exceeds the proxy size limit"}
	}
	return contentType, body, nil
}

func (e *FetchError) Error() string { return e.Message }
