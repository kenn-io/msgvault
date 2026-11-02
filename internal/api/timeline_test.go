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
		{"?date=2027-03-01&format=markdown", "nothing archived", 200},
		{"?days=32", "", 400}, {"?days=0", "", 400}, {"?budget=0", "", 400},
		{"?date=bad", "", 400}, {"?before=-1", "", 400},
		{"?days=1&days=2", "", 400}, {"?include_cold=bad", "", 400},
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
			assertions.Contains(w.Body.String(), tc.content)
		})
	}
}
func TestTimelineLibrarySourceScopeDeniesOwnerNotes(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, st, src := timelineRouteFixture(t)
	const noteBody = "Synthetic owner-only timeline note"
	_, err := st.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2027-03-01", Body: noteBody, Author: "Synthetic Owner",
	})
	requirements.NoError(err)
	handler := srv.Handler(func(_ http.ResponseWriter, _ *http.Request, op Operation) bool {
		return op.ID == "getTimeline"
	})
	const path = "/api/v1/timeline?mode=day&date=2027-03-01"
	owner := httptest.NewRecorder()
	handler.ServeHTTP(owner, httptest.NewRequest(http.MethodGet, path, nil))
	requirements.Equal(http.StatusOK, owner.Code, owner.Body.String())
	requirements.Contains(owner.Body.String(), noteBody, "the unrestricted host request must exercise owner-only notes")

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
			assertions.NotContains(response.Body.String(), noteBody)
		})
	}

	_, secret, _, err := srv.agentGrants.Issue("timeline note control", []agentgrant.Permission{agentgrant.PermissionMessageRead},
		[]agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	delegated := timelineGET(srv, "?mode=day&date=2027-03-01", secret)
	requirements.Equal(http.StatusOK, delegated.Code, delegated.Body.String())
	assertions.NotContains(delegated.Body.String(), noteBody)
}

func TestTimelineDelegatedAdmission(t *testing.T) {
	srv, _, src := timelineRouteFixture(t)
	for _, tc := range []struct {
		name    string
		perms   []agentgrant.Permission
		sources []agentgrant.SourceRef
		status  int
	}{
		{"message", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, 200},
		{"calendar only", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, 403},
		{"no admitted sources", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 9998, Type: "gmail", Identifier: "removed@example.com"}}, 403},
		{"missing source", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 9999, Type: "gmail", Identifier: "missing@example.com"}}, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, secret, _, err := srv.agentGrants.Issue(tc.name, tc.perms, tc.sources, time.Time{})
			requirements.NoError(err)
			w := timelineGET(srv, "?mode=day&date=2027-03-01", secret)
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.status == 200 {
				var got timeline.Timeline
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &got))
				assertions.Equal(timeline.ModeDay, got.Mode)
				assertions.Contains(got.Gaps, timeline.Gap{Section: "calls", Code: "owner-only"})
			}
		})
	}
}
func TestTimelineMutationGate(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		gated, _, err := operationGateRequest(httptest.NewRequest(method, "/api/v1/timeline", nil), requestAuthentication{})
		require.NoError(t, err)
		assert.True(t, gated, "owner timelines may cache loop evidence")
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

func TestTimelineDelegatedRawBytesIgnoreHiddenMutation(t *testing.T) {
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
	anchor := timelineGET(srv, fmt.Sprintf("?around=msg:%d&before=0&after=0", messageIDs[0]), secret)
	requirements.Equal(http.StatusOK, anchor.Code, anchor.Body.String())
	var anchored timeline.Timeline
	requirements.NoError(json.Unmarshal(anchor.Body.Bytes(), &anchored))
	requirements.NotNil(anchored.Anchor)
	assertions.False(anchored.Anchor.Filtered)
	assertions.Equal(timeline.ModeAnchor, anchored.Mode)
	first := timelineGET(srv, "?days=1", secret)
	requirements.Equal(200, first.Code, first.Body.String())
	assertions.Contains(first.Body.String(), fmt.Sprintf(`"copies":{"%d":1,"%d":1,"%d":1}`, messageIDs[0], messageIDs[1], messageIDs[2]), "exercise an actual multi-key map on the HTTP wire")
	_, _, err = testutil.CreateIndexedSourceMessage(st, "hidden@example.com", "hidden", "Hidden subject", "Hidden body")
	requirements.NoError(err)
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
		{"before=5&after=5", 5, 5, true, true},
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
	for _, query := range []string{"days=0", "budget=0"} {
		_, err := parseTimelineRequest(query)
		require.ErrorIs(t, err, timeline.ErrInvalidRequest, query)
	}
	got, err := parseTimelineRequest("")
	require.NoError(t, err)
	assertions.Zero(got.Days)
	assertions.Zero(got.Budget)
}

// The production adapter leaves MaxDirect omitted so stored projection policy,
// rather than the daemon configuration default, governs full-audience reads.
func TestTimelineStoreSourcesUsesStoredProjectionPolicy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, src := timelineRouteFixture(t)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	requirements.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	conversation, err := st.EnsureConversation(src.ID, "policy-audience", "Policy audience")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: "policy-audience", MessageType: "email", SentAt: sql.NullTime{Time: srv.clockNow().Add(-time.Hour), Valid: true}, SenderID: sql.NullInt64{Int64: peer, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner, other}, nil))
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", MaxDirectCounterparts: 1})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	sources, err := timeline.NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	assertions.Zero(sources.Filters.MaxDirect)
	for _, includeNoise := range []bool{false, true} {
		w := timelineGET(srv, fmt.Sprintf("?date=2027-03-01&include_noise=%t", includeNoise), "")
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		var got timeline.Timeline
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &got))
		var bursts []timeline.Entry
		for _, day := range got.Days {
			bursts = append(bursts, day.Bursts...)
		}
		if includeNoise {
			requirements.Len(bursts, 1)
			requirements.NotNil(bursts[0].Burst)
			assertions.Zero(bursts[0].Burst.Inbound)
			assertions.Zero(bursts[0].Burst.Outbound)
		} else {
			assertions.Empty(bursts)
		}
	}
}
