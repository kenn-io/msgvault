package gmail

import (
	"net/http"

	"golang.org/x/oauth2"
)

// NewNativePermissionFixtureClient redirects only network transport to the
// synthetic receiver. Production Gmail requests, parsers and grant inspection
// remain intact. This helper is compiled only into the Gmail test binary.
func NewNativePermissionFixtureClient(url string, grants func() []string) *Client {
	client := NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-access"}), WithGrantedScopesReader(grants), WithRateLimiter(NewRateLimiter(1000)))
	client.httpClient = &http.Client{Transport: &rewriteTransport{base: url, wrapped: http.DefaultTransport}}
	return client
}

// NewNativeRefreshFixtureClient replaces only the network destination while
// retaining the native OAuth token refresh transport and Gmail parser.
func NewNativeRefreshFixtureClient(url string, token oauth2.TokenSource) *Client {
	client := NewClient(token, WithRateLimiter(NewRateLimiter(1000)))
	client.httpClient = &http.Client{Transport: &oauth2.Transport{Source: token, Base: &rewriteTransport{base: url, wrapped: http.DefaultTransport}}}
	return client
}
