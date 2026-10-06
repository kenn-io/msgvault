package callsync

import (
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// ErrRedirectRefused marks a redirect the client refused to follow, a
// permanent failure for that request.
var ErrRedirectRefused = errors.New("redirect refused")

// MediaClient copies base for one recording download. Its deadline follows the
// size cap instead of the API timeout, and it follows at most 5 redirects.
// Recordings may redirect to storage on another host only over HTTPS, which
// never receives the account's credentials.
func MediaClient(base *http.Client, maxBytes int64) *http.Client {
	media := *base
	media.Timeout = attachmentpolicy.DownloadTimeout(maxBytes)
	media.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("%w: too many recording redirects", ErrRedirectRefused)
		}
		if req.URL.User != nil || req.URL.Fragment != "" {
			return fmt.Errorf("%w: invalid recording redirect", ErrRedirectRefused)
		}
		if origin := via[0].URL; req.URL.Scheme == origin.Scheme && req.URL.Host == origin.Host {
			return nil
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: recording redirect off the API origin must use HTTPS", ErrRedirectRefused)
		}
		// net/http keeps credentials on a redirect to a subdomain, so drop them here.
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}
	return &media
}
