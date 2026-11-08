package feeds

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// publicFetcher fetches as if host resolved to a public address while it
// connects to server, so tests can play a feed on the public internet.
func publicFetcher(t *testing.T, server *httptest.Server, host string) *Fetcher {
	t.Helper()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	public := netip.MustParseAddr("1.1.1.1")
	fetcher := NewFetcher("msgvault-test")
	fetcher.LookupNetIP = func(_ context.Context, name string) ([]netip.Addr, error) {
		if name == host {
			return []netip.Addr{public}, nil
		}
		return net.DefaultResolver.LookupNetIP(t.Context(), "ip", name)
	}
	dialer := &net.Dialer{Timeout: time.Second}
	fetcher.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if addr, err := netip.ParseAddrPort(address); err == nil && addr.Addr() == public {
			address = serverURL.Host
		}
		return dialer.DialContext(ctx, network, address)
	}
	return fetcher
}

func TestFetchKeepsAPublicFeedOffPrivateAddresses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/feed.atom":
			_, _ = w.Write([]byte(atomFeed))
		case "/moved.atom":
			http.Redirect(w, r, server.URL+"/feed.atom", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err)
	fetcher := publicFetcher(t, server, "feeds.example.com")
	public := "http://feeds.example.com:" + port

	fetched, err := fetcher.Fetch(t.Context(), public+"/feed.atom", Validators{})
	require.NoError(err, "a public feed is fetched")
	assert.Equal("Example Project releases", fetched.Feed.Title)

	_, err = fetcher.Fetch(t.Context(), public+"/moved.atom", Validators{})
	require.ErrorIs(err, ErrPrivateDestination, "a public feed can't redirect to a loopback address")

	fetched, err = fetcher.Fetch(t.Context(), server.URL+"/moved.atom", Validators{})
	require.NoError(err, "a feed the user addressed on their own network may stay there")
	assert.Equal("Example Project releases", fetched.Feed.Title)
}

func TestFetchClosesItsConnection(t *testing.T) {
	keptAlive := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keptAlive <- !r.Close
		_, _ = w.Write([]byte(atomFeed))
	}))
	t.Cleanup(server.Close)

	_, err := NewFetcher("msgvault-test").Fetch(t.Context(), server.URL+"/feed.atom", Validators{})
	require.NoError(t, err)
	assert.False(t, <-keptAlive, "a one-fetch client leaves no idle connection behind")
}

func TestFollowRejectsAnUnsolicitedNotModified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(server.Close)

	st := testutil.NewTestStore(t)
	summary, err := newTestSyncer(st, time.Now()).Follow(t.Context(), []string{server.URL + "/feed.atom"})
	require.NoError(err)
	require.Len(summary.Feeds, 1)
	require.Error(summary.Feeds[0].Err)
	assert.Contains(summary.Feeds[0].Err.Error(), "304")
	list, err := NewRegistry(st).List(t.Context())
	require.NoError(err)
	assert.Empty(list, "a feed that answered nothing is not followed")
}
