package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type meetingLinksTestStore struct {
	*mockStore

	refreshes  int
	refreshErr error
	postgres   bool
	member     int64
	event      store.OccurrenceRef
	decision   string
	ref        store.MeetingLinkRef
	err        error
	calls      int
}

func (s *meetingLinksTestStore) IsPostgreSQL() bool { return s.postgres }

func (s *meetingLinksTestStore) RefreshIdentityDatasets(context.Context) (int64, error) {
	s.refreshes++
	return 0, s.refreshErr
}

func (s *meetingLinksTestStore) LinkMeetingContext(_ context.Context, id int64, ref store.OccurrenceRef) error {
	s.calls++
	s.member, s.event, s.decision = id, ref, "link"
	return s.err
}
func (s *meetingLinksTestStore) UnlinkMeetingContext(_ context.Context, id int64, ref store.OccurrenceRef) error {
	s.calls++
	s.member, s.event, s.decision = id, ref, "unlink"
	return s.err
}
func (s *meetingLinksTestStore) SuggestMeetingLinksContext(_ context.Context, ref store.MeetingLinkRef, _ int) (store.MeetingSuggestionPage, error) {
	s.calls++
	s.ref = ref
	return store.MeetingSuggestionPage{
		Suggestions:  []store.MeetingSuggestion{{Ref: "evt:81@20270314T100000Z", MemberMessageID: 42, OverlapMinutes: 30, SharedAttendees: 1, TitleTokens: 2, Reasons: []string{"time_overlap", "shared_attendees", "title_tokens"}}},
		Unexpandable: []store.CalendarUnexpandable{{MessageID: 99, Reason: "unsupported recurrence"}},
	}, s.err
}
func (s *meetingLinksTestStore) MeetingLinksContext(_ context.Context, id int64) ([]store.MeetingLink, error) {
	s.calls++
	s.member = id
	return []store.MeetingLink{{Ref: "evt:81", Subject: "Planning", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC)}}, s.err
}

func TestMeetingLinkRoutes(t *testing.T) {
	requirements := require.New(t)

	backend := &meetingLinksTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "reader", APIKey: "synthetic-reader-key"}}}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.com"}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		name, method, path, body, token string
		status                          int
		err                             error
	}{
		{"link", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270314T100000Z","decision":"link"}`, "", 200, nil},
		{"unlink", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270314T100000Z","decision":"unlink"}`, "", 200, nil},
		{"links", "GET", "/meetings/links?message_id=42", "", "", 200, nil},
		{"invalid links ID", "GET", "/meetings/links?message_id=0", "", "", 400, nil},
		{"missing links ID", "GET", "/meetings/links", "", "", 400, nil},
		{"missing links", "GET", "/meetings/links?message_id=42", "", "", 404, sql.ErrNoRows},
		{"delegated links", "GET", "/meetings/links?message_id=42", "", secret, 401, nil},
		{"remote links", "GET", "/meetings/links?message_id=42", "", "synthetic-reader-key", 403, nil},
		{"note suggestions", "GET", "/meetings/suggestions?ref=msg:42", "", "", 200, nil},
		{"event suggestions", "GET", "/meetings/suggestions?ref=evt:81@20270314T100000Z", "", "", 200, nil},
		{"invalid ref", "GET", "/meetings/suggestions?ref=msg:42@20270314", "", "", 400, nil},
		{"invalid decision", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"split"}`, "", 400, nil},
		{"invalid key", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81@20270230","decision":"link"}`, "", 400, nil},
		{"missing", "GET", "/meetings/suggestions?ref=msg:42", "", "", 404, sql.ErrNoRows},
		{"broad", "GET", "/meetings/suggestions?ref=msg:42", "", "", 400, store.ErrQueryTooBroad},
		{"delegated read", "GET", "/meetings/suggestions?ref=msg:42", "", secret, 401, nil},
		{"remote read", "GET", "/meetings/suggestions?ref=msg:42", "", "synthetic-reader-key", 403, nil},
		{"delegated write", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"link"}`, secret, 401, nil},
		{"remote write", "POST", "/meetings/links/decisions", `{"message_id":42,"event":"evt:81","decision":"link"}`, "synthetic-reader-key", 403, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			backend.err = tc.err
			before := backend.calls
			beforeRefresh := backend.refreshes
			r := httptest.NewRequest(tc.method, "/api/v1"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			if tc.token == secret {
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			} else if tc.token != "" {
				r.Header.Set("X-Api-Key", tc.token)
			} else {
				r.Header.Set("X-Api-Key", "synthetic-owner-key")
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.status == 200 && tc.method == http.MethodPost {
				assertions.Equal(beforeRefresh+1, backend.refreshes)
			} else {
				assertions.Equal(beforeRefresh, backend.refreshes)
			}
			if tc.status >= 400 && tc.err == nil {
				assertions.Equal(before, backend.calls)
			}
			if tc.status != 200 {
				return
			}
			if tc.method == http.MethodPost {
				assertions.Equal(int64(42), backend.member)
				assertions.Equal(store.OccurrenceRef{MessageID: 81, Key: "20270314T100000Z"}, backend.event)
				assertions.Equal(tc.name, backend.decision)
				var response map[string]any
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
				assertions.Equal("ready", response["cache_state"])
				delete(response, "cache_state")
				body, err := json.Marshal(response)
				requirements.NoError(err)
				assertions.JSONEq(tc.body, string(body))
			} else if tc.name == "links" {
				assertions.Equal(int64(42), backend.member)
				assertions.JSONEq(`{"links":[{"ref":"evt:81","subject":"Planning","start":"2027-03-14T10:00:00Z","end":"2027-03-14T11:00:00Z"}]}`, w.Body.String())
			} else {
				want := store.MeetingLinkRef{Kind: "msg", MessageID: 42}
				if strings.Contains(tc.path, "ref=evt:") {
					want = store.MeetingLinkRef{Kind: "evt", MessageID: 81, Key: "20270314T100000Z"}
				}
				assertions.Equal(want, backend.ref)
				assertions.JSONEq(`{"suggestions":[{"ref":"evt:81@20270314T100000Z","member_message_id":42,"overlap_minutes":30,"shared_attendees":1,"title_tokens":2,"reasons":["time_overlap","shared_attendees","title_tokens"]}],"unexpandable":[{"message_id":99,"reason":"unsupported recurrence"}]}`, w.Body.String())
			}
		})
	}
}

func TestMeetingLinkDecisionPostgreSQLSkipsCacheRefresh(t *testing.T) {
	for _, decision := range []string{"link", "unlink"} {
		t.Run(decision, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := &meetingLinksTestStore{
				mockStore: &mockStore{stats: &StoreStats{}},
				postgres:  true, refreshErr: errors.New("Parquet cache is unavailable on PostgreSQL"),
			}
			srv := NewServer(&config.Config{}, backend, nil, testLogger())
			request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(`{"message_id":42,"event":"evt:81","decision":"`+decision+`"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code)
			var body MeetingLinkDecisionResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal("ready", body.CacheState)
			assert.Equal(1, backend.calls)
			assert.Equal(int64(42), backend.member)
			assert.Equal(store.OccurrenceRef{MessageID: 81}, backend.event)
			assert.Equal(decision, backend.decision)
			assert.Zero(backend.refreshes)
		})
	}
}

func TestMeetingLinkDecisionReportsStaleCache(t *testing.T) {
	backend := &meetingLinksTestStore{mockStore: &mockStore{stats: &StoreStats{}}, refreshErr: errors.New("refresh failed")}
	srv := NewServer(&config.Config{}, backend, nil, testLogger())
	for _, decision := range []string{"link", "unlink"} {
		t.Run(decision, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(`{"message_id":42,"event":"evt:81","decision":"`+decision+`"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code)
			var body map[string]any
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal("stale", body["cache_state"])
			assert.Equal(decision, backend.decision)
		})
	}
}
