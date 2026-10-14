package caldav

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const calendarBody = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nDTSTART:20270314T130000Z\r\nDURATION:PT1H\r\nSUMMARY:Example meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func multi(inner string) string {
	return `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">` + inner + `</D:multistatus>`
}
func prop(href, props string) string {
	return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` + props + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}
func data(href, raw string) string {
	return prop(href, `<D:getetag>"v1"</D:getetag><C:calendar-data><![CDATA[`+raw+`]]></C:calendar-data>`)
}

// Route an allowed documentation IP to a real local HTTP server. This changes
// dialing only; DAV auth, redirects, HTTP and XML parsing remain production.
func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	requirements := require.New(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	local, err := url.Parse(server.URL)
	requirements.NoError(err)
	origin, err := url.Parse("http://203.0.113.1/dav")
	requirements.NoError(err)
	c, err := NewClient(carddav.ClientOptions{CredentialOrigin: origin, Username: "example", Password: "synthetic", AllowInsecureCredentials: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, local.Host)
	}})
	requirements.NoError(err)
	return c
}
func discoveryResponse(r *http.Request) (string, bool) {
	if r.Method != "PROPFIND" {
		return "", false
	}
	switch r.URL.Path {
	case "/dav":
		return multi(prop("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`)), true
	case "/principal":
		return multi(prop("/principal", `<C:calendar-home-set><D:href>/home/</D:href><D:href>/home/</D:href></C:calendar-home-set>`)), true
	case "/home/":
		return multi(prop("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`) + prop("/home/cal/", `<D:displayname>Example calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report><D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`)), true
	}
	return "", false
}
func TestDiscoverAndPull(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		assertions.True(ok)
		assertions.Equal("example", user)
		assertions.Equal("synthetic", password)
		if body, ok := discoveryResponse(r); ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if !assertions.NoError(err) {
			http.Error(w, "fixture read failed", http.StatusBadRequest)
			return
		}
		if strings.Contains(string(raw), "sync-collection") {
			if r.Header.Get("Depth") != "0" {
				http.Error(w, "sync-collection requires Depth: 0", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(prop("/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)+`<D:sync-token>t1</D:sync-token>`))
			return
		}
		assertions.Contains(string(raw), "calendar-multiget")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, multi(data("/home/cal/a.ics", calendarBody)))
	})
	calendars, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
	requirements.NoError(err)
	requirements.Len(calendars, 1)
	assertions.Equal("Example calendar", calendars[0].DisplayName)
	p, err := c.pull(t.Context(), calendars[0], "", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	assertions.True(p.Full)
	assertions.Equal("t1", p.Token)
	requirements.Len(p.Resources, 1)
	assertions.Contains(string(p.Resources["http://203.0.113.1/home/cal/a.ics"].Raw), "UID:example@example.org")
}

func TestDiscoverAndSnapshotAcceptCollectionHrefsWithoutTrailingSlash(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond := func(body string) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
		}
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == "/dav":
			respond(multi(prop("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`)))
		case r.Method == "PROPFIND" && r.URL.Path == "/principal":
			respond(multi(prop("/principal", `<C:calendar-home-set><D:href>/home/</D:href></C:calendar-home-set>`)))
		case r.Method == "PROPFIND" && r.URL.Path == "/home/":
			respond(multi(prop("/home", `<D:resourcetype><D:collection/></D:resourcetype>`) + prop("/home/cal/", `<D:displayname>Example calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`)))
		case r.Method == "PROPFIND" && r.URL.Path == "/home/cal/":
			respond(multi(prop("/home/cal", `<D:resourcetype><D:collection/></D:resourcetype>`) + prop("/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)))
		case r.Method == http.MethodGet && r.URL.Path == "/home/cal/a.ics":
			_, _ = io.WriteString(w, calendarBody)
		default:
			http.NotFound(w, r)
		}
	})

	calendars, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
	requirements.NoError(err)
	requirements.Len(calendars, 1)
	assertions.Equal("http://203.0.113.1/home/cal/", calendars[0].URL.String())

	p, err := c.pull(t.Context(), calendars[0], "", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	assertions.True(p.Full)
	requirements.Len(p.Resources, 1)
	assertions.Contains(string(p.Resources["http://203.0.113.1/home/cal/a.ics"].Raw), "UID:example@example.org")
}

func TestPrincipalReturnsErrorWhenHrefIsMissing(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, multi(prop("/dav", `<D:displayname>Example</D:displayname>`)))
	})
	target, err := url.Parse("http://203.0.113.1/dav")
	require.NoError(t, err)

	principal, err := c.principal(t.Context(), target, carddav.NewBudget(1<<20))
	require.ErrorContains(t, err, "CalDAV current-user-principal is missing")
	assert.Nil(t, principal)
}

func TestDiscoverRejectsFailedCalendarChildResponse(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/dav":
			body = multi(prop("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`))
		case "/principal":
			body = multi(prop("/principal", `<C:calendar-home-set><D:href>/home/</D:href></C:calendar-home-set>`))
		case "/home/":
			body = multi(
				prop("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`) +
					prop("/home/cal/", `<D:displayname>Available calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`) +
					`<D:response><D:href>/home/broken/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`,
			)
		default:
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, body)
	})
	_, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
	require.ErrorContains(t, err, "CalDAV calendar discovery response failed with status 507")
}

func TestDiscoverAndPullResolveRelativeHrefsAgainstEffectiveURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		respond := func(body string) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
		}
		redirect := func(path string) {
			w.Header().Set("Location", path)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
		switch r.URL.Path {
		case "/dav":
			respond(multi(prop("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`)))
		case "/principal":
			respond(multi(prop("/principal", `<C:calendar-home-set><D:href>/home/</D:href></C:calendar-home-set>`)))
		case "/home/":
			redirect("/canonical/home/")
		case "/canonical/home/":
			respond(multi(prop("/canonical/home/", `<D:resourcetype><D:collection/></D:resourcetype>`) + prop("cal/", `<D:displayname>Example calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report><D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`)))
		case "/home/cal/", "/canonical/home/cal/":
			redirect("/canonical/final/cal/")
		case "/canonical/final/cal/":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "unable to read request body", http.StatusBadRequest)
				return
			}
			if strings.Contains(string(body), "sync-collection") {
				respond(multi(prop("a.ics", `<D:getetag>"v1"</D:getetag>`) + `<D:sync-token>t1</D:sync-token>`))
				return
			}
			respond(multi(data("a.ics", calendarBody)))
		default:
			http.NotFound(w, r)
		}
	})

	calendars, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
	require.NoError(err)
	require.Len(calendars, 1)
	assert.Equal("http://203.0.113.1/canonical/home/cal/", calendars[0].URL.String())

	p, err := c.pull(t.Context(), calendars[0], "", carddav.NewBudget(1<<20))
	require.NoError(err)
	require.Contains(p.Resources, "http://203.0.113.1/canonical/final/cal/a.ics")
	assert.Contains(string(p.Resources["http://203.0.113.1/canonical/final/cal/a.ics"].Raw), "UID:example@example.org")
}

func TestDiscoverRequiresSuccessfulHomeCollectionResponse(t *testing.T) {
	calendar := prop("/home/cal/", `<D:displayname>Example calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`)
	for _, tc := range []struct{ name, homeResponse string }{
		{"failed", multi(`<D:response><D:href>/home/</D:href><D:status>HTTP/1.1 403 Forbidden</D:status></D:response>`)},
		{"omitted with calendar", multi(calendar)},
		{"omitted empty", multi("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body string
				switch r.URL.Path {
				case "/dav":
					body = multi(prop("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`))
				case "/principal":
					body = multi(prop("/principal", `<C:calendar-home-set><D:href>/home/</D:href></C:calendar-home-set>`))
				case "/home/":
					body = tc.homeResponse
				default:
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusMultiStatus)
				_, _ = io.WriteString(w, body)
			})
			_, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
			require.ErrorContains(t, err, "home collection response")
		})
	}
}

func TestMultigetMustBeComplete(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"omitted", multi("")}, {"duplicate", multi(data("/home/cal/a.ics", calendarBody) + data("/home/cal/a.ics", calendarBody))},
		{"unexpected", multi(data("/home/cal/other.ics", calendarBody))},
		{"wrong namespace", strings.ReplaceAll(multi(data("/home/cal/a.ics", calendarBody)), "urn:ietf:params:xml:ns:caldav", "urn:example:wrong")},
		{"partial", multi(`<D:response><D:href>/home/cal/a.ics</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				w.WriteHeader(http.StatusMultiStatus)
				if strings.Contains(string(raw), "sync-collection") {
					_, _ = io.WriteString(w, multi(prop("/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)+`<D:sync-token>t1</D:sync-token>`))
				} else {
					_, _ = io.WriteString(w, tc.reply)
				}
			})
			u, err := url.Parse("http://203.0.113.1/home/cal/")
			requirements.NoError(err)
			_, err = c.pull(t.Context(), Calendar{URL: u, SupportsSync: true, SupportsMultiget: true}, "", carddav.NewBudget(1<<20))
			requirements.Error(err)
		})
	}
}
func TestPullFallbackAndBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			w.WriteHeader(http.StatusMethodNotAllowed)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(prop("/home/cal/", `<D:resourcetype><D:collection/></D:resourcetype>`)+prop("/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)))
		case http.MethodGet:
			_, _ = io.WriteString(w, calendarBody)
		default:
			assertions.Fail("unexpected method", r.Method)
		}
	})
	u, err := url.Parse("http://203.0.113.1/home/cal/")
	requirements.NoError(err)
	p, err := c.pull(t.Context(), Calendar{URL: u, SupportsSync: true, SupportsMultiget: true}, "old", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	assertions.True(p.Full)
	assertions.Empty(p.Token)
	assertions.Equal(calendarBody, string(p.Resources[u.String()+"a.ics"].Raw))
	_, err = c.pull(t.Context(), Calendar{URL: u}, "", carddav.NewBudget(10))
	requirements.ErrorIs(err, carddav.ErrOperationLimit)
}
func TestDiscoveryRejectsExternalHref(t *testing.T) {
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, multi(prop("/dav", `<D:current-user-principal><D:href>http://elsewhere.example.org/principal</D:href></D:current-user-principal>`)))
	})
	_, err := c.Discover(t.Context(), "http://203.0.113.1/dav")
	requirements.Error(err)
}

func TestSyncReportRejectsCollectionPartialStatus(t *testing.T) {
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, multi(`<D:response><D:href>/home/cal/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response><D:sync-token>partial</D:sync-token>`))
	})
	u, err := url.Parse("http://203.0.113.1/home/cal/")
	requirements.NoError(err)
	_, err = c.pull(t.Context(), Calendar{URL: u, SupportsSync: true}, "old", carddav.NewBudget(1<<20))
	requirements.Error(err)
}

func TestCalDAVMultigetCanonicalHref(t *testing.T) {
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(string(raw), "sync-collection") {
			_, _ = io.WriteString(w, multi(prop("http://203.0.113.1:80/home/cal/%61.ics", `<D:getetag>"v1"</D:getetag>`)+`<D:sync-token>t1</D:sync-token>`))
		} else {
			_, _ = io.WriteString(w, multi(data("/home/cal/a.ics", calendarBody)))
		}
	})
	u, err := url.Parse("http://203.0.113.1/home/cal/")
	requirements.NoError(err)
	p, err := c.pull(t.Context(), Calendar{URL: u, SupportsSync: true, SupportsMultiget: true}, "", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	requirements.Contains(p.Resources, "http://203.0.113.1/home/cal/a.ics")
}
