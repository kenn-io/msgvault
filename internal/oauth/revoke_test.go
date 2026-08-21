package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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
		wantErrIs   error
		errContains string
	}{
		{
			name:   "success",
			status: http.StatusOK,
		},
		{
			// Google answers 400 invalid_token for an already expired or
			// revoked credential; account removal reads the typed error as
			// nothing-to-do.
			name:      "already invalid credential is a typed error",
			status:    http.StatusBadRequest,
			body:      `{"error": "invalid_token"}`,
			wantErr:   true,
			wantErrIs: ErrRevokeCredentialInvalid,
		},
		{
			name:        "other 400 is a real failure",
			status:      http.StatusBadRequest,
			body:        `{"error": "invalid_request"}`,
			wantErr:     true,
			errContains: "invalid_request",
		},
		{
			name:        "server error is a real failure",
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
				if tt.wantErrIs != nil {
					require.ErrorIs(err, tt.wantErrIs)
				}
				if tt.errContains != "" {
					assert.Contains(err.Error(), tt.errContains)
				}
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

// TestRevokeStoredCredential_NoTokenFile pins that the client-config-free
// adapter used by account removal reports a missing file as os.ErrNotExist,
// which removal treats as nothing-to-revoke. The revocation body itself is
// covered by TestRevokeToken.
func TestRevokeStoredCredential_NoTokenFile(t *testing.T) {
	err := RevokeStoredCredential(context.Background(), t.TempDir(), "missing@example.com")

	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestFindEquivalentTokenEmails covers the Gmail alias rules the read-only
// decision must honor: authorization accepts these variants as the same
// account, so token lookup has to as well or a --readonly run through an
// alias spelling reads as a fresh account while the stored spelling keeps
// its credential.
//
// Case-only variants are deliberately absent: on a case-insensitive
// filesystem they resolve to the account's own file (excluded via
// os.SameFile, matching what HasToken sees), while on a case-sensitive one
// they are distinct files that do match — the dot/plus/googlemail cases
// below behave identically everywhere.
func TestFindEquivalentTokenEmails(t *testing.T) {
	mgr := setupTestManager(t, Scopes)
	writeTokenFile(t, mgr, "username@gmail.com", testToken, Scopes)
	writeTokenFile(t, mgr, "user.name@googlemail.com", testToken, Scopes)
	writeTokenFile(t, mgr, "other@example.com", testToken, Scopes)

	tests := []struct {
		name  string
		email string
		want  []string
	}{
		{
			name:  "dot variant matches every equivalent spelling",
			email: "user.name@gmail.com",
			want:  []string{"user.name@googlemail.com", "username@gmail.com"},
		},
		{
			name:  "plus address matches every equivalent spelling",
			email: "username+archive@gmail.com",
			want:  []string{"user.name@googlemail.com", "username@gmail.com"},
		},
		{
			name:  "exact spelling is not its own equivalent",
			email: "username@gmail.com",
			want:  []string{"user.name@googlemail.com"},
		},
		{
			name:  "unrelated gmail account does not match",
			email: "someoneelse@gmail.com",
		},
		{
			name:  "unrelated non-gmail account does not match",
			email: "different@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, mgr.FindEquivalentTokenEmails(tt.email))
		})
	}
}
