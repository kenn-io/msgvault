package requestsign

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ValidateBase pins a single HTTPS origin and canonical optional mount prefix.
func ValidateBase(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("signing requires an HTTPS origin and prefix without credentials, query or fragment")
	}
	if err := validatePath(u); err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = strings.TrimSuffix(u.RawPath, "/")
	return u, nil
}

func validatePath(u *url.URL) error {
	path := u.EscapedPath()
	if u.RawPath != "" && path != u.RawPath {
		return ErrInvalidSignature
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		return ErrInvalidSignature
	}
	lower := strings.ToLower(path)
	for _, escaped := range []string{"%2f", "%5c", "%25", "%2e"} {
		if strings.Contains(lower, escaped) {
			return ErrInvalidSignature
		}
	}
	if strings.Contains(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return ErrInvalidSignature
	}
	for part := range strings.SplitSeq(u.Path, "/") {
		if part == "." || part == ".." {
			return ErrInvalidSignature
		}
	}
	for _, b := range []byte(u.Path) {
		if b < 0x20 || b == 0x7f {
			return ErrInvalidSignature
		}
	}
	return nil
}

func ValidateTarget(target string) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || len(target) > 8192 {
		return nil, ErrInvalidSignature
	}
	if err := validatePath(u); err != nil {
		return nil, err
	}
	for _, b := range []byte(u.RawQuery) {
		if b < 0x21 || b > 0x7e {
			return nil, ErrInvalidSignature
		}
	}
	return u, nil
}

// ExternalTarget reconstructs authority/prefix only from operator configuration,
// retaining the proxy-stripped escaped path and exact raw query.
func ExternalTarget(base string, r *http.Request) (string, error) {
	u, err := ValidateBase(base)
	if err != nil {
		return "", err
	}
	if err := validatePath(r.URL); err != nil {
		return "", err
	}
	if r.RequestURI != "" && (!strings.HasPrefix(r.RequestURI, "/") || r.RequestURI != r.URL.RequestURI()) {
		return "", ErrInvalidSignature
	}
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	target := u.String() + path
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		target += "?" + r.URL.RawQuery
	}
	if _, err := ValidateTarget(target); err != nil {
		return "", err
	}
	return target, nil
}
