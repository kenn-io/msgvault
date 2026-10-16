package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func retireCalendarControlSourceForTest(t *testing.T, st *store.Store, account, calendarID string) {
	t.Helper()
	retired, err := st.GetOrCreateSource(gcal.SourceType, account+"/"+calendarID)
	require.NoError(t, err)
	syncConfig, err := json.Marshal(map[string]string{
		"account_email": account,
		"calendar_id":   calendarID,
	})
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(retired.ID, string(syncConfig)))
	destination, err := st.GetOrCreateSource(gcal.SourceType, account+"/archive@example.test")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: retired.ID,
		IntoSourceID: destination.ID,
	})
	require.NoError(t, err)
}

func TestCalendarControlRejectsRetiredSourcesBeforeProviderMutation(t *testing.T) {
	const account = "person@example.test"
	for _, tc := range []struct {
		name            string
		retiredCalendar string
		action          string
		destination     string
	}{
		{name: "retired source", retiredCalendar: "team@example.test", action: "update"},
		{name: "retired move destination", retiredCalendar: "other@example.test", action: "move", destination: "other@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			retireCalendarControlSourceForTest(t, st, account, tc.retiredCalendar)

			var providerMutations atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/users/me/calendarList":
					assertions.NoError(json.MarshalWrite(w, gcal.CalendarListPage{Items: []gcal.Calendar{
						{ID: "team@example.test", AccessRole: "owner", TimeZone: "UTC"},
						{ID: "other@example.test", AccessRole: "owner", TimeZone: "UTC"},
					}}))
				case r.Method == http.MethodGet && r.URL.Path == "/calendars/team@example.test/events/event":
					assertions.NoError(json.MarshalWrite(w, gcal.Event{
						ID: "event", ETag: `"v1"`, Summary: "Planning",
						Start: gcal.EventDateTime{DateTime: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)},
						End:   gcal.EventDateTime{DateTime: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)},
					}))
				case r.Method == http.MethodPatch || r.Method == http.MethodDelete || r.Method == http.MethodPost:
					providerMutations.Add(1)
					if r.Method == http.MethodDelete {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					assertions.NoError(json.MarshalWrite(w, gcal.Event{ID: "event", ETag: `"v2"`, Summary: "Updated"}))
				default:
					assertions.Fail("unexpected calendar provider request", r.Method+" "+r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(provider.Close)

			cfg := &config.Config{GCal: []config.GCalSource{{
				Email: account, Enabled: true,
				WriteCalendars: []string{"team@example.test", "other@example.test"},
			}}}
			adapter := &storeAPIAdapter{
				store:  st,
				config: cfg,
				calendarClientFactory: func(context.Context, config.GCalSource, bool) (gcal.ControlAPI, error) {
					return gcal.NewClient(
						oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-calendar-token"}),
						gcal.WithBaseURL(provider.URL),
					), nil
				},
			}
			request := calcontrol.Request{
				Action: tc.action, Account: account, CalendarID: "team@example.test",
				EventID: "event", Destination: tc.destination,
			}
			if tc.action == "update" {
				summary := "Updated planning"
				request.Event.Summary = &summary
			}
			acquired, released := false, false

			result, err := adapter.ControlCalendar(t.Context(), request, nil, func(context.Context) (func(), error) {
				acquired = true
				return func() { released = true }, nil
			})

			require.ErrorIs(t, err, calcontrol.ErrDenied)
			assertions.Nil(result)
			assertions.True(acquired, "retirement must be checked after acquiring the mutation gate")
			assertions.True(released, "the mutation gate must be released after refusal")
			assertions.Zero(providerMutations.Load(), "a retired calendar must be refused before provider mutation")
		})
	}
}
