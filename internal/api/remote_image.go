package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/remoteimage"
)

const (
	remoteImagePath = "/api/v1/content/remote-image"

	remoteImageMaxBytes        = 10 << 20 // 10 MiB hard cap on the proxied body
	remoteImageTimeout         = 15 * time.Second
	remoteImageMaxRedirects    = 3
	remoteImageMaxURLBytes     = 4096
	remoteImageMaxRequestBytes = 16 << 10 // JSON body carries one bounded URL
	remoteImageUserAgent       = "msgvault-image-proxy"
)

// prohibitedRemoteIP retains the proxy's policy-test seam.
func prohibitedRemoteIP(addr netip.Addr) bool { return netguard.ProhibitedIP(addr) }

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
	f := remoteimage.NewFetcher()
	return &remoteImageFetcher{f.LookupNetIP, f.DialContext, f.MaxBytes, f.MaxRedirects}
}

func (f *remoteImageFetcher) fetch(ctx context.Context, url string) (string, []byte, *remoteImageFetchError) {
	shared := &remoteimage.Fetcher{LookupNetIP: f.lookupNetIP, DialContext: f.dialContext, MaxBytes: f.maxBytes, MaxRedirects: f.maxRedirects}
	ct, body, err := shared.Fetch(ctx, url)
	if err != nil {
		return "", nil, &remoteImageFetchError{err.Status, err.Code, err.Message}
	}
	return ct, body, nil
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
