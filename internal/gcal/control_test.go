package gcal

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestEventControlWire(t *testing.T) {
	for _, operation := range []string{"create", "patch", "delete", "move", "freebusy"} {
		t.Run(operation, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "Bearer token", r.Header.Get("Authorization"))
				if operation == "freebusy" {
					assert.Equal(t, "/freeBusy", r.URL.Path)
					assert.Equal(t, http.MethodPost, r.Method)
					var body FreeBusyRequest
					if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !assertions.Len(body.Items, 1) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assertions.Equal("shared@example.com", body.Items[0].ID)
					_, _ = io.WriteString(w, `{"calendars":{"shared@example.com":{"busy":[{"start":"2026-10-02T10:00:00Z","end":"2026-10-02T11:00:00Z"}]}}}`)
					return
				}
				assert.Equal(t, "none", r.URL.Query().Get("sendUpdates"))
				switch operation {
				case "create":
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "/calendars/shared@example.com/events", r.URL.Path)
					var body map[string]any
					if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assert.Equal(t, "Synthetic planning", body["summary"])
					assert.NotContains(t, body, "organizer")
				case "patch":
					assert.Equal(t, http.MethodPatch, r.Method)
					assert.Equal(t, `"v1"`, r.Header.Get("If-Match"))
					var body map[string]any
					if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assert.IsType(t, "", body["summary"])
					assert.Empty(t, body["summary"])
					assert.Equal(t, []any{}, body["attendees"])
					assert.NotContains(t, body, "start")
				case "delete":
					assert.Equal(t, http.MethodDelete, r.Method)
					w.WriteHeader(http.StatusNoContent)
					return
				case "move":
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "destination@example.com", r.URL.Query().Get("destination"))
				}
				_, _ = io.WriteString(w, `{"id":"event1","etag":"\"v2\"","summary":"Synthetic planning","organizer":{"email":"shared@example.com"}}`)
			}))
			defer srv.Close()
			client := NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "token"}), WithBaseURL(srv.URL))
			opts := MutationOptions{IfMatch: `"v1"`}
			title := "Synthetic planning"
			var err error
			switch operation {
			case "create":
				_, err = client.InsertEvent(t.Context(), "shared@example.com", EventInput{Summary: &title}, opts)
			case "patch":
				empty := ""
				attendees := []Attendee{}
				_, err = client.PatchEvent(t.Context(), "shared@example.com", "event1", EventInput{Summary: &empty, Attendees: &attendees}, opts)
			case "delete":
				err = client.DeleteEvent(t.Context(), "shared@example.com", "event1", opts)
			case "move":
				_, err = client.MoveEvent(t.Context(), "shared@example.com", "event1", "destination@example.com", opts)
			case "freebusy":
				var result *FreeBusyResponse
				result, err = client.FreeBusy(t.Context(), FreeBusyRequest{TimeMin: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), TimeMax: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), Items: []FreeBusyItem{{ID: "shared@example.com"}}})
				requirements.NoError(err)
				assertions.Len(result.Calendars["shared@example.com"].Busy, 1)
			}
			requirements.NoError(err)
			assertions.Equal(1, calls)
		})
	}
}

func TestListInstancesUsesSupportedPaginationParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/calendars/team@example.com/events/series/instances", r.URL.Path)
		query := r.URL.Query()
		assertions.Equal("p2", query.Get("pageToken"))
		assertions.Equal("false", query.Get("showDeleted"))
		assertions.Equal("2500", query.Get("maxResults"))
		assertions.NotContains(query, "originalStart")
		assertions.NotContains(query, "original_start")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"instance","recurringEventId":"series","originalStartTime":{"dateTime":"2026-10-03T09:00:00Z"}}],"nextPageToken":"p3"}`)
	}))
	defer srv.Close()

	page, err := testClient(t, srv).ListInstances(t.Context(), "team@example.com", "series", EventsListParams{PageToken: "p2"})
	requirements.NoError(err)
	requirements.Len(page.Items, 1)
	assertions.Equal("instance", page.Items[0].ID)
	assertions.Equal("p3", page.NextPageToken)
}

func TestEventControlDoesNotReplayAmbiguousWrites(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	client := NewClient(nil, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	_, err := client.InsertEvent(context.Background(), "primary", EventInput{}, MutationOptions{})
	requirements.Error(err)
	requirements.ErrorIs(err, ErrOutcomeUnknown)
	assertions.Equal(1, calls)
	_, err = client.InsertEvent(t.Context(), "primary", EventInput{}, MutationOptions{SendUpdates: "invalid"})
	requirements.Error(err)
	assertions.Equal(1, calls)
}

func TestEventControl412ReturnsTypedPreconditionConflict(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv.Close()
	client := NewClient(nil, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))

	_, err := client.PatchEvent(t.Context(), "team@example.com", "event-1", EventInput{}, MutationOptions{})
	requirements.Error(err)
	var conflict *PreconditionFailedError
	requirements.ErrorAs(err, &conflict)
	assertions.Equal("calendar event changed; fetch it again before applying the update", conflict.Error())
}

func TestEventControlCancellationRetainsUnknownOutcome(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	received, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(received)
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := NewClient(nil, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := client.InsertEvent(ctx, "team@example.com", EventInput{}, MutationOptions{})
		done <- err
	}()
	select {
	case <-received:
	case err := <-done:
		requirements.NoError(err, "provider must receive the request before cancellation")
		return
	case <-time.After(30 * time.Second):
		requirements.FailNow("provider did not receive the request")
	}
	cancel()
	err := <-done
	requirements.ErrorIs(err, ErrOutcomeUnknown)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Contains(err.Error(), "inspect the event before retrying")
	assertions.Equal(int64(1), calls.Load())
}
