package caldav

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
func data(href, raw string) string {
	return testutil.DAVPropResponse(href, `<D:getetag>"v1"</D:getetag><C:calendar-data><![CDATA[`+raw+`]]></C:calendar-data>`)
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
		return multi(testutil.DAVPropResponse("/dav", `<D:current-user-principal><D:href>/principal</D:href></D:current-user-principal>`)), true
	case "/principal":
		return multi(testutil.DAVPropResponse("/principal", `<C:calendar-home-set><D:href>/home/</D:href><D:href>/home/</D:href></C:calendar-home-set>`)), true
	case "/home/":
		return multi(testutil.DAVPropResponse("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`) + testutil.DAVPropResponse("/home/cal/", `<D:displayname>Example calendar</D:displayname><D:resourcetype><D:collection/><C:calendar/></D:resourcetype><D:supported-report-set><D:supported-report><D:report><D:sync-collection/></D:report></D:supported-report><D:supported-report><D:report><C:calendar-multiget/></D:report></D:supported-report></D:supported-report-set>`)), true
	}
	return "", false
}
func TestDiscoverAndPull(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
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
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, multi(testutil.DAVPropResponse("/home/cal/a.ics", `<D:getetag>"v1"</D:getetag>`)+`<D:sync-token>t1</D:sync-token>`))
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

func TestDiscoverOnlyEventCalendars(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, ok := discoveryResponse(r)
		if r.URL.Path == "/home/" {
			body = multi(testutil.DAVPropResponse("/home/", `<D:resourcetype><D:collection/></D:resourcetype>`))
			var entries strings.Builder
			for _, entry := range []struct{ name, components string }{
				{"events", `<C:supported-calendar-component-set><C:comp name="VEVENT"/><C:comp name="VTODO"/></C:supported-calendar-component-set>`},
				{"tasks", `<C:supported-calendar-component-set><C:comp name="VTODO"/></C:supported-calendar-component-set>`},
				{"unspecified", ""},
			} {
				entries.WriteString(testutil.DAVPropResponse("/home/"+entry.name+"/", `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`+entry.components))
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
				_, _ = io.WriteString(w, multi(testutil.DAVPropResponse("/principal/", "")))
			} else {
				_, _ = io.WriteString(w, multi(testutil.DAVPropResponse("/dav", `<D:current-user-principal><D:href>/principal/</D:href></D:current-user-principal>`)))
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
