package carddav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGoogleDiscoveryDirectCollectionWithBearerTokens(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	var authorization []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		if r.URL.Path == "/.well-known/carddav" {
			w.Header().Set("Location", "/contacts/default/")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		assertions.Equal("PROPFIND", r.Method)
		assertions.Equal("0", r.Header.Get("Depth"))
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/contacts/default/</D:href><D:propstat><D:prop><D:sync-token>token-1</D:sync-token><D:displayname>Contacts</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	}))
	t.Cleanup(server.Close)
	client := newFixtureClient(t, server.URL, "", "")
	next := 0
	client.bearerToken = func(context.Context) (string, error) {
		next++
		return fmt.Sprintf("synthetic-token-%d", next), nil
	}
	st := testutil.NewTestStore(t)
	service := NewGoogleService(st, client)
	discovery, err := service.DiscoverAndPersist(t.Context(), server.URL+"/.well-known/carddav", "person@example.com")
	required.NoError(err)
	required.Len(discovery.Books, 1)
	assertions.Equal(server.URL+"/contacts/default/", discovery.Books[0].URL.String())
	assertions.Equal([]string{"Bearer synthetic-token-1", "Bearer synthetic-token-2"}, authorization)
	books, err := service.ListBooks(t.Context())
	required.NoError(err)
	required.Len(books, 1)
	assertions.True(books[0].SupportsSyncCollection)
	assertions.True(books[0].SupportsMultiget)
	assertions.Equal([]string{"3.0"}, books[0].SupportedVCardVersions)
	assertions.Equal(new(true), books[0].CanCreate)
	assertions.Equal(new(true), books[0].CanUpdate)
	assertions.Equal(new(true), books[0].CanDelete)
	assertions.True(books[0].IsWriteTarget)
	assertions.True(books[0].IsSubscribed)
}

func TestGoogleDiscoveryRequiresSuccessfulCollectionProperty(t *testing.T) {
	for _, tc := range []struct{ name, href, status string }{
		{"failed property", "/", "404 Not Found"},
		{"contact href", "/person.vcf", "200 OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = fmt.Fprintf(w, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>%s</D:href><D:propstat><D:prop><D:sync-token>token-1</D:sync-token></D:prop><D:status>HTTP/1.1 %s</D:status></D:propstat></D:response></D:multistatus>`, tc.href, tc.status)
			}))
			t.Cleanup(server.Close)
			_, err := discoverGoogle(t.Context(), newFixtureClient(t, server.URL, "", ""), server.URL+"/")
			require.Error(t, err)
		})
	}
}

func TestGoogleDiscoveryHonorsAdvertisedReadOnlyPrivileges(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:sync-token>token-1</D:sync-token><D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	}))
	t.Cleanup(server.Close)
	service := NewGoogleService(testutil.NewTestStore(t), newFixtureClient(t, server.URL, "", ""))
	_, err := service.DiscoverAndPersist(t.Context(), server.URL+"/", "person@example.com")
	required.NoError(err)
	books, err := service.ListBooks(t.Context())
	required.NoError(err)
	required.Len(books, 1)
	assertions.Equal(new(false), books[0].CanCreate)
	assertions.Equal(new(false), books[0].CanUpdate)
	assertions.Equal(new(false), books[0].CanDelete)
	assertions.False(books[0].IsWriteTarget)
	assertions.False(books[0].IsSubscribed)
	assertions.True(books[0].IsLookupSource)
}

func TestGoogleDiscoveryPrincipalCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, privileges string
		writable         bool
	}{
		{name: "privileges omitted", writable: true},
		{name: "read only", privileges: `<D:current-user-privilege-set><D:privilege><D:read/></D:privilege></D:current-user-privilege-set>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
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
					<D:response><D:href>/books/default/</D:href><D:propstat><D:prop>
						<D:resourcetype><D:collection/><C:addressbook/></D:resourcetype>`+tc.privileges+`
					</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			service := NewGoogleService(testutil.NewTestStore(t), newFixtureClient(t, server.URL, "", ""))
			_, err := service.DiscoverAndPersist(t.Context(), server.URL+"/.well-known/carddav", "person@example.com")
			required.NoError(err)
			books, err := service.ListBooks(t.Context())
			required.NoError(err)
			required.Len(books, 1)
			assertions.Equal(new(tc.writable), books[0].CanCreate)
			assertions.Equal(new(tc.writable), books[0].CanUpdate)
			assertions.Equal(new(tc.writable), books[0].CanDelete)
			assertions.Equal(tc.writable, books[0].IsWriteTarget)
			assertions.Equal(tc.writable, books[0].IsSubscribed)
		})
	}
}
