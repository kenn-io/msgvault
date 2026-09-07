package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"

	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/remoteimage"
)

const (
	remoteImagePath = "/api/v1/content/remote-image"

	remoteImageMaxRequestBytes = 16 << 10 // JSON body carries one bounded URL
)

// prohibitedRemoteIP retains the proxy's policy-test seam.
func prohibitedRemoteIP(addr netip.Addr) bool { return netguard.ProhibitedIP(addr) }

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
		fetcher = remoteimage.NewFetcher()
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteimage.Timeout)
	defer cancel()
	contentType, body, ferr := fetcher.Fetch(ctx, req.URL)
	if ferr != nil {
		writeError(w, ferr.Status, ferr.Code, ferr.Message)
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
