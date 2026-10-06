package callsync

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// A recording download's deadline follows its size cap, and a redirect off the
// API host must use HTTPS and loses the account's credentials, even on a
// subdomain.
func TestMediaClient(t *testing.T) {
	const capBytes = 64 << 20
	assert.Equal(t, attachmentpolicy.DownloadTimeout(capBytes), MediaClient(&http.Client{}, capBytes).Timeout)
	for _, tc := range []struct {
		name, target string
		refused      bool
	}{
		{"HTTPS storage on another host", "https://storage.test/audio", false},
		{"HTTPS storage on a subdomain", "https://media.example.com/audio", false},
		{"plain HTTP storage", "http://storage.test/audio", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(r.Header.Get("Authorization"))
				_, _ = w.Write([]byte("audio"))
			}))
			defer storage.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("synthetic-key", r.Header.Get("Authorization"))
				http.Redirect(w, r, tc.target, http.StatusFound)
			}))
			defer api.Close()
			// Every host name resolves to the test servers: port 443 to storage.
			base := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					target := api.Listener.Addr().String()
					if strings.HasSuffix(addr, ":443") {
						target = storage.Listener.Addr().String()
					}
					return (&net.Dialer{}).DialContext(ctx, network, target)
				},
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server certificate
			}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/recording", nil)
			require.NoError(err)
			req.Header.Set("Authorization", "synthetic-key")
			res, err := MediaClient(base, 1<<20).Do(req)
			if tc.refused {
				require.ErrorIs(err, ErrRedirectRefused)
				return
			}
			require.NoError(err)
			defer func() { _ = res.Body.Close() }()
			body, err := io.ReadAll(res.Body)
			require.NoError(err)
			assert.Equal("audio", string(body))
		})
	}
}
