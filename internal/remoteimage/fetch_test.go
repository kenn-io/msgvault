package remoteimage

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A real upstream with only DNS/dial substituted exercises the same pinned
// transport as production without ever contacting an external image host.
func TestFetcherPinsPublicAddressAndRejectsPrivateRedirect(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Empty(r.Header.Get("Cookie"))
		assert.Empty(r.Header.Get("Referer"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
	}))
	defer upstream.Close()
	f := NewFetcher()
	f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	f.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		assert.Equal("93.184.216.34:80", address)
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	ct, body, err := f.Fetch(t.Context(), "http://images.example/chart")
	require.Nil(err)
	assert.Equal("image/png", ct)
	assert.Equal([]byte("\x89PNG\r\n\x1a\nsynthetic"), body)
	_, _, err = f.Fetch(t.Context(), "http://images.example/redirect")
	require.NotNil(err)
	assert.Equal("prohibited_host", err.Code)
	assert.Equal(int64(2), requests.Load())
}
