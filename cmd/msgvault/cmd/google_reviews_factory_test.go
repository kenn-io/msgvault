package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/googlereviews"
	"golang.org/x/oauth2"
)

type googleReviewTransport func(*http.Request) (*http.Response, error)

func (f googleReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGoogleReviewsDaemonFactoryChecksActualAccountAndGeneration(t *testing.T) {
	for _, change := range []string{"actual account", "client", "scope", "snapshot", "disable and restore", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			dir := t.TempDir()
			secrets := filepath.Join(dir, "client.json")
			path := filepath.Join(dir, "config.toml")
			require.NoError(os.WriteFile(secrets, []byte(`{"installed":{"client_id":"example-client","client_secret":"synthetic","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","redirect_uris":["http://localhost"]}}`), 0600))
			content := fmt.Sprintf(`[data]
data_dir = %q
[oauth.apps.business]
client_secrets = %q
[[google_reviews]]
name = "shop"
provider = "google"
email = "owner@example.com"
oauth_app = "business"
location = "accounts/a/locations/l"
enabled = true
replies_enabled = true
own_project = true
`, dir, secrets)
			require.NoError(os.WriteFile(path, []byte(content), 0600))
			cfg, err := config.Load(path, dir)
			require.NoError(err)
			cfg.Data.DataDir = dir
			manager, err := newGoogleReviewOAuthManager(t.Context(), cfg, cfg.GoogleReviews[0])
			require.NoError(err)
			tokenPath := manager.TokenPath("owner@example.com")
			require.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0700))
			token := fmt.Sprintf(`{"access_token":"checked","refresh_token":"grant","token_type":"Bearer","expiry":%q,"client_id":"example-client","scopes":[%q,%q]}`, time.Now().Add(time.Hour).Format(time.RFC3339), businessManageScope, "https://www.googleapis.com/auth/userinfo.email")
			require.NoError(os.WriteFile(tokenPath, []byte(token), 0600))
			account := "owner@example.com"
			puts, gets := 0, 0
			client := &http.Client{Transport: googleReviewTransport(func(r *http.Request) (*http.Response, error) {
				assert.Equal("Bearer checked", r.Header.Get("Authorization"))
				body := ""
				switch r.URL.Host {
				case "www.googleapis.com":
					assert.Equal("/oauth2/v2/userinfo", r.URL.Path)
					body = `{"email":"` + account + `"}`
				case "mybusiness.googleapis.com":
					if r.Method == http.MethodPut {
						puts++
						body = `{"comment":"Thanks"}`
					} else {
						gets++
						body = `{"reviewId":"r","comment":"Live review"}`
					}
				default:
					require.Fail("unexpected endpoint", r.URL.Host)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
			service := newDaemonGoogleReviews(cfg)
			defer service.Close()
			preview, err := service.Execute(ctx, "owner", googlereviews.Request{Action: "preview_reply", Target: "shop", ReviewID: "r", Comment: "Thanks"})
			require.NoError(err)
			require.NotNil(preview.Preview)
			switch change {
			case "actual account":
				account = "other@example.com"
			case "client":
				require.NoError(os.WriteFile(tokenPath, []byte(strings.Replace(token, `"client_id":"example-client"`, `"client_id":"different"`, 1)), 0600))
			case "scope":
				require.NoError(os.WriteFile(tokenPath, []byte(strings.Replace(token, businessManageScope, "missing", 1)), 0600))
			case "snapshot":
				require.NoError(os.WriteFile(tokenPath, []byte(token+"\n"), 0600))
			case "disable and restore":
				require.NoError(os.WriteFile(path, []byte(strings.Replace(content, "enabled = true", "enabled = false", 1)), 0600))
				require.NoError(os.WriteFile(path, []byte(content), 0600))
				future := time.Now().Add(time.Second)
				require.NoError(os.Chtimes(path, future, future))
			}
			_, err = service.Execute(ctx, "owner", googlereviews.Request{Action: "confirm_reply", ConfirmationToken: preview.Preview.ConfirmationToken, Confirmed: true})
			if change == "unchanged" {
				require.NoError(err)
				assert.Equal(1, puts)
				assert.Equal(2, gets)
			} else {
				require.ErrorIs(err, googlereviews.ErrConflict)
				assert.Zero(puts)
				assert.Equal(1, gets)
			}
		})
	}
}
