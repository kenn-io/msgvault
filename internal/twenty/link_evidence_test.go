package twenty

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestTwentySelectsICalUid(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			return
		}
		switch {
		case strings.Contains(request.Query, "query Probe"):
			assert.Contains(request.Query, "iCalUid")
			_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}},"calendarEvents":{"edges":[]},"calendarEventParticipants":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
		case strings.Contains(request.Query, "query Recordings"):
			assert.Contains(request.Query, "iCalUid")
			_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[{"node":{"id":"recording-1","title":"Planning","createdAt":"2026-10-13T10:00:00Z","startedAt":"2026-10-13T10:05:00Z","summary":{"markdown":"Decisions"},"calendarEventId":"event-1","calendarEvent":{"id":"event-1","iCalUid":"planning@example.com","startsAt":"2026-10-13T12:00:00+02:00"}}}],"pageInfo":{"hasNextPage":false}}}}`)
		default:
			_, _ = fmt.Fprint(w, `{"data":{"calendarEventParticipants":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "example-key")
	require.NoError(err)
	client.limiter = rate.NewLimiter(rate.Inf, 1)
	require.NoError(client.Probe(t.Context()))
	page, err := client.ListRecordings(t.Context(), epoch, "", 1)
	require.NoError(err)
	require.Len(page.Records, 1)
	recording := page.Records[0]
	snapshot, eligible, err := archiveSnapshot(1, "recorder@example.com", recording, recording.Calendar)
	require.NoError(err)
	require.True(eligible)
	var metadata map[string]any
	require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
	assert.Equal("planning@example.com", metadata["calendar_ical_uid"])
	assert.Equal("2026-10-13T10:00:00Z", metadata["scheduled_start"])
	assert.Equal("2026-10-13T10:05:00Z", snapshot.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
}

func TestTwentyFallsBackWhenICalUidUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		for _, startWithProbe := range []bool{true, false} {
			t.Run(fmt.Sprintf("status=%d/probe_first=%t", status, startWithProbe), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				attempts, unsupported := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts++
					var request struct {
						Query string `json:"query"`
					}
					if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
						return
					}
					if strings.Contains(request.Query, "iCalUid") {
						unsupported++
						w.WriteHeader(status)
						_, _ = fmt.Fprint(w, `{"errors":[{"message":"Cannot query field \"iCalUid\" on type \"CalendarEvent\".","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`)
						return
					}
					_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}},"calendarEvents":{"edges":[]},"calendarEventParticipants":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
				}))
				defer srv.Close()
				client, err := NewClient(srv.URL, "example-key")
				require.NoError(err)
				client.limiter = rate.NewLimiter(rate.Inf, 1)
				if startWithProbe {
					require.NoError(client.Probe(t.Context()))
				}
				for range 2 {
					_, err := client.ListRecordings(t.Context(), epoch, "", 1)
					require.NoError(err)
				}
				require.NoError(client.Probe(t.Context()))
				assert.Equal(1, unsupported, "unsupported field is tried only once per client")
				wantAttempts := 4
				if startWithProbe {
					wantAttempts++
				}
				assert.Equal(wantAttempts, attempts)
			})
		}
	}
}

func TestTwentyCalendarMetadataOmitsUnknownEvidence(t *testing.T) {
	for _, tt := range []struct {
		name, raw     string
		wantScheduled bool
	}{
		{"null uid", `{"startsAt":"2026-09-01T09:30:00Z","iCalUid":null}`, true},
		{"invalid scheduled start", `{"startsAt":"invalid"}`, false},
		{"no calendar", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			recording := recordingFixture(t, "recording-1", "null")
			var calendar *Calendar
			if tt.raw != "" {
				calendar = &Calendar{Raw: jsontext.Value(tt.raw)}
			}
			snapshot, eligible, err := archiveSnapshot(1, "recorder@example.com", recording, calendar)
			require.NoError(err)
			require.True(eligible)
			var metadata map[string]any
			require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
			assert.NotContains(metadata, "calendar_ical_uid")
			if tt.wantScheduled {
				assert.Equal("2026-09-01T09:30:00Z", metadata["scheduled_start"])
			} else {
				assert.NotContains(metadata, "scheduled_start")
			}
		})
	}
}

func TestTwentyFallbackOnlyOnFieldRejection(t *testing.T) {
	for _, tt := range []struct {
		name            string
		status          int
		body, wantError string
		wantAttempts    int
		probeOnly       bool
	}{
		{"permission error naming field", 200, `{"errors":[{"message":"Cannot query field \"iCalUid\" on type \"CalendarEvent\".","extensions":{"code":"FORBIDDEN"}}]}`, "FORBIDDEN", 1, false},
		{"permission error naming field HTTP 400", 400, `{"errors":[{"message":"Cannot query field \"iCalUid\" on type \"CalendarEvent\".","extensions":{"code":"FORBIDDEN"}}]}`, "HTTP 400", 1, false},
		{"uncoded error naming field", 200, `{"errors":[{"message":"Access denied to iCalUid"}]}`, "Access denied to iCalUid", 1, false},
		{"other field", 200, `{"errors":[{"message":"Cannot query field \"transcript\" on type \"CallRecording\".","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`, "transcript", 1, false},
		{"auth", 401, "Unauthorized", "HTTP 401", 1, true},
		{"HTTP throttling", 429, "Rate limit reached", "HTTP 429", 5, true},
		{"GraphQL throttling", 200, `{"errors":[{"message":"Rate limit reached","extensions":{"code":"RATE_LIMITED"}}]}`, "rate limit reached", 5, false},
	} {
		paths := []bool{true, false}
		if tt.probeOnly {
			// Direct recording reads are covered by TestClientHTTPFailuresAreRedacted.
			paths = []bool{true}
		}
		for _, probe := range paths {
			t.Run(fmt.Sprintf("%s/probe=%t", tt.name, probe), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				attempts, fallbacks := 0, 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts++
					var request struct {
						Query string `json:"query"`
					}
					if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
						return
					}
					if !strings.Contains(request.Query, "iCalUid") {
						fallbacks++
						_, _ = fmt.Fprint(w, `{"data":{"callRecordings":{"edges":[],"pageInfo":{"hasNextPage":false}},"calendarEvents":{"edges":[]},"calendarEventParticipants":{"edges":[],"pageInfo":{"hasNextPage":false}}}}`)
						return
					}
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tt.status)
					_, _ = fmt.Fprint(w, tt.body)
				}))
				defer srv.Close()
				client, err := NewClient(srv.URL, "example-key")
				require.NoError(err)
				client.limiter = rate.NewLimiter(rate.Inf, 1)
				if probe {
					err = client.Probe(t.Context())
				} else {
					_, err = client.ListRecordings(t.Context(), epoch, "", 1)
				}
				require.ErrorContains(err, tt.wantError)
				assert.Equal(tt.wantAttempts, attempts)
				assert.Zero(fallbacks)
			})
		}
	}
}
