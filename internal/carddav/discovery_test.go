package carddav

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryCollectionComparisonNormalizesOriginAndPath(t *testing.T) {
	left, err := url.Parse("https://contacts.example/book/")
	require.NoError(t, err)
	right, err := url.Parse("HTTPS://CONTACTS.example:443/book")
	require.NoError(t, err)

	assert.True(t, sameCollectionURL(left, right))
}

func TestDAVWritePrivilegeDoesNotGrantCollectionMembershipChanges(t *testing.T) {
	assert := assert.New(t)

	capabilities := capabilitiesFrom(Properties{
		PrivilegesPresent: true,
		Privileges:        []string{"write"},
	})

	assert.False(capabilities.Create)
	assert.True(capabilities.Update)
	assert.False(capabilities.Delete)
	assert.True(capabilities.CreateKnown)
	assert.True(capabilities.UpdateKnown)
	assert.True(capabilities.DeleteKnown)
}

func TestDiscoverUsesDirectPrincipalAndEnumeratesCapabilities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path+" depth="+r.Header.Get("Depth"))
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/dav/principals/alice/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/dav/principals/alice/":
			writeMultiStatus(t, w, `<D:response><D:href>/dav/principals/alice/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/dav/books/alice/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/dav/books/alice/":
			writeMultiStatus(t, w, `<D:response><D:href>/dav/books/alice/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/dav/books/alice/personal/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
				<D:displayname>Personal</D:displayname>
				<D:supported-report-set>
					<D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report>
					<D:supported-report><D:report><C:addressbook-multiget/></D:report></D:supported-report>
				</D:supported-report-set>
				<D:current-user-privilege-set>
					<D:privilege><D:bind/></D:privilege>
					<D:privilege><D:write-content/></D:privilege>
				</D:current-user-privilege-set>
				<C:supported-address-data>
					<C:address-data-type content-type="text/vcard" version="4.0"/>
					<C:address-data-type content-type="text/vcard" version="3.0"/>
					<C:address-data-type content-type="application/json" version="9.9"/>
				</C:supported-address-data>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	require.NoError(err)
	require.Len(discovery.Books, 1)
	assert.Equal("/dav/principals/alice/", discovery.PrincipalURL.Path)
	assert.Equal("/dav/books/alice/", discovery.HomeURL.Path)
	book := discovery.Books[0]
	assert.Equal("/dav/books/alice/personal/", book.URL.Path)
	assert.Equal("Personal", book.DisplayName)
	assert.True(book.SupportsSyncCollection)
	assert.True(book.SupportsMultiget)
	assert.Equal([]string{"4.0", "3.0"}, book.SupportedVCardVersions)
	assert.True(book.Capabilities.CreateKnown)
	assert.True(book.Capabilities.Create)
	assert.True(book.Capabilities.UpdateKnown)
	assert.True(book.Capabilities.Update)
	assert.True(book.Capabilities.DeleteKnown)
	assert.False(book.Capabilities.Delete)
	assert.Equal([]string{
		"PROPFIND /dav depth=0",
		"PROPFIND /dav/principals/alice/ depth=0",
		"PROPFIND /dav/books/alice/ depth=1",
	}, requests)
}

func TestDiscoverEnumeratesAllHomeCollectionsAndDeduplicatesBooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set>
					<D:href>/books/</D:href><D:href>/books/team/</D:href><D:href>/books/</D:href>
				</C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/team/shared/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Shared</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/team/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/team/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/team/shared/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Shared</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/team/local/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Team</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	require.NoError(err)
	require.Len(discovery.HomeURLs, 2)
	assert.Equal([]string{"/books/", "/books/team/"}, []string{discovery.HomeURLs[0].Path, discovery.HomeURLs[1].Path})
	assert.Equal(discovery.HomeURLs[0], discovery.HomeURL)
	require.Len(discovery.Books, 2)
	assert.Equal("/books/team/shared/", discovery.Books[0].URL.Path)
	assert.Equal(0, discovery.Books[0].DiscoveryIndex)
	assert.Equal("/books/team/local/", discovery.Books[1].URL.Path)
	assert.Equal(1, discovery.Books[1].DiscoveryIndex)
}

func TestDiscoverSharesTransferBudgetAcrossRequests(t *testing.T) {
	require := require.New(t)
	direct := []byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/dav</D:href><D:propstat><D:prop><D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	principal := []byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/principal/</D:href><D:propstat><D:prop><C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusMultiStatus)
		switch r.URL.Path {
		case "/dav":
			_, _ = w.Write(direct)
		case "/principal/":
			_, _ = w.Write(principal)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newFixtureClient(t, server.URL, "alice", "secret")
	client.operationBytes = int64(len(direct) + len(principal) - 1)

	_, err := Discover(t.Context(), client, server.URL+"/dav")
	require.ErrorIs(err, ErrOperationLimit)
	assert.Equal(t, 2, requests, "the second response must exhaust the shared budget")
}

func TestDiscoverSharesDeadlineAcrossRequests(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newFixtureClient(t, server.URL, "alice", "secret")
	client.operationTimeout = 300 * time.Millisecond

	started := time.Now()
	_, err := Discover(t.Context(), client, server.URL+"/dav")
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 500*time.Millisecond, "requests must share one operation deadline")
}

func TestDiscoverFallsBackToWellKnownAndKeepsMissingPrivilegesUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/entered":
			http.NotFound(w, r)
		case "/.well-known/carddav":
			writeMultiStatus(t, w, `<D:response><D:href>/.well-known/carddav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/personal/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Personal</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
			<D:propstat><D:prop><D:current-user-privilege-set><D:privilege><D:bind/></D:privilege>
			</D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat></D:response>
			<D:response><D:href>/books/directory/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Directory</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/entered")
	require.NoError(err)
	require.Len(discovery.Books, 2)
	assert.Equal("/books/personal/", discovery.Books[0].URL.Path)
	assert.False(discovery.Books[0].Capabilities.CreateKnown)
	assert.False(discovery.Books[0].Capabilities.Create)
	assert.Equal("/books/directory/", discovery.Books[1].URL.Path)
}

func TestDiscoverDoesNotMaskDirectTransientOrParsingFailures(t *testing.T) {
	tests := []struct {
		name       string
		direct     func(http.ResponseWriter)
		wantStatus int
	}{
		{
			name: "rate limit with retry after",
			direct: func(w http.ResponseWriter) {
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name: "server failure",
			direct: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "malformed multistatus",
			direct: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = w.Write([]byte(`<D:multistatus xmlns:D="DAV:"><D:response>`))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/entered" {
					tc.direct(w)
					return
				}
				http.Error(w, "fallback must not run", http.StatusTeapot)
			}))
			t.Cleanup(server.Close)

			_, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/entered")
			require.Error(t, err)
			assert.Equal([]string{"/entered"}, paths)
			if tc.wantStatus != 0 {
				var status *StatusError
				require.ErrorAs(t, err, &status)
				assert.Equal(tc.wantStatus, status.StatusCode)
				if tc.wantStatus == http.StatusTooManyRequests {
					assert.Equal(90*time.Second, status.RetryAfter)
				}
			}
		})
	}
}

func TestDiscoverDoesNotRetryNetworkFailureThroughWellKnown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	origin, err := url.Parse("https://contacts.example")
	require.NoError(err)
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	var dialCalls atomic.Int32
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin,
		Username:         "alice",
		Password:         "secret",
		Resolver:         resolver,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("synthetic network failure")
		},
	})
	require.NoError(err)

	_, err = Discover(t.Context(), client, origin.String()+"/entered")
	require.ErrorContains(err, "synthetic network failure")
	assert.Equal(int32(1), dialCalls.Load(), "network failure must not trigger well-known discovery")
}

func TestDiscoverSkipsNonHomeResponseWithoutSuccessfulResourceType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/inaccessible/</D:href><D:propstat><D:prop>
				<D:resourcetype/>
			</D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat></D:response>
			<D:response><D:href>/books/personal/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
				<D:displayname>Personal</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	require.NoError(t, err)
	require.Len(t, discovery.Books, 1)
	assert.Equal(t, "/books/personal/", discovery.Books[0].URL.Path)
}

func TestDiscoverSkipsUnavailableNonHomeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>/books/inaccessible/</D:href><D:status>HTTP/1.1 403 Forbidden</D:status></D:response>
			<D:response><D:href>/books/personal/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
				<D:displayname>Personal</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	require.NoError(t, err)
	require.Len(t, discovery.Books, 1)
	assert.Equal(t, "/books/personal/", discovery.Books[0].URL.Path)
}

func TestDiscoverRejectsUnavailableHomeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			writeMultiStatus(t, w, `<D:response><D:href>/books/</D:href><D:status>HTTP/1.1 403 Forbidden</D:status></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	var status *StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusForbidden, status.StatusCode)
}

func TestDiscoverPreservesRedirectedBookAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav":
			writeMultiStatus(t, w, `<D:response><D:href>/dav</D:href><D:propstat><D:prop>
				<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/principal/":
			writeMultiStatus(t, w, `<D:response><D:href>/principal/</D:href><D:propstat><D:prop>
				<C:addressbook-home-set><D:href>/books/</D:href></C:addressbook-home-set>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		case "/books/":
			http.Redirect(w, r, "/canonical/books/", http.StatusTemporaryRedirect)
		case "/canonical/books/":
			writeMultiStatus(t, w, `<D:response><D:href>./</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/></D:resourcetype>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			<D:response><D:href>personal/</D:href><D:propstat><D:prop>
				<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>
				<D:displayname>Personal</D:displayname>
			</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	discovery, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/dav")
	require.NoError(err)
	require.Len(discovery.Books, 1)
	assert.Equal("/canonical/books/personal/", discovery.Books[0].URL.Path)
	require.NotNil(discovery.Books[0].DiscoveryAliasURL)
	assert.Equal("/books/personal/", discovery.Books[0].DiscoveryAliasURL.Path)
}

func TestDiscoverRejectsCrossOriginDiscoveryResponseHrefWithoutFallback(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeMultiStatus(t, w, `<D:response><D:href>https://elsewhere.example/dav</D:href><D:propstat><D:prop>
			<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>
		</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}))
	t.Cleanup(server.Close)

	_, err := Discover(t.Context(), newFixtureClient(t, server.URL, "alice", "secret"), server.URL+"/entered")
	require.ErrorIs(t, err, ErrUnsafeHref)
	assert.Equal(t, 1, requests, "an unsafe direct response must not be retried through well-known discovery")
}

func writeMultiStatus(t *testing.T, w http.ResponseWriter, responses string) {
	t.Helper()
	w.WriteHeader(http.StatusMultiStatus)
	_, err := fmt.Fprintf(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">%s</D:multistatus>`, strings.TrimSpace(responses))
	require.NoError(t, err)
}
