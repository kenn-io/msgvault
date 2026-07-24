package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// POST /api/v1/content/remote-image is the SSRF-hardened proxy consented
// remote mail images load through. The browser never contacts a
// sender-controlled host directly: the daemon fetches the image itself,
// which closes DNS rebinding (a public hostname resolving to a private
// address at fetch time) because every resolved address is validated here
// and the connection is dialed against the validated IP only. It also stops
// mail senders from learning the reader's browser IP.
//
// The route is POST (JSON body) rather than GET on purpose: browsers cannot
// make an <img> element or a navigation issue a POST, and the session CSRF
// middleware enforces same-origin plus X-Csrf-Token on every unsafe method,
// so a same-site sibling origin cannot trigger authenticated outbound
// fetches through a victim's session.
const (
	remoteImagePath = "/api/v1/content/remote-image"

	remoteImageMaxBytes        = 10 << 20 // 10 MiB hard cap on the proxied body
	remoteImageTimeout         = 15 * time.Second
	remoteImageMaxRedirects    = 3
	remoteImageMaxURLBytes     = 4096
	remoteImageMaxRequestBytes = 16 << 10 // JSON body carries one bounded URL
	remoteImageUserAgent       = "msgvault-image-proxy"

	remoteImageErrInvalidURL      = "invalid_url"
	remoteImageErrProhibitedHost  = "prohibited_host"
	remoteImageErrProhibitedDest  = "prohibited_destination"
	remoteImageErrFetchFailed     = "fetch_failed"
	remoteImageErrTooManyRedirect = "too_many_redirects"
	remoteImageErrUpstream        = "upstream_error"
	remoteImageErrTooLarge        = "image_too_large"
	remoteImageErrUnsupportedType = "unsupported_type"
)

// remoteImageProhibitedSuffixes mirrors the browser-side hostname gate in
// web/src/lib/content/url-safety.ts: reserved or conventionally-private
// names never resolve to a legitimate public image host.
var remoteImageProhibitedSuffixes = []string{"localhost", "local", "internal", "home.arpa"}

// remoteImageProhibitedV4 lists IPv4 ranges a proxied image fetch must never
// reach: unspecified/"this network", RFC 1918, loopback, CGNAT, link-local
// (cloud metadata), IETF protocol assignments, benchmarking, multicast, and
// reserved/broadcast space.
var remoteImageProhibitedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// remoteImageProhibitedV6 lists the IPv6 counterparts: ULA, link-local, and
// multicast. Loopback/unspecified and the IPv4-embedding transition forms are
// handled in prohibitedRemoteIP by validating the embedded IPv4 address.
var remoteImageProhibitedV6 = []netip.Prefix{
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	// 64:ff9b:1::/48 is the RFC 8215 local-use IPv4/IPv6 translation space.
	// Unlike the well-known NAT64 prefix, the embedded-IPv4 layout is
	// network-specific (RFC 8215 permits several prefix lengths), so the
	// embedded destination cannot be reliably extracted; the whole range is
	// rejected instead.
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// IPv6 transition mechanisms that embed an IPv4 destination at a fixed,
// well-defined offset. A translated packet is ultimately delivered to the
// embedded IPv4 address, so these ranges are validated against the IPv4
// prohibited list rather than the IPv6 one.
var (
	// ::/96 — deprecated IPv4-compatible addresses (::a.b.c.d).
	remoteImageV4CompatPrefix = netip.MustParsePrefix("::/96")
	// 64:ff9b::/96 — RFC 6052 well-known NAT64/DNS64 translation prefix.
	remoteImageNAT64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// 2002::/16 — RFC 3056 6to4; the IPv4 address follows the prefix.
	remoteImage6to4Prefix = netip.MustParsePrefix("2002::/16")
	// 2001::/32 — RFC 4380 Teredo; the client IPv4 is the last four bytes,
	// each XORed with 0xFF.
	remoteImageTeredoPrefix = netip.MustParsePrefix("2001::/32")
)

// embeddedIPv4 extracts the IPv4 destination embedded in an IPv6 transition
// address, reporting false for addresses that embed none.
func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	raw := addr.As16()
	switch {
	case remoteImageV4CompatPrefix.Contains(addr) || remoteImageNAT64Prefix.Contains(addr):
		// The IPv4 address is the last four bytes. ::/128 and ::1/128 map to
		// 0.0.0.0/8 here and stay prohibited.
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}), true
	case remoteImage6to4Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}), true
	case remoteImageTeredoPrefix.Contains(addr):
		return netip.AddrFrom4([4]byte{
			raw[12] ^ 0xff, raw[13] ^ 0xff, raw[14] ^ 0xff, raw[15] ^ 0xff,
		}), true
	default:
		return netip.Addr{}, false
	}
}

// prohibitedRemoteIP reports whether a resolved or literal address is a
// destination the image proxy must never dial. IPv6 forms that embed an IPv4
// destination — IPv4-mapped (::ffff:a.b.c.d), deprecated IPv4-compatible
// (::a.b.c.d), NAT64 (64:ff9b::a.b.c.d), 6to4 (2002::/16), and Teredo
// (2001::/32) — defer to the embedded IPv4 rules so an obfuscated loopback or
// NAT64-translated private destination cannot slip through as IPv6.
func prohibitedRemoteIP(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if v4, ok := embeddedIPv4(addr); ok {
			addr = v4
		}
	}
	prefixes := remoteImageProhibitedV4
	if addr.Is6() {
		prefixes = remoteImageProhibitedV6
	}
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// prohibitedRemoteHostname is the server-side port of the browser hostname
// gate for non-IP-literal hosts: reserved/conventional-private suffixes,
// unqualified single-label names, and unnormalized numeric or hex final
// labels (an IPv4 obfuscation the platform resolver may still parse as an
// address) are all rejected before any DNS query is made.
func prohibitedRemoteHostname(hostname string) bool {
	host := strings.ToLower(strings.TrimSpace(hostname))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return true
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return true
	}
	last := labels[len(labels)-1]
	if isDecimalLabel(last) || isHexLabel(last) {
		return true
	}
	for _, suffix := range remoteImageProhibitedSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func isDecimalLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, r := range label {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHexLabel(label string) bool {
	rest, ok := strings.CutPrefix(label, "0x")
	if !ok {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// remoteImageFetchError is a fetch failure mapped to an API error response.
// The fetched bytes are never echoed on error.
type remoteImageFetchError struct {
	status  int
	code    string
	message string
}

// remoteImageFetcher performs the hardened fetch. The resolver and dialer
// are injectable so tests can simulate DNS rebinding and private resolution
// without real DNS; production uses the default resolver and a plain dialer.
type remoteImageFetcher struct {
	lookupNetIP  func(ctx context.Context, host string) ([]netip.Addr, error)
	dialContext  func(ctx context.Context, network, address string) (net.Conn, error)
	maxBytes     int64
	maxRedirects int
}

func newRemoteImageFetcher() *remoteImageFetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &remoteImageFetcher{
		lookupNetIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolving remote image host %q: %w", host, err)
			}
			return addrs, nil
		},
		dialContext:  dialer.DialContext,
		maxBytes:     remoteImageMaxBytes,
		maxRedirects: remoteImageMaxRedirects,
	}
}

// validateTarget applies every pre-connection check to one URL hop and
// returns the address the connection must be pinned to: scheme http/https,
// no userinfo, hostname-spelling gate, private-literal rejection, and
// validation of every resolved A/AAAA answer. Dialing only the returned
// address closes the check-then-resolve-again (TOCTOU rebinding) window.
func (f *remoteImageFetcher) validateTarget(
	ctx context.Context, rawURL string,
) (*url.URL, netip.AddrPort, *remoteImageFetchError) {
	var none netip.AddrPort
	if len(rawURL) > remoteImageMaxURLBytes {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL is too long"}
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL could not be parsed"}
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != schemeHTTPS {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL must use http or https"}
	}
	if target.User != nil {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL must not carry credentials"}
	}
	host := target.Hostname()
	if host == "" {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL has no host"}
	}
	port := uint16(80)
	if scheme == schemeHTTPS {
		port = 443
	}
	if portText := target.Port(); portText != "" {
		parsed, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || parsed == 0 {
			return nil, none, &remoteImageFetchError{
				http.StatusBadRequest, remoteImageErrInvalidURL, "Remote image URL has an invalid port"}
		}
		port = uint16(parsed)
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		if prohibitedRemoteIP(literal) {
			return nil, none, &remoteImageFetchError{
				http.StatusBadRequest, remoteImageErrProhibitedHost,
				"Remote image URL targets a private or reserved address"}
		}
		return target, netip.AddrPortFrom(literal.Unmap(), port), nil
	}
	if prohibitedRemoteHostname(host) {
		return nil, none, &remoteImageFetchError{
			http.StatusBadRequest, remoteImageErrProhibitedHost,
			"Remote image URL targets a reserved or private hostname"}
	}
	addrs, err := f.lookupNetIP(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, none, &remoteImageFetchError{
			http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image host could not be resolved"}
	}
	if slices.ContainsFunc(addrs, prohibitedRemoteIP) {
		return nil, none, &remoteImageFetchError{
			http.StatusBadGateway, remoteImageErrProhibitedDest,
			"Remote image host resolves to a private or reserved address"}
	}
	return target, netip.AddrPortFrom(addrs[0].Unmap(), port), nil
}

// doPinned performs one GET against a single validated hop. The transport's
// DialContext ignores the address derived from the URL and dials the
// already-validated IP, so a rebinding resolver cannot swap the destination
// between validation and connection. TLS verification still uses the URL
// hostname. No cookies or stored credentials ever travel outbound.
func (f *remoteImageFetcher) doPinned(
	ctx context.Context, target *url.URL, pinned netip.AddrPort,
) (*http.Response, error) {
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
			return f.dialContext(dialCtx, network, pinned.String())
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
	// SSRF mitigation. validateTarget rejected private/reserved hosts and
	// resolved addresses, and the transport above dials only the pinned,
	// validated IP.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building remote image request: %w", err)
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", remoteImageUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching remote image: %w", err)
	}
	return resp, nil
}

// fetch retrieves one consented remote image, re-validating and re-pinning
// every redirect hop, and enforces the image/* content type and the byte
// cap before any byte is returned.
func (f *remoteImageFetcher) fetch(
	ctx context.Context, rawURL string,
) (contentType string, body []byte, fetchErr *remoteImageFetchError) {
	current := rawURL
	for hop := 0; ; hop++ {
		target, pinned, ferr := f.validateTarget(ctx, current)
		if ferr != nil {
			return "", nil, ferr
		}
		resp, err := f.doPinned(ctx, target, pinned)
		if err != nil {
			return "", nil, &remoteImageFetchError{
				http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image could not be fetched"}
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if hop >= f.maxRedirects {
				return "", nil, &remoteImageFetchError{
					http.StatusBadGateway, remoteImageErrTooManyRedirect, "Remote image redirected too many times"}
			}
			next, err := target.Parse(location)
			if location == "" || err != nil {
				return "", nil, &remoteImageFetchError{
					http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image redirect target is invalid"}
			}
			current = next.String()
			continue
		case http.StatusOK:
			contentType, body, fetchErr = f.readImageBody(resp)
			_ = resp.Body.Close()
			return contentType, body, fetchErr
		default:
			_ = resp.Body.Close()
			return "", nil, &remoteImageFetchError{
				http.StatusBadGateway, remoteImageErrUpstream,
				fmt.Sprintf("Remote image host returned status %d", resp.StatusCode)}
		}
	}
}

func (f *remoteImageFetcher) readImageBody(resp *http.Response) (string, []byte, *remoteImageFetchError) {
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if base, _, found := strings.Cut(contentType, ";"); found {
		contentType = strings.TrimSpace(base)
	}
	if !strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "image/svg") {
		return "", nil, &remoteImageFetchError{
			http.StatusUnsupportedMediaType, "unsupported_type", "Remote content is not a permitted image type"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return "", nil, &remoteImageFetchError{
			http.StatusBadGateway, remoteImageErrFetchFailed, "Remote image body could not be read"}
	}
	if int64(len(body)) > f.maxBytes {
		return "", nil, &remoteImageFetchError{
			http.StatusBadGateway, remoteImageErrTooLarge, "Remote image exceeds the proxy size limit"}
	}
	return contentType, body, nil
}

// RemoteImageRequest is the JSON body of POST /api/v1/content/remote-image.
type RemoteImageRequest struct {
	URL string `json:"url" doc:"Absolute http(s) URL of the consented remote image"`
}

// handleRemoteImage serves POST /api/v1/content/remote-image. Success passes
// through only the upstream Content-Type plus the bytes; the API-wide
// Cache-Control: no-store middleware covers caching.
func (s *Server) handleRemoteImage(w http.ResponseWriter, r *http.Request) {
	var req RemoteImageRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, remoteImageMaxRequestBytes))
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Request body must be a JSON object with a 'url' field")
		return
	}
	if !requireSingleJSONValue(w, decoder, "invalid_request") {
		return
	}
	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "missing_url", "Missing 'url' in request body")
		return
	}
	fetcher := s.remoteImages
	if fetcher == nil {
		fetcher = newRemoteImageFetcher()
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteImageTimeout)
	defer cancel()
	contentType, body, ferr := fetcher.fetch(ctx, req.URL)
	if ferr != nil {
		writeError(w, ferr.status, ferr.code, ferr.message)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Not XSS-reachable: readImageBody enforced a non-SVG image/* content
	// type, nosniff pins it, and the frontend consumes the bytes as a blob
	// URL inside a sandboxed srcdoc frame.
	_, _ = w.Write(body)
}
