package slack

import (
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Credentials authenticates either an OAuth user token or a browser session.
// Cookie is the raw value of Slack's d cookie, not a Cookie header.
// Browser credentials can carry broader permissions than an OAuth token.
type Credentials struct {
	Token  string
	Cookie string
}

// Validate rejects incomplete browser sessions and unsafe header values.
// Errors deliberately exclude the credential values.
func (c Credentials) Validate() error {
	if c.Token == "" || strings.ContainsAny(c.Token, " \t\r\n") {
		return errors.New("slack token is empty or contains whitespace")
	}
	for _, r := range c.Token {
		if r < 0x21 || r > 0x7e {
			return errors.New("slack token contains invalid header characters")
		}
	}
	browser := strings.HasPrefix(c.Token, "xoxc-")
	if browser && c.Cookie == "" {
		return errors.New("slack browser token requires its d cookie (use --cookie-file)")
	}
	if !browser && c.Cookie != "" {
		return errors.New("slack d cookie requires an xoxc browser token")
	}
	if c.Cookie != "" && !validCookieValue(c.Cookie) {
		return errors.New("slack d cookie contains invalid cookie characters")
	}
	return nil
}

// validCookieValue accepts the RFC 6265 cookie-octet set, so the value can be
// sent as is in a Cookie header without quoting.
func validCookieValue(value string) bool {
	for i := range len(value) {
		b := value[i]
		if b < 0x21 || b > 0x7e || b == '"' || b == ',' || b == ';' || b == '\\' {
			return false
		}
	}
	return true
}

// NewClientWithCredentials creates a client with a validated credential pair.
func NewClientWithCredentials(baseURL string, credentials Credentials) (*Client, error) {
	if err := credentials.Validate(); err != nil {
		return nil, err
	}
	client := NewClient(baseURL, credentials.Token)
	client.cookie = credentials.Cookie
	return client, nil
}

// canRead reports whether a read capability may be attempted. Browser sessions
// do not report OAuth grants. Their server-side session permissions remain
// authoritative; the client does not synthesize OAuth scopes for them.
func (c *Client) canRead(scope string) bool {
	return c.cookie != "" || slices.Contains(c.scopes, scope)
}

func (c *Client) authenticate(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.cookie != "" {
		// Validate limits the value to cookie octets, so it needs no quoting.
		req.Header.Set("Cookie", "d="+c.cookie)
	}
}
