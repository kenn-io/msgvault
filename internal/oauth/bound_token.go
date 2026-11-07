package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"

	"golang.org/x/oauth2"
)

var ErrCheckedToken = errors.New("google credential account, client or scopes could not be verified; authorize the selected app again")

// CheckedTokenSource verifies the actual account using a scope-appropriate
// profile endpoint. It returns the exact checked token as a static source, so
// later requests cannot silently switch to another stored credential. Binding
// is an opaque in-memory snapshot digest; callers must never persist it.
func (m *Manager) CheckedTokenSource(ctx context.Context, email string, requiredScopes []string) (oauth2.TokenSource, string, error) {
	tf, err := m.loadTokenFile(ctx, email)
	if err != nil || tf.ClientID == "" || tf.ClientID != m.config.ClientID {
		return nil, "", ErrCheckedToken
	}
	for _, scope := range requiredScopes {
		if !slices.Contains(tf.Scopes, canonicalScope(scope)) {
			return nil, "", ErrCheckedToken
		}
	}
	original := tf.snapshot
	token, err := m.config.TokenSource(withRefreshHTTPClient(ctx), &tf.Token).Token()
	if err != nil {
		return nil, "", ErrCheckedToken
	}
	if token.AccessToken != tf.AccessToken || token.RefreshToken != tf.RefreshToken || !token.Expiry.Equal(tf.Expiry) {
		if err := m.saveTokenComparedContext(ctx, email, token, tf.Scopes, tf); err != nil {
			return nil, "", ErrCheckedToken
		}
		original = tf.snapshot
	}
	// Reject replacement during refresh or validation instead of accepting the
	// generic source's reload-on-conflict behavior for a different grant.
	source := oauth2.StaticTokenSource(token)
	endpoint := tokenProfileEndpointForScopes(requiredScopes)
	if m.profileURL != "" {
		endpoint.url = m.profileURL
	}
	client := oauth2.NewClient(ctx, source)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = resolveTimeout
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.url, nil)
	if err != nil {
		return nil, "", ErrCheckedToken
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", ErrCheckedToken
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 || response.StatusCode != http.StatusOK {
		return nil, "", ErrCheckedToken
	}
	var profile struct {
		Email        string `json:"email"`
		EmailAddress string `json:"emailAddress"`
		ID           string `json:"id"`
	}
	if json.Unmarshal(data, &profile) != nil {
		return nil, "", ErrCheckedToken
	}
	actual := profile.Email
	if actual == "" {
		actual = profile.EmailAddress
	}
	if actual == "" {
		actual = profile.ID
	}
	if actual == "" || !SameGoogleAccount(email, actual) {
		return nil, "", ErrCheckedToken
	}
	current, err := m.loadTokenFile(ctx, email)
	if err != nil || !bytes.Equal(current.snapshot, original) {
		return nil, "", ErrCheckedToken
	}
	digest := sha256.Sum256(original)
	return source, hex.EncodeToString(digest[:]), nil
}
