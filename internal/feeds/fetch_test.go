package feeds

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
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

	_, err = fetcher.Fetch(t.Context(), "http://feeds\u3002example\u3002com:"+port+"/moved.atom", Validators{})
	require.ErrorIs(err, ErrPrivateDestination, "nor can one written with ideographic full stops, which net/http connects to as feeds.example.com")

	fetched, err = fetcher.Fetch(t.Context(), server.URL+"/moved.atom", Validators{})
	require.NoError(err, "a feed the user addressed on their own network may stay there")
	assert.Equal("Example Project releases", fetched.Feed.Title)
}

func TestFetchKeepsANumericPublicHostOffPrivateAddresses(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/feed.atom", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)

	// 16843009 is 1.1.1.1 to a resolver that reads inet_aton forms.
	_, err = publicFetcher(t, server, "16843009").Fetch(t.Context(), "http://16843009:"+port+"/moved.atom", Validators{})
	require.ErrorIs(t, err, ErrPrivateDestination, "a numeric public host can't redirect to a loopback address")
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

func TestFetchErrorLeavesOutOnlyTheFeedsOwnAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const gone = "http://gone.example/feed.atom"
	errNoHost := errors.New("no such host")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, gone, http.StatusFound)
	}))
	t.Cleanup(server.Close)
	fetcher := NewFetcher("msgvault-test")
	fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) { return nil, errNoHost }

	_, err := fetcher.Fetch(t.Context(), gone, Validators{})
	require.ErrorIs(err, errNoHost, "the reason stays")
	assert.NotContains(err.Error(), gone, "every report of a fetch already names its feed")

	_, err = fetcher.Fetch(t.Context(), server.URL+"/moved.atom", Validators{})
	require.ErrorIs(err, errNoHost)
	assert.Contains(err.Error(), gone, "the address a redirect led to stays")
}

func TestFetchRefusesAFeedWhoseXMLBaseExpandsWithoutBound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// gofeed resolves every URL against the xml:base in scope while it
	// parses, so a long base and many short references grow without bound.
	longBase := "https://example.com/" + strings.Repeat("a", 16<<10) + "/"
	atom := func(base, entry string) string {
		return `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="` + base + `"><title>Base</title>` +
			`<id>urn:example:base</id><updated>2026-11-07T15:00:00Z</updated><entry><title>Entry</title>` +
			`<id>urn:example:base:1</id><updated>2026-11-07T15:00:00Z</updated>` + entry + `</entry></feed>`
	}
	server := newFeedServer(t, map[string]string{
		"/links.atom": atom(longBase, strings.Repeat(`<link rel="related" href="x"/>`, 5000)),
		"/html.atom":  atom(longBase, `<content type="html">`+strings.Repeat("&lt;img src=x&gt;", 1000)+`</content>`),
		"/short.atom": atom("https://example.com/blog/", `<link href="post"/>`),
	})
	fetcher := NewFetcher("msgvault-test")

	for _, path := range []string{"/links.atom", "/html.atom"} {
		_, err := fetcher.Fetch(t.Context(), server.url(path), Validators{})
		require.ErrorIs(err, ErrFeedTooLarge, path)
	}
	fetched, err := fetcher.Fetch(t.Context(), server.url("/short.atom"), Validators{})
	require.NoError(err, "an ordinary xml:base is resolved as before")
	require.Len(fetched.Feed.Items, 1)
	assert.Equal("https://example.com/blog/post", fetched.Feed.Items[0].Link)
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
