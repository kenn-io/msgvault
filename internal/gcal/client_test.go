package gcal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/gmail"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})
	return NewClient(ts,
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		// A roomy limiter so multi-call tests don't block on the bucket.
		WithRateLimiter(gmail.NewRateLimiterWithCapacity(100, 100)),
	)
}

func TestClient_ListCalendars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/users/me/calendarList", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items": [
				{"id":"primary","summary":"Personal","timeZone":"America/Los_Angeles","accessRole":"owner","primary":true},
				{"id":"team@group.calendar.google.com","summary":"Team","summaryOverride":"My Team","accessRole":"writer"},
				{"id":"holidays","summary":"Holidays","accessRole":"reader"}
			]
		}`))
	}))
	defer srv.Close()

	assert := assert.New(t)
	require := require.New(t)
	page, err := testClient(t, srv).ListCalendars(context.Background(), "")
	require.NoError(err)
	require.Len(page.Items, 3)
	assert.Equal("primary", page.Items[0].ID)
	assert.True(page.Items[0].Primary)
	assert.Equal("owner", page.Items[0].AccessRole)
	// summaryOverride wins over summary.
	assert.Equal("My Team", page.Items[1].Summary)
	assert.Equal("reader", page.Items[2].AccessRole)
}

func TestClient_ListEvents_PaginationAndSyncToken(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/calendars/primary/events", r.URL.Path)
		// Full sync: no syncToken on the first page.
		q := r.URL.Query()
		assert.Equal(t, "false", q.Get("singleEvents"))
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("pageToken") {
		case "":
			// First page: a timed event + an all-day event, with a nextPageToken
			// and NO syncToken (sync token must only appear on the final page).
			_, _ = w.Write([]byte(`{
				"items": [
					{"id":"e1","status":"confirmed","summary":"Standup","location":"Room 1",
					 "organizer":{"email":"alice@example.com","displayName":"Alice","self":true},
					 "start":{"dateTime":"2024-05-01T09:00:00-07:00","timeZone":"America/Los_Angeles"},
					 "end":{"dateTime":"2024-05-01T09:30:00-07:00"},
					 "attendees":[{"email":"bob@example.com","displayName":"Bob","responseStatus":"accepted"}]},
					{"id":"e2","status":"confirmed","summary":"Vacation",
					 "start":{"date":"2024-06-10"},"end":{"date":"2024-06-15"}}
				],
				"nextPageToken":"p2"
			}`))
		case "p2":
			// Final page: a recurring master + a cancelled instance, with the
			// terminal nextSyncToken.
			_, _ = w.Write([]byte(`{
				"items": [
					{"id":"r1","status":"confirmed","summary":"Weekly sync",
					 "start":{"dateTime":"2024-05-02T10:00:00Z"},"end":{"dateTime":"2024-05-02T10:30:00Z"},
					 "recurrence":["RRULE:FREQ=WEEKLY;BYDAY=TH"]},
					{"id":"r1_20240509T100000Z","status":"cancelled","recurringEventId":"r1",
					 "originalStartTime":{"dateTime":"2024-05-09T10:00:00Z"}}
				],
				"nextSyncToken":"SYNC_TOKEN_FINAL"
			}`))
		default:
			assert.Failf(t, "unexpected pageToken", "%q", q.Get("pageToken"))
		}
	}))
	defer srv.Close()

	c := testClient(t, srv)
	ctx := context.Background()

	assert := assert.New(t)
	require := require.New(t)

	p1, err := c.ListEvents(ctx, "primary", EventsListParams{SingleEvents: false, ShowDeleted: true, MaxResults: 2500})
	require.NoError(err)
	require.Len(p1.Items, 2)
	assert.Empty(p1.NextSyncToken, "sync token must not appear before the final page")
	assert.Equal("p2", p1.NextPageToken)

	// Timed event mapping.
	e1 := p1.Items[0]
	assert.Equal("Standup", e1.Summary)
	assert.Equal("alice@example.com", e1.Organizer.Email)
	assert.True(e1.Organizer.Self)
	require.Len(e1.Attendees, 1)
	assert.Equal("bob@example.com", e1.Attendees[0].Email)
	inst, ok := e1.Start.Instant()
	require.True(ok)
	assert.Equal(time.Date(2024, 5, 1, 16, 0, 0, 0, time.UTC), inst.UTC())
	assert.False(e1.Start.IsAllDay())

	// All-day event mapping.
	e2 := p1.Items[1]
	assert.True(e2.Start.IsAllDay())
	assert.Equal("2024-06-10", e2.Start.Date)
	allDay, ok := e2.Start.Instant()
	require.True(ok)
	assert.Equal(time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC), allDay)

	// Final page carries the sync token.
	p2, err := c.ListEvents(ctx, "primary", EventsListParams{
		SingleEvents: false, ShowDeleted: true, MaxResults: 2500, PageToken: p1.NextPageToken,
	})
	require.NoError(err)
	require.Len(p2.Items, 2)
	assert.Equal("SYNC_TOKEN_FINAL", p2.NextSyncToken)
	assert.Empty(p2.NextPageToken)

	// Recurring master + cancelled instance mapping.
	master := p2.Items[0]
	require.Len(master.Recurrence, 1)
	assert.Equal("RRULE:FREQ=WEEKLY;BYDAY=TH", master.Recurrence[0])
	cancelled := p2.Items[1]
	assert.True(cancelled.IsCancelled())
	assert.Equal("r1", cancelled.RecurringEventID)
	assert.Equal(time.Date(2024, 5, 9, 10, 0, 0, 0, time.UTC), cancelled.OriginalStartTime.DateTime.UTC())

	assert.Equal(int32(2), calls.Load())
}

func TestClient_ListEvents_IncrementalParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Incremental: syncToken present, timeMin/timeMax MUST NOT be sent.
		assert.Equal(t, "PRIOR_TOKEN", q.Get("syncToken"))
		assert.Empty(t, q.Get("timeMin"), "timeMin must not accompany syncToken")
		assert.Empty(t, q.Get("timeMax"), "timeMax must not accompany syncToken")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"nextSyncToken":"NEW_TOKEN"}`))
	}))
	defer srv.Close()

	page, err := testClient(t, srv).ListEvents(context.Background(), "primary", EventsListParams{
		SyncToken: "PRIOR_TOKEN", SingleEvents: false, ShowDeleted: true,
		// These must be dropped because SyncToken is set.
		TimeMin: "2024-01-01T00:00:00Z", TimeMax: "2024-12-31T00:00:00Z",
	})
	require.NoError(t, err)
	assert.Equal(t, "NEW_TOKEN", page.NextSyncToken)
}

func TestClient_ListEvents_Gone410(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":410,"message":"Sync token is no longer valid"}}`))
	}))
	defer srv.Close()

	_, err := testClient(t, srv).ListEvents(context.Background(), "primary", EventsListParams{SyncToken: "STALE"})
	var gone *GoneError
	require.ErrorAs(t, err, &gone, "expected *GoneError, got %v", err)
}

func TestClient_GetEvent_NotFound404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Not Found"}}`))
	}))
	defer srv.Close()

	_, err := testClient(t, srv).GetEvent(context.Background(), "primary", "missing")
	var nf *NotFoundError
	require.ErrorAs(t, err, &nf, "expected *NotFoundError, got %v", err)
}

func TestClient_Retry429ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"nextSyncToken":"OK"}`))
	}))
	defer srv.Close()

	page, err := testClient(t, srv).ListEvents(context.Background(), "primary", EventsListParams{})
	require.NoError(t, err)
	assert.Equal(t, "OK", page.NextSyncToken)
	assert.GreaterOrEqual(t, calls.Load(), int32(2), "should have retried")
}

func TestClient_QuotaForbiddenRetries_PermissionForbiddenTerminal(t *testing.T) {
	t.Run("quota 403 retries", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded","domain":"usageLimits"}]}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"nextSyncToken":"OK"}`))
		}))
		defer srv.Close()

		_, err := testClient(t, srv).ListEvents(context.Background(), "primary", EventsListParams{})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, calls.Load(), int32(2))
	})

	t.Run("permission 403 is terminal", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"errors":[{"reason":"insufficientPermissions"}]}}`))
		}))
		defer srv.Close()

		_, err := testClient(t, srv).ListEvents(context.Background(), "primary", EventsListParams{})
		require.Error(t, err)
		assert.Equal(t, int32(1), calls.Load(), "permission error must not retry")
	})
}

func TestClient_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := testClient(t, srv).ListCalendars(ctx, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
