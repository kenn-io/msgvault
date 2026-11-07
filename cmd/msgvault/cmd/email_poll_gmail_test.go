package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json/v2"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRapidEmailGmailProfileMismatchDoesNotMutateArchive(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyJSON, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "service@example.test",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":   "https://token.example.test/oauth2",
	})
	require.NoError(t, err)
	keyPath := filepath.Join(dir, "service-account.json")
	require.NoError(t, os.WriteFile(keyPath, keyJSON, 0o600))
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
`), 0o600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	cfg.OAuth.ServiceAccountKey = keyPath
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth2":
			if !assert.NoError(t, r.ParseForm()) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			if !assert.Len(t, parts, 3) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var claims map[string]any
			if !assert.NoError(t, json.Unmarshal(claimsJSON, &claims)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Equal(t, oauth.ScopeGmailReadonly, claims["scope"], "rapid acquisition does not widen a service-account grant")
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`))
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"other@example.test","historyId":"100"}`))
		default:
			assert.Fail(t, "unexpected provider operation after a mismatched profile", "%s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	loopback, err := url.Parse(server.URL)
	require.NoError(t, err)
	assert := assert.New(t)
	require := require.New(t)
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = loopback.Scheme, loopback.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = transport })
	st := storetest.New(t)
	src, err := st.Store.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(err)
	require.NoError(st.Store.UpdateSourceSyncCursor(src.ID, "100"))
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	err = runScheduledSync(ctx, src.Identifier, st.Store, nil, invocationFromContext(ctx))
	require.ErrorContains(err, "reauthorize", "authenticated mismatch must fail closed with stable remediation")
	for _, table := range []string{"account_identities", "sync_runs", "messages"} {
		var count int
		require.NoError(st.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		assert.Zero(count, "profile verification precedes every %s mutation", table)
	}
	current, err := st.Store.GetSourceByID(src.ID)
	require.NoError(err)
	assert.Equal("100", current.SyncCursor.String)
}
