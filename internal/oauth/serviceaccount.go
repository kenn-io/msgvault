package oauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ServiceAccountManager provides OAuth2 token sources via a Google service
// account with domain-wide delegation. No per-user token storage is needed —
// JWTConfig.TokenSource handles JWT signing and automatic refresh.
type ServiceAccountManager struct {
	keyData []byte
	scopes  []string
}

// NewServiceAccountManager creates a manager from a service account JSON key file.
func NewServiceAccountManager(keyPath string, scopes []string) (*ServiceAccountManager, error) {
	if len(scopes) == 0 {
		return nil, errors.New("service account requires at least one scope")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read service account key: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf(
				"service account key permissions for %s are too open (%04o); use chmod 600 %s",
				keyPath, info.Mode().Perm(), keyPath,
			)
		}
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read service account key: %w", err)
	}

	// Validate that the key parses as a service account
	if _, err := google.JWTConfigFromJSON(data, scopes...); err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}

	return &ServiceAccountManager{
		keyData: data,
		scopes:  append([]string(nil), scopes...),
	}, nil
}

// TokenSource returns an oauth2.TokenSource that impersonates the given user
// via domain-wide delegation. The returned source automatically handles JWT
// signing and token refresh (tokens are ~1 hour, re-signed transparently).
func (m *ServiceAccountManager) TokenSource(ctx context.Context, email string) (oauth2.TokenSource, error) {
	conf, err := google.JWTConfigFromJSON(m.keyData, m.scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	conf.Subject = email
	return conf.TokenSource(ctx), nil
}
