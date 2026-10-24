package caldav

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/testutil"
)

const calendarBody = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:example@example.org\r\nDTSTART:20270314T130000Z\r\nDURATION:PT1H\r\nSUMMARY:Example meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func multi(inner string) string {
	return testutil.DAVMultiStatus(inner, "urn:ietf:params:xml:ns:caldav")
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
	credential := t.Name()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	local, err := url.Parse(server.URL)
	requirements.NoError(err)
	origin, err := url.Parse("http://203.0.113.1/dav")
	requirements.NoError(err)
	c, err := NewClient(carddav.ClientOptions{CredentialOrigin: origin, Username: "example", Password: credential, AllowInsecureCredentials: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
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
		assertions.Equal(t.Name(), password)
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
	calendars, err := c.discover(t.Context(), "http://203.0.113.1/dav", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	requirements.Len(calendars, 1)
	assertions.Equal("Example calendar", calendars[0].DisplayName)
	assertions.True(calendars[0].SupportsSyncCollection)
	assertions.True(calendars[0].SupportsMultiget)
	p, err := c.pull(t.Context(), calendars[0], "", carddav.NewBudget(1<<20))
	requirements.NoError(err)
	assertions.True(p.ReplaceAll)
	assertions.Equal("t1", p.NextSyncToken)
	requirements.Len(p.Members, 1)
	assertions.Contains(string(p.Members[0].Body), "UID:example@example.org")
}

func TestDiscoverSkipsUnavailableUnselectedCollection(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	var unselectedReads atomic.Int32
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/home/" {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(prop("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`)+prop("/home/cal/", `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`)+prop("/home/unavailable/", `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`)))
			return
		}
		if body, ok := discoveryResponse(r); ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
			return
		}
		if r.URL.Path == "/home/cal/" {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(prop("/home/cal/", `<D:resourcetype><D:collection/></D:resourcetype>`)))
			return
		}
		if r.URL.Path == "/home/unavailable/" {
			unselectedReads.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
	})
	st := testutil.NewTestStore(t)
	opts := syncOpts()
	opts.Calendars = []string{"http://203.0.113.1/home/cal/"}
	result, err := c.Sync(t.Context(), st, opts)
	requirements.NoError(err)
	assertions.Equal(1, result.Calendars)
	assertions.Zero(unselectedReads.Load())
	opts.Calendars = []string{"http://203.0.113.1/home/absent/"}
	_, err = c.Sync(t.Context(), st, opts)
	assertions.ErrorContains(err, "not discovered")
}

func TestDiscoverOnlyEventCalendars(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, ok := discoveryResponse(r)
		if r.URL.Path == "/home/" {
			body = multi(prop("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`))
			var entries strings.Builder
			for _, entry := range []struct{ name, components string }{
				{"events", `<C:supported-calendar-component-set><C:comp name="VEVENT"/><C:comp name="VTODO"/></C:supported-calendar-component-set>`},
				{"tasks", `<C:supported-calendar-component-set><C:comp name="VTODO"/></C:supported-calendar-component-set>`},
				{"unspecified", ""},
			} {
				entries.WriteString(prop("/home/"+entry.name+"/", `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`+entry.components))
			}
			body = strings.Replace(body, "</D:multistatus>", entries.String()+"</D:multistatus>", 1)
		}
		if ok {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, body)
		}
	})
	books, err := c.discover(t.Context(), "http://203.0.113.1/dav", carddav.NewBudget(1<<20))
	require.NoError(t, err)
	var paths []string
	for _, book := range books {
		paths = append(paths, book.URL.Path)
	}
	assert.Equal(t, []string{"/home/events/", "/home/unspecified/"}, paths)
}

func TestCalendarDAVErrorMessages(t *testing.T) {
	t.Run("malformed XML", func(t *testing.T) {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, `<D:multistatus xmlns:D="DAV:"><broken`)
		})
		u, err := url.Parse("http://203.0.113.1/home/cal/")
		require.NoError(t, err)
		_, err = c.pull(t.Context(), carddav.DiscoveredBook{URL: u, SupportsSyncCollection: true}, "t1", carddav.NewBudget(1<<20))
		assert.ErrorContains(t, err, "CalDAV XML")
	})
	t.Run("discovery", func(t *testing.T) {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMultiStatus)
			if strings.Contains(r.URL.Path, "principal") {
				_, _ = io.WriteString(w, multi(prop("/principal/", "")))
			} else {
				_, _ = io.WriteString(w, multi(prop("/dav", `<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>`)))
			}
		})
		_, err := c.discover(t.Context(), "http://203.0.113.1/dav", carddav.NewBudget(1<<20))
		require.ErrorContains(t, err, "CalDAV discovery did not return calendar-home-set")
	})
	t.Run("sync", func(t *testing.T) {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(""))
		})
		u, err := url.Parse("http://203.0.113.1/home/cal/")
		require.NoError(t, err)
		_, err = c.pull(t.Context(), carddav.DiscoveredBook{URL: u, SupportsSyncCollection: true}, "t1", carddav.NewBudget(1<<20))
		require.ErrorContains(t, err, "CalDAV sync response lacks a usable sync token")
	})
}
