package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/timeline"
)

func timelineRouteFixture(t *testing.T) (*Server, *store.Store, *store.Source) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, _, err := testutil.CreateIndexedSourceMessage(st, "alice@example.com", "one", "Planning", "Hello")
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	srv.clock = func() time.Time { return time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv, st, src
}
func timelineGET(srv *Server, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/timeline"+path, nil)
	if token == "" {
		r.Header.Set("X-Api-Key", "owner")
	} else {
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}
func TestTimelineOwnerFormatsAndErrors(t *testing.T) {
	srv, _, _ := timelineRouteFixture(t)
	for _, tc := range []struct {
		query, content string
		status         int
	}{
		{"?date=2027-03-01", `"mode":"day"`, 200},
		{"?date=2027-03-01&format=markdown", "", 200},
		{"?cursor=invalid", "cursor_expired", 400},
		{"?with=person%3Aabc", `"error":"invalid_request"`, 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			w := timelineGET(srv, tc.query, "")
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusOK {
				contentType := "application/json; charset=utf-8"
				if tc.query == "?date=2027-03-01&format=markdown" {
					contentType = "text/markdown; charset=utf-8"
				}
				assertions.Equal(contentType, w.Header().Get("Content-Type"))
				assertions.Equal("nosniff", w.Header().Get("X-Content-Type-Options"))
			}
			if tc.content != "" {
				assertions.Contains(w.Body.String(), tc.content)
			}
		})
	}
}
func TestTimelineLibrarySourceScopeDeniesOwnerTimeline(t *testing.T) {
	requirements := require.New(t)
	srv, _, src := timelineRouteFixture(t)
	handler := srv.Handler(func(_ http.ResponseWriter, _ *http.Request, op Operation) bool {
		return op.ID == "getTimeline"
	})
	const path = "/api/v1/timeline?mode=day&date=2027-03-01"
	owner := httptest.NewRecorder()
	handler.ServeHTTP(owner, httptest.NewRequest(http.MethodGet, path, nil))
	requirements.Equal(http.StatusOK, owner.Code, owner.Body.String())

	for _, tc := range []struct {
		name string
		ids  []int64
	}{
		{"source", []int64{src.ID}},
		{"empty", []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			ctx := sourcescope.WithSourceIDs(t.Context(), tc.ids)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
			assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
			assertions.Contains(response.Body.String(), "unsupported_source_scope")
		})
	}
}

func TestTimelineDelegatedAdmission(t *testing.T) {
	srv, _, src := timelineRouteFixture(t)
	for _, tc := range []struct {
		name    string
		perms   []agentgrant.Permission
		sources []agentgrant.SourceRef
		status  int
	}{
		{"calendar only", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, 403},
		{"no admitted sources", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 9998, Type: "gmail", Identifier: "removed@example.com"}}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			_, secret, _, err := srv.agentGrants.Issue(tc.name, tc.perms, tc.sources, time.Time{})
			requirements.NoError(err)
			w := timelineGET(srv, "?mode=day&date=2027-03-01", secret)
			requirements.Equal(tc.status, w.Code, w.Body.String())
		})
	}
}
func TestTimelinePersonAuthorizationAndAmbiguity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, src := timelineRouteFixture(t)
	var people []*store.Person
	var participants []int64
	for _, address := range []string{"alice-one@example.com", "alice-two@example.com"} {
		participant, err := st.EnsureParticipant(address, "Alice", "example.com")
		requirements.NoError(err)
		p, _, err := st.CreatePersonFromParticipant(participant)
		requirements.NoError(err)
		people = append(people, p)
		participants = append(participants, participant)
	}
	var messageID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM messages WHERE source_id = ?"), src.ID).Scan(&messageID))
	requirements.NoError(st.ReplaceMessageRecipients(messageID, "to", participants, nil))
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, query := range []string{
		fmt.Sprintf("?with=person:%d", people[0].ID),
		"?with=Alice", "?mode=day&with=Alice", "?around=msg:1&with=Alice", "?with=unknown", "?mode=day&with=Alice&days=invalid",
	} {
		w := timelineGET(srv, query, secret)
		assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), `"error":"permission_denied"`)
		assertions.Contains(w.Body.String(), "Relationship filters require owner access")
		assertions.NotContains(w.Body.String(), "candidates")
	}
	owner := timelineGET(srv, fmt.Sprintf("?with=person:%d", people[0].ID), "")
	assertions.Equal(http.StatusOK, owner.Code, owner.Body.String())
	w := timelineGET(srv, "?with=Alice", "")
	requirements.Equal(409, w.Code, w.Body.String())
	var got struct {
		Candidates []timeline.Candidate `json:"candidates"`
	}
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assertions.Len(got.Candidates, 2)
}

func TestTimelineDelegatedCopiesHaveDeterministicWireOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, src := timelineRouteFixture(t)
	owner, err := st.EnsureParticipant("alice@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(src.ID, "alice@example.com", "manual"))
	conversation, err := st.EnsureConversation(src.ID, "visible", "Visible conversation")
	requirements.NoError(err)
	var messageIDs []int64
	for i := range 3 {
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: fmt.Sprintf("visible-%d", i), MessageType: "email", SentAt: sql.NullTime{Time: srv.clockNow().Add(-time.Hour), Valid: true}, SenderID: sql.NullInt64{Int64: peer, Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
		messageIDs = append(messageIDs, id)
	}
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC"})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	first := timelineGET(srv, "?days=1", secret)
	requirements.Equal(200, first.Code, first.Body.String())
	assertions.Contains(first.Body.String(), fmt.Sprintf(`"copies":{"%d":1,"%d":1,"%d":1}`, messageIDs[0], messageIDs[1], messageIDs[2]), "exercise an actual multi-key map on the HTTP wire")
	for range 8 {
		next := timelineGET(srv, "?days=1", secret)
		requirements.Equal(200, next.Code, next.Body.String())
		assertions.Equal(first.Body.String(), next.Body.String())
	}
}

// Timeline models share short Go names with existing APIs. The published
// schemas must retain the timeline fields instead of reusing another domain.
func TestTimelineSchemaUsesDomainTypes(t *testing.T) {
	schemas := OpenAPIDocument().Components.Schemas.Map()
	for name, fields := range map[string][]string{
		"TimelineResult":    {"mode", "window", "days", "omitted"},
		"TimelineCandidate": {"person_id", "display_name", "address"},
		"TimelineEntry":     {"kind", "anchor_ref", "open", "hidden_text"},
		"TimelineWindow":    {"from", "to"},
	} {
		schema := schemas[name]
		require.NotNil(t, schema, name)
		for _, field := range fields {
			assert.Contains(t, schema.Properties, field, name)
		}
	}
}

func TestTimelineParserPresenceAndExplicitZero(t *testing.T) {
	assertions := assert.New(t)
	for _, tc := range []struct {
		query               string
		before, after       int
		beforeSet, afterSet bool
	}{
		{"", 0, 0, false, false},
		{"before=0", 0, 0, true, false},
		{"after=0", 0, 0, false, true},
		{"before=0&after=0", 0, 0, true, true},
		{"before=2&after=3", 2, 3, true, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			assertions := assert.New(t)
			got, err := parseTimelineRequest(tc.query)
			require.NoError(t, err)
			assertions.Equal(tc.before, got.Before)
			assertions.Equal(tc.after, got.After)
			assertions.Equal(tc.beforeSet, got.BeforeSet)
			assertions.Equal(tc.afterSet, got.AfterSet)
		})
	}
	got, err := parseTimelineRequest("")
	require.NoError(t, err)
	assertions.Zero(got.Days)
	assertions.Zero(got.Budget)
	for _, query := range []string{"days=0", "budget=0", "days=1&days=2", "include_cold=bad"} {
		_, err := parseTimelineRequest(query)
		assertions.ErrorIs(err, timeline.ErrInvalidRequest, query)
	}
}

func TestTimelineFreshnessUsesSourceStatus(t *testing.T) {
	for _, tc := range []struct {
		name, kind, identifier, job, outcome, reason, command string
		next                                                  time.Duration
		never                                                 bool
		errors                                                int64
	}{
		{name: "scheduled calendar stale", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: -time.Hour, reason: "stale", command: "msgvault sync-calendar calendar@example.com"},
		{name: "scheduled SMS stale", kind: "synctech_sms", identifier: "+12025550123", job: "synctech-sms:+12025550123", next: -time.Hour, reason: "stale"},
		{name: "calendar not due", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: 48 * time.Hour},
		{name: "calendar never succeeded", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: time.Hour, never: true, reason: "never_synced"},
		{name: "calendar failed", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: time.Hour, outcome: "failed"},
		{name: "calendar cancelled", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: time.Hour, outcome: "cancelled"},
		{name: "calendar paused", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: time.Hour, outcome: "paused"},
		{name: "calendar completed with errors", kind: "gcal", identifier: "calendar@example.com/work", job: "gcal:calendar@example.com", next: time.Hour, outcome: "completed", errors: 2},
		{name: "manual discovery alias", kind: "caldav", identifier: `["owner@example.com","https://calendar.example.com/discovered/work/"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			srv, st, _ := timelineRouteFixture(t)
			src, err := st.GetOrCreateSource(tc.kind, tc.identifier)
			requirements.NoError(err)
			if !tc.never {
				run, err := st.StartSync(src.ID, "full")
				requirements.NoError(err)
				requirements.NoError(st.CompleteSync(run, ""))
				_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET started_at = ?, completed_at = ? WHERE id = ?"), srv.clockNow().Add(-49*time.Hour), srv.clockNow().Add(-48*time.Hour), run)
				requirements.NoError(err)
			}
			if tc.outcome != "" {
				run, err := st.StartSync(src.ID, "full")
				requirements.NoError(err)
				requirements.NoError(st.FailSync(run, "synthetic failure"))
				_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET status = ?, errors_count = ?, started_at = ? WHERE id = ?"), tc.outcome, tc.errors, srv.clockNow().Add(-time.Hour), run)
				requirements.NoError(err)
			}
			sched := newMockScheduler()
			if tc.job != "" {
				sched.jobStatuses = []JobStatus{{Name: tc.job, Schedule: "0 */6 * * *", NextRun: srv.clockNow().Add(tc.next)}}
			}
			srv.scheduler = sched
			srv.cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://calendar.example.com/alias/", Username: "owner@example.com"}}
			w := timelineGET(srv, "?date=2027-03-01", "")
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			var got timeline.Timeline
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &got))
			if tc.reason == "" && tc.outcome == "" {
				assertions.Empty(got.Freshness.Sources)
			} else {
				requirements.Len(got.Freshness.Sources, 1)
				item := got.Freshness.Sources[0]
				assertions.Equal(tc.reason, item.Reason)
				assertions.Equal(tc.errors, item.LastSyncErrors)
				assertions.Equal(src.ID, *item.SourceID)
				if tc.command != "" {
					assertions.Equal(tc.command, item.SyncCommand)
				}
				if tc.outcome != "" {
					assertions.Equal(tc.outcome, item.Outcome)
					assertions.Contains(timeline.Markdown(&got), "⚠ gcal:calendar@example.com/work")
				}
			}
			visibleWarnings := len(got.Freshness.Sources)
			// The fixture's Gmail source warns for the owner but is outside this grant.
			sched.scheduled["alice@example.com"] = true
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
			requirements.NoError(err)
			w = timelineGET(srv, "?date=2027-03-01", token)
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &got))
			assertions.Len(got.Freshness.Sources, visibleWarnings)
			for _, item := range got.Freshness.Sources {
				assertions.Equal(src.ID, *item.SourceID)
			}
		})
	}
}
