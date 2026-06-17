package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestFetchTokenProfileEmail(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantEmail  string
		wantErr    string
		wantMis    bool
	}{
		{
			name:       "happy path",
			statusCode: http.StatusOK,
			body:       `{"emailAddress":"user@gmail.com"}`,
			wantEmail:  "user@gmail.com",
		},
		{
			name:       "mismatch",
			statusCode: http.StatusOK,
			body:       `{"emailAddress":"other@gmail.com"}`,
			wantErr:    "token mismatch",
			wantMis:    true,
		},
		{
			name:       "http error",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":"invalid credentials"}`,
			wantErr:    "gmail API returned HTTP 401",
		},
		{
			name:       "decode failure",
			statusCode: http.StatusOK,
			body:       `not json`,
			wantErr:    "parse profile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("Bearer test-token", r.Header.Get("Authorization"), "Authorization")
				w.WriteHeader(tt.statusCode)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			ts := oauth2.StaticTokenSource(&oauth2.Token{
				AccessToken: "test-token",
				TokenType:   "Bearer",
			})
			got, err := fetchTokenProfileEmail(
				context.Background(),
				ts,
				srv.URL,
				"user@gmail.com",
				tokenProfileErrorServiceAccount,
			)
			if tt.wantErr != "" {
				require.Error(err, "expected error")
				require.ErrorContains(err, tt.wantErr)
				var mismatch *TokenMismatchError
				assert.Equal(tt.wantMis, errors.As(err, &mismatch), "TokenMismatchError presence")
				return
			}
			require.NoError(err, "fetchTokenProfileEmail")
			assert.Equal(tt.wantEmail, got, "email")
		})
	}
}
