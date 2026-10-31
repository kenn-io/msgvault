package x

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const DefaultRevokeURL = "https://api.x.com/2/oauth2/revoke"

// RevokeCredential makes one explicit best-effort revocation request. It never
// refreshes a grant, probes an authentication style, redirects or retries.
func RevokeCredential(ctx context.Context, record TokenRecord, endpoint string, client *http.Client) error {
	if ValidateID(record.OwnerID) != nil || record.ClientID == "" || record.RefreshToken() == "" {
		return errors.New("X revocation credential is unavailable")
	}
	if endpoint == "" {
		endpoint = DefaultRevokeURL
	}
	form := url.Values{"token": {record.RefreshToken()}, "token_type_hint": {"refresh_token"}}
	if record.ClientType == PublicClient {
		form.Set("client_id", record.ClientID)
	} else if record.ClientType != ConfidentialClient {
		return errors.New("invalid X revocation client type")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot create X revocation request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if record.ClientType == ConfidentialClient {
		request.SetBasicAuth(record.ClientID, record.ClientSecret())
	}
	response, err := singleRequestHTTPClient(client).Do(request)
	if err != nil {
		return errors.Join(errors.New("X credential revocation failed"), ctx.Err())
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("X credential revocation was not confirmed")
	}
	return nil
}

// RemoveCredentials removes only this owner's known filename, even when its
// contents are malformed. The account-removal caller owns its lifecycle lock;
// unlink never follows a token-file symlink or accepts an arbitrary path.
func (m *TokenManager) RemoveCredentials(ctx context.Context, userID string) error {
	if err := ValidateID(userID); err != nil {
		return err
	}
	return m.withLock(ctx, ".x-store.lock", func() error {
		err := os.Remove(m.tokenPath(userID))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}
