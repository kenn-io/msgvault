package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// newRevokeServer records the credential each revocation request carries and
// responds with the given status and body.
func newRevokeServer(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var revoked []string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.NoError(t, r.ParseForm())
			revoked = append(revoked, r.PostForm.Get("token"))
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)
	return srv, &revoked
}

func TestRevokeToken(t *testing.T) {
	const email = "user@example.com"

	tests := []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		errContains string
	}{
		{
			name:   "success",
			status: http.StatusOK,
		},
		{
			// Google answers 400 invalid_token for an already expired or
			// revoked credential. That is the state revocation is trying to
			// reach, so it must not fail the narrowing run.
			name:   "already revoked counts as success",
			status: http.StatusBadRequest,
			body:   `{"error": "invalid_token"}`,
		},
		{
			name:        "other 400 fails closed",
			status:      http.StatusBadRequest,
			body:        `{"error": "invalid_request"}`,
			wantErr:     true,
			errContains: "invalid_request",
		},
		{
			name:        "server error fails closed",
			status:      http.StatusInternalServerError,
			body:        "boom",
			wantErr:     true,
			errContains: "HTTP 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			srv, revoked := newRevokeServer(t, tt.status, tt.body)
			mgr := setupTestManager(t, Scopes)
			mgr.revokeURL = srv.URL
			writeTokenFile(t, mgr, email, oauth2.Token{
				AccessToken:  "access-token",
				RefreshToken: "refresh-token",
				TokenType:    "Bearer",
				Expiry:       time.Now().Add(time.Hour),
			}, Scopes)

			err := mgr.RevokeToken(context.Background(), email)

			if tt.wantErr {
				require.Error(err)
				assert.Contains(err.Error(), tt.errContains)
			} else {
				require.NoError(err)
			}
			assert.Equal([]string{"refresh-token"}, *revoked,
				"revocation must target the refresh token, which kills the grant")
		})
	}
}

// TestRevokeToken_FallsBackToAccessToken covers token files without a refresh
// token (e.g. a partially written or hand-copied file): the access token is
// the only revocable credential left.
func TestRevokeToken_FallsBackToAccessToken(t *testing.T) {
	srv, revoked := newRevokeServer(t, http.StatusOK, "")
	mgr := setupTestManager(t, Scopes)
	mgr.revokeURL = srv.URL
	writeTokenFile(t, mgr, "user@example.com", oauth2.Token{
		AccessToken: "access-only",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
	}, Scopes)

	require.NoError(t, mgr.RevokeToken(context.Background(), "user@example.com"))
	assert.Equal(t, []string{"access-only"}, *revoked)
}

func TestRevokeToken_NoTokenFile(t *testing.T) {
	mgr := setupTestManager(t, Scopes)
	mgr.revokeURL = "http://127.0.0.1:0" // must not be contacted

	err := mgr.RevokeToken(context.Background(), "missing@example.com")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "load token for missing@example.com")
}

func TestRevokeToken_NoCredential(t *testing.T) {
	mgr := setupTestManager(t, Scopes)
	mgr.revokeURL = "http://127.0.0.1:0" // must not be contacted
	writeTokenFile(t, mgr, "user@example.com", oauth2.Token{}, Scopes)

	err := mgr.RevokeToken(context.Background(), "user@example.com")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential to revoke")
}
