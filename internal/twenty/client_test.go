package twenty

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type twentyTransport func(*http.Request) (*http.Response, error)

func (f twentyTransport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientPacesRequestsWithinTwentyRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		client, err := NewClient("https://api.twenty.com", "example-key")
		require.NoError(err)
		client.http.Transport = twentyTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)), Header: make(http.Header)}, nil
		})
		_, err = client.ListRecordings(t.Context(), "", 1)
		require.NoError(err)
		started := time.Now()
		_, err = client.ListRecordings(t.Context(), "", 1)
		require.NoError(err)
		assert.GreaterOrEqual(time.Since(started), 650*time.Millisecond, "sustained requests must stay below the documented 100 per minute")
	})
}

func TestClientsShareRateLimitForSameOriginAndCredential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		const baseURL = "https://shared-limiter.twenty.example"
		const apiKey = "shared-example-key"
		first, err := NewClient(baseURL, apiKey)
		require.NoError(err)
		second, err := NewClient("https://SHARED-LIMITER.twenty.example:443/", apiKey)
		require.NoError(err)
		var started []time.Time
		transport := twentyTransport(func(*http.Request) (*http.Response, error) {
			started = append(started, time.Now())
			body := `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		first.http.Transport = transport
		second.http.Transport = transport

		_, err = first.ListRecordings(t.Context(), "", 1)
		require.NoError(err)
		_, err = second.ListRecordings(t.Context(), "", 1)
		require.NoError(err)

		require.Len(started, 2)
		assert.GreaterOrEqual(started[1].Sub(started[0]), 650*time.Millisecond, "separate clients must share the per-credential request pace")
	})
}

func TestClientRecordingPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/graphql", r.URL.Path)
		assert.Equal("Bearer example-key", r.Header.Get("Authorization"))
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		assert.Contains(request.Query, "id: AscNullsLast")
		assert.Equal("cursor-1", request.Variables["after"])
		assert.InDelta(2, request.Variables["first"], 1e-9)
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[{"node":{"id":"recording-1","title":"Planning","status":"COMPLETED","transcript":null,"summary":{"markdown":"Summary"},"calendarEventId":null}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-2"}}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	page, err := client.ListRecordings(t.Context(), "cursor-1", 2)
	require.NoError(err)
	require.Len(page.Records, 1)
	assert.Equal("recording-1", page.Records[0].ID)
	assert.Equal("Planning", page.Records[0].Title)
	assert.Empty(page.Records[0].CalendarEventID)
	assert.Contains(string(page.Records[0].Raw), `"markdown":"Summary"`)
	assert.True(page.HasMore)
	assert.Equal("cursor-2", page.NextCursor)
}

func TestClientCalendarReadsEveryParticipantPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		assert.Equal("event-1", request.Variables["id"])
		switch {
		case strings.Contains(request.Query, "calendarEvents("):
			_, _ = fmt.Fprint(w, `{"data":{"calendarEvents":{"edges":[{"node":{"id":"event-1","title":"Planning","startsAt":"2026-09-01T10:00:00Z"}}]}}}`)
		case request.Variables["after"] == nil:
			edges := []any{map[string]any{"node": map[string]any{"id": "person-1", "handle": "organizer@example.com", "displayName": "Organizer", "isOrganizer": true}}}
			for i := 2; i <= 100; i++ {
				edges = append(edges, map[string]any{"node": map[string]any{"id": fmt.Sprintf("person-%d", i), "handle": fmt.Sprintf("attendee%d@example.com", i)}})
			}
			assert.NoError(json.MarshalWrite(w, map[string]any{"data": map[string]any{"calendarEventParticipants": map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "next"}}}}))
		default:
			assert.Equal("next", request.Variables["after"])
			_, _ = fmt.Fprint(w, `{"data":{"calendarEventParticipants":{"edges":[{"node":{"id":"person-2","handle":"attendee@example.com","displayName":"Attendee","isOrganizer":false}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	calendar, err := client.GetCalendar(t.Context(), "event-1")
	require.NoError(err)
	require.Len(calendar.Participants, 101)
	assert.Equal("organizer@example.com", calendar.Participants[0].Handle)
	assert.True(calendar.Participants[0].IsOrganizer)
	assert.Equal("attendee@example.com", calendar.Participants[100].Handle)
}

func TestClientRejectsBrokenAndPartialResponses(t *testing.T) {
	for _, body := range []string{
		`{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}}},"errors":[{"message":"example-key secret response"}]}`,
		`{"data":null}`, `{"data":{}}`, `{"data":{"callRecordings":null}}`,
		`{"data":{"callRecordings":{"edges":null,"pageInfo":{"hasNextPage":false}}}}`,
		`{"data":{"callRecordings":{"edges":[{"node":null}],"pageInfo":{"hasNextPage":false}}}}`,
		`{"data":{"callRecordings":{"edges":[{"node":{"id":""}}],"pageInfo":{"hasNextPage":false}}}}`,
		`{"data":{"callRecordings":{"edges":[],"pageInfo":{}}}}`,
		`{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":true,"endCursor":"current"}}}}`,
		`{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":true}}}}`,
		`invalid example-key secret response`,
	} {
		t.Run(fmt.Sprintf("case-%d", len(body)), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer srv.Close()
			client, err := NewClient(srv.URL, "example-key")
			require.NoError(err)
			_, err = client.ListRecordings(t.Context(), "current", 100)
			require.Error(err)
			assert.NotContains(err.Error(), "example-key")
			assert.NotContains(err.Error(), "secret response")
		})
	}
}

func TestClientProbeChecksAllObjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		for _, collection := range []string{"callRecordings", "calendarEvents", "calendarEventParticipants"} {
			assert.Contains(request.Query, collection)
		}
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}},"calendarEvents":{"edges":[]},"calendarEventParticipants":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	require.NoError(client.Probe(t.Context()))
}

func TestClientRejectsRedirectAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Empty(r.Header.Get("Authorization"))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	_, err = client.ListRecordings(t.Context(), "", 1)
	require.Error(err)
	assert.Zero(hits)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.ListRecordings(ctx, "", 1)
	require.ErrorIs(err, context.Canceled)
}

func TestClientHTTPFailuresAreRedacted(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "example-key private content")
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, "example-key")
			require.NoError(err)
			_, err = client.ListRecordings(t.Context(), "", 1)
			require.ErrorContains(err, strconv.Itoa(status))
			assert.NotContains(err.Error(), "private content")
			assert.NotContains(err.Error(), "example-key")
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, value := range []string{"", "http://twenty.example", "https://user:secret@twenty.example", "https://twenty.example/prefix", "https://twenty.example/?key=secret", "https://twenty.example/#fragment", "file:///tmp/example", "http://localhost.evil.example"} {
		_, err := ValidateBaseURL(value)
		require.Error(err)
		assert.NotContains(err.Error(), "secret")
	}
	for _, value := range []string{"https://workspace.twenty.example/", "http://127.0.0.1:3000", "http://[::1]:3000", "http://localhost:3000"} {
		out, err := ValidateBaseURL(value)
		require.NoError(err)
		assert.Equal(strings.TrimSuffix(value, "/"), out)
	}
}

func TestClientRejectsMismatchedCalendar(t *testing.T) {
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"calendarEvents":{"edges":[{"node":{"id":"another-event"}}]}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	_, err = client.GetCalendar(t.Context(), "event-1")
	require.ErrorContains(err, "mismatch")
}

func TestClientMissingCalendarEventReturnsNoCalendarEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Query string `json:"query"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		assert.Contains(request.Query, "calendarEvents(")
		_, _ = fmt.Fprint(w, `{"data":{"calendarEvents":{"edges":[]}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "missing-calendar-example-key")
	require.NoError(err)

	calendar, err := client.GetCalendar(t.Context(), "deleted-event")
	require.ErrorContains(err, "linked Twenty calendar event is unavailable")
	assert.Nil(calendar)
	assert.Equal(1, requests, "participant lookup requires an existing calendar event")
}

func TestClientBoundsResponseAndPageSize(t *testing.T) {
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat(" ", 1<<20)
		for range 65 {
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	_, err = client.ListRecordings(t.Context(), "", 1)
	require.ErrorContains(err, "exceeds")
	for _, first := range []int{-1, 0, 101} {
		_, err = client.ListRecordings(t.Context(), "", first)
		require.ErrorContains(err, "page size")
	}
}

func TestClientRetriesOversizedRecordingPagesWithoutSkippingCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type pageRequest struct {
		First int    `json:"first"`
		After string `json:"after"`
	}
	requests := make(chan pageRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Variables pageRequest `json:"variables"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		requests <- request.Variables
		if request.Variables.First > 1 {
			chunk := strings.Repeat(" ", 1<<20)
			for range 65 {
				if _, err := fmt.Fprint(w, chunk); err != nil {
					return
				}
			}
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[{"node":{"id":"recording-1","summary":{"markdown":"Long meeting"}}}],"pageInfo":{"hasNextPage":true,"endCursor":"next-cursor"}}}}`)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	page, err := client.ListRecordings(t.Context(), "catalog-cursor", 2)
	require.NoError(err)
	require.Len(page.Records, 1)
	assert.Equal("recording-1", page.Records[0].ID)
	assert.True(page.HasMore)
	assert.Equal("next-cursor", page.NextCursor)
	assert.Equal(pageRequest{First: 2, After: "catalog-cursor"}, <-requests)
	assert.Equal(pageRequest{First: 1, After: "catalog-cursor"}, <-requests)
	assert.Empty(requests)
}
