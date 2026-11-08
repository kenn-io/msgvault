package notionmeetings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestConnectedHumanRequiresExplicitPATMode(t *testing.T) {
	var self User
	require.NoError(t, json.Unmarshal([]byte(`{"object":"user","id":"bot-1","type":"bot","bot":{"owner":{"type":"user","user":{"id":"human-1"}}}}`), &self))
	for _, tc := range []struct {
		mode, id, want, provenance string
		fails                      bool
	}{
		{"pat", "", "human-1", "pat_owner", false}, {"", "", "", "", true},
		{"", "configured-1", "configured-1", "configured", false}, {"pat", "different", "", "", true},
	} {
		t.Run(tc.mode+tc.id, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			identity, err := ResolveIdentity(&self, tc.mode, tc.id)
			if tc.fails {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(tc.want, identity.UserID)
			assert.Equal(tc.provenance, identity.Provenance)
		})
	}
	_, err := ResolveIdentity(&User{Object: "user", ID: "bot-1", Type: "bot"}, "pat", "")
	require.Error(t, err)
}

func TestDiscoveryClientHTTPContracts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users/me":
			_, _ = io.WriteString(w, `{"object":"user","id":"bot-1","type":"bot"}`)
		case "/v1/blocks/meeting_notes/query":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.NotContains(body, "start_cursor")
			filterJSON, err := json.Marshal(body["filter"])
			if !assert.NoError(err) {
				return
			}
			assert.JSONEq(`{"operator":"and","filters":[{"property":"created_time","filter":{"operator":"date_is_on_or_after","value":{"type":"exact","value":{"type":"datetime","start_date":"2026-11-01","start_time":"00:00","time_zone":"UTC"}}}},{"property":"created_time","filter":{"operator":"date_is_before","value":{"type":"exact","value":{"type":"datetime","start_date":"2026-11-02","start_time":"00:00","time_zone":"UTC"}}}}]}`, string(filterJSON))
			_, _ = io.WriteString(w, `{"results":[],"has_more":false}`)
		case "/v1/search", "/v1/data_sources/data-1/query":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal("cursor-1", body["start_cursor"])
			_, _ = io.WriteString(w, `{"object":"list","results":[{"object":"page","id":"page-1"},{"object":"data_source","id":"wiki-1"}],"has_more":true,"next_cursor":"cursor-2","request_status":{"type":"incomplete","incomplete_reason":"query_result_limit_reached"}}`)
		case "/v1/databases/db-1":
			_, _ = io.WriteString(w, `{"object":"database","id":"db-1","data_sources":[{"id":"data-1"},{"id":"data-2"}]}`)
		default:
			assert.Fail("unexpected path", r.URL.Path)
		}
	}))
	defer server.Close()
	c := NewClient(server.URL, "synthetic")
	self, err := c.RetrieveSelf(t.Context())
	require.NoError(err)
	assert.Equal("bot-1", self.ID)
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	_, err = c.QueryMeetingNotesFiltered(t.Context(), 50, queryWindow{From: from, To: to}.filter())
	require.NoError(err)
	search, err := c.Search(t.Context(), "cursor-1")
	require.NoError(err)
	assert.Equal("cursor-2", search.NextCursor)
	rows, err := c.QueryDataSource(t.Context(), "data-1", "cursor-1")
	require.NoError(err)
	assert.Equal("incomplete", rows.RequestStatus.Type)
	db, err := c.RetrieveDatabase(t.Context(), "db-1")
	require.NoError(err)
	assert.Len(db.DataSources, 2)
}
func TestQueryWindowSplitPartitionsBoundary(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		minutes := 1 + int(hegel.Draw(ht, hegel.Integers(uint(0), ^uint(0)))%100000)
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		w := queryWindow{From: start, To: start.Add(time.Duration(minutes) * time.Minute)}
		parts, ok := w.split(nil)
		if minutes == 1 {
			assert.False(ht, ok)
			return
		}
		require.True(ht, ok)
		require.Len(ht, parts, 2)
		assert.Equal(ht, w.From, parts[0].From)
		assert.Equal(ht, parts[0].To, parts[1].From)
		assert.Equal(ht, w.To, parts[1].To)
		assert.True(ht, parts[0].To.After(w.From))
		assert.True(ht, parts[1].From.Before(w.To))
	}, hegel.WithTestCases(128), hegel.WithDatabase(""))
}

func TestClientSharedAttemptBudgetIncludesRetriesAndUserRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"rate_limited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"user","id":"bot","type":"bot"}`)
	}))
	defer server.Close()
	c := NewClient(server.URL, "synthetic")
	var delays []time.Duration
	c.wait = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	ctx := WithAttemptBudget(t.Context(), 2)
	_, err := c.RetrieveSelf(ctx)
	require.NoError(err)
	_, err = c.RetrieveUser(ctx, "human")
	require.ErrorIs(err, ErrRequestBudget)
	assert.Equal(2, calls)
	assert.Equal([]time.Duration{2 * time.Second}, delays)
}
func TestDiscoveryClientRejectsMalformedPages(t *testing.T) {
	for _, body := range []string{
		`{"object":"list","results":[{"object":"agent","id":"private"}],"has_more":false}`,
		`{"object":"list","results":[{"object":"page","id":" "}],"has_more":false}`,
		`{"object":"list","results":[],"has_more":true,"next_cursor":null}`,
		`{"object":"list","results":[],"next_cursor":null}`,
		`{"object":"page","results":[],"has_more":false}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			c := NewClient(server.URL, "synthetic")
			_, err := c.Search(t.Context(), "")
			require.ErrorIs(t, err, ErrMalformedResponse)
			_, err = c.QueryDataSource(t.Context(), "data", "")
			require.ErrorIs(t, err, ErrMalformedResponse)
		})
	}
}

func TestQuerySaturationUsesRealFilteredHTTPWorld(t *testing.T) {
	for _, count := range []int{0, 1, 49, 50, 51, 75} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			instant := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
			world := []MeetingNote{}
			for i := range count {
				m := hydrationMeeting()
				m.ID = fmt.Sprintf("tied-%d", i)
				m.CreatedTime = instant.Format(time.RFC3339)
				world = append(world, m)
			}
			old := hydrationMeeting()
			old.ID = "historical"
			old.CreatedTime = "1900-01-01T00:00:00Z"
			world = append(world, old)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Limit  int            `json:"limit"`
					Filter *MeetingFilter `json:"filter"`
				}
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				matches := []MeetingNote{}
				for _, m := range world {
					at, err := time.Parse(time.RFC3339, m.CreatedTime)
					if !assert.NoError(err) {
						return
					}
					include := true
					for _, term := range payload.Filter.Filters {
						v := term.Filter.Value.Value
						bound, err := time.Parse("2006-01-02 15:04", v.StartDate+" "+v.StartTime)
						if !assert.NoError(err) {
							return
						}
						switch term.Filter.Operator {
						case "date_is_before":
							include = include && at.Before(bound)
						case "date_is_on_or_after":
							include = include && !at.Before(bound)
						default:
							assert.Fail("unexpected date operator")
						}
					}
					if include {
						matches = append(matches, m)
					}
				}
				more := len(matches) > payload.Limit
				matches = matches[:min(len(matches), payload.Limit)]
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"results": matches, "has_more": more}))
			}))
			defer server.Close()
			client := NewClient(server.URL, "synthetic")
			state := newSyncState()
			state.Windows = []queryWindow{{To: instant.Add(time.Hour)}}
			run := &discoveryRun{state: &state, client: client, source: client, limits: defaultDiscoveryLimits(), now: instant.Add(time.Hour)}
			admitted := map[string]bool{}
			for len(state.Windows) > 0 && run.requests < run.limits.Requests {
				changed, err := run.stepQuery(t.Context())
				require.NoError(err)
				require.True(changed)
				for _, id := range state.Admissions {
					admitted[id] = true
				}
				state.Admissions = nil
			}
			assert.Empty(state.Windows)
			assert.True(admitted["historical"])
			assert.Len(admitted, 1+min(count, 50))
			if count > 50 {
				assert.Positive(state.Coverage.Reasons["query_cap"])
			} else {
				assert.NotContains(state.Coverage.Reasons, "query_cap")
			}
		})
	}
}

func TestQueryClientRejectsOversizedProviderPage(t *testing.T) {
	meetings := make([]MeetingNote, 51)
	for i := range meetings {
		meetings[i] = hydrationMeeting()
		meetings[i].ID = strconv.Itoa(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"results": meetings, "has_more": false}))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "synthetic").QueryMeetingNotes(t.Context(), 50)
	require.ErrorIs(t, err, ErrMalformedResponse)
}

func TestIdentityRejectsMalformedPrincipalAndUnsupportedMode(t *testing.T) {
	for _, self := range []*User{nil, {Object: "bot", ID: "id", Type: "bot"}, {Object: "user", ID: " ", Type: "bot"}, {Object: "user", ID: "id"}, {Object: "user", ID: "id", Type: "unsupported"}} {
		_, err := ResolveIdentity(self, "", "configured")
		require.ErrorIs(t, err, ErrUnknownIdentity)
	}
	_, err := ResolveIdentity(&User{Object: "user", ID: "id", Type: "bot"}, "unsupported", "configured")
	require.ErrorIs(t, err, ErrUnknownIdentity)
}
