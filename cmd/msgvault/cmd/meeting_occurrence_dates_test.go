package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLinkedMeetingCopiesUseScopedDate(t *testing.T) {
	dir := t.TempDir()
	dbPath, analytics := filepath.Join(dir, "archive.db"), filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	first, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(t, err)
	second, err := st.GetOrCreateSource("gcal", "other@example.com")
	require.NoError(t, err)
	for _, id := range []int64{first.ID, second.ID} {
		require.NoError(t, st.AddAccountIdentity(id, "owner@example.com", "manual"))
	}
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.com", "Person", "example.com")
	require.NoError(t, err)
	other, err := st.EnsureParticipant("other-person@example.com", "Other", "example.com")
	require.NoError(t, err)
	persist := func(source, sender int64, fromMe bool, key string, at time.Time) int64 {
		organizer := ""
		if fromMe {
			organizer = `,"organizer":{"email":"other-person@example.com","self":true}`
		}
		raw := `{"id":"` + key + `","start":{"dateTime":"` + at.Format(time.RFC3339) + `"},"end":{"dateTime":"` + at.Add(time.Hour).Format(time.RFC3339) + `"}` + organizer + `}`
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, SourceMessageID: key, MessageType: "calendar_event", SentAt: sql.NullTime{Time: at, Valid: true}, SenderID: sql.NullInt64{Int64: sender, Valid: true}, IsFromMe: fromMe}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(raw), RawFormat: "gcal_json"})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
		return id
	}
	dec := time.Date(2024, 12, 29, 10, 0, 0, 0, time.UTC)
	jan := dec.Add(5 * 24 * time.Hour)
	right := persist(second.ID, owner, false, "second", jan)
	left := persist(first.ID, other, true, "first", dec)
	late, err := st.EnsureParticipant("late@late.example", "Late", "late.example")
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageRecipients(right, "to", []int64{person, other, late}, []string{"", "", ""}))
	_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "evt", MessageID: left}, store.MeetingLinkRef{Kind: "evt", MessageID: right}, "link")
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer engine.Close()
	after := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	latePerson, err := engine.GetPerson(t.Context(), late, query.Context{}, nil)
	require.NoError(t, err)
	require.NotNil(t, latePerson)
	assert.Equal(t, dec, latePerson.FirstAt)
	assert.Equal(t, dec, latePerson.LastAt)
	for _, tc := range []struct {
		sources []int64
		want    time.Time
	}{{[]int64{first.ID, second.ID}, dec}, {[]int64{second.ID}, jan}} {
		detail, err := engine.GetPerson(t.Context(), late, query.Context{SourceIDs: tc.sources}, nil)
		require.NoError(t, err)
		require.NotNil(t, detail)
		assert.Equal(t, tc.want, detail.FirstAt)
		assert.Equal(t, tc.want, detail.LastAt)
	}
	lateDomain, err := engine.SearchDomains(t.Context(), query.DomainSearchRequest{})
	require.NoError(t, err)
	foundLateDomain := false
	for _, domain := range lateDomain.Rows {
		if domain.Domain == "late.example" {
			foundLateDomain = true
			assert.Equal(t, dec, domain.FirstAt)
			assert.Equal(t, dec, domain.LastAt)
		}
	}
	assert.True(t, foundLateDomain)
	lateTimeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: late, Context: query.Context{Before: &after}})
	require.NoError(t, err)
	if assert.Len(t, lateTimeline.Rows, 1) {
		assert.Equal(t, dec, lateTimeline.Rows[0].OccurredAt)
		require.NotNil(t, lateTimeline.Rows[0].AnchorMessageID)
		assert.Equal(t, right, *lateTimeline.Rows[0].AnchorMessageID)
		assert.Equal(t, second.ID, lateTimeline.Rows[0].SourceID)
	}
	timeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Context: query.Context{SourceIDs: []int64{second.ID}}})
	require.NoError(t, err)
	require.Len(t, timeline.Rows, 1)
	assert.Equal(t, jan, timeline.Rows[0].OccurredAt)
	for _, tc := range []struct {
		year int
		want int64
	}{{2024, 1}, {2025, 0}} {
		calendar, err := engine.RelationshipCalendar(t.Context(), query.RelationshipCalendarRequest{CanonicalID: person, Year: tc.year, Timezone: "UTC"})
		require.NoError(t, err)
		var meetings int64
		for _, day := range calendar.Days {
			meetings += day.Meetings
		}
		assert.Equal(t, tc.want, meetings, "year %d", tc.year)
	}
	for _, tc := range []struct {
		name  string
		scope query.Context
		want  int64
	}{
		{"unfiltered", query.Context{}, 1},
		{"both_sources", query.Context{SourceIDs: []int64{first.ID, second.ID}}, 1},
		{"all_sources_before", query.Context{Before: &after}, 1},
		{"second_source_before", query.Context{Before: &after, SourceIDs: []int64{second.ID}}, 0},
		{"all_sources_after", query.Context{After: &after}, 0},
		{"first_source_after", query.Context{After: &after, SourceIDs: []int64{first.ID}}, 0},
		{"second_source_after", query.Context{After: &after, SourceIDs: []int64{second.ID}}, 1},
		{"both_sources_after", query.Context{After: &after, SourceIDs: []int64{first.ID, second.ID}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, err := engine.GetPerson(t.Context(), person, tc.scope, nil)
			require.NoError(t, err)
			var got int64
			if detail != nil {
				got = detail.MeetingCount
			}
			assert.Equal(t, tc.want, got)
			timeline, err := engine.RelationshipTimeline(t.Context(), query.RelationshipTimelineRequest{CanonicalID: person, Context: tc.scope})
			require.NoError(t, err)
			assert.Len(t, timeline.Rows, int(tc.want))
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: tc.scope}})
			require.NoError(t, err)
			got = 0
			for _, p := range people.Rows {
				if p.ID == person {
					got = p.MeetingCount
					if tc.name == "unfiltered" || tc.name == "both_sources" {
						assert.Equal(t, dec, p.FirstAt)
						assert.Equal(t, dec, p.LastAt)
					}
				}
			}
			assert.Equal(t, tc.want, got)
			rel, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: tc.scope, ShowAll: true})
			require.NoError(t, err)
			got = 0
			foundLate := false
			for _, p := range rel.Rows {
				if tc.name != "second_source_after" {
					assert.NotEqual(t, other, p.CanonicalID)
				}
				if p.CanonicalID == late && tc.name == "unfiltered" {
					foundLate = true
					assert.Equal(t, dec, p.LastAt)
				}
				if p.CanonicalID == person {
					got = p.Signals.MeetingCount
				}
			}
			assert.Equal(t, tc.want, got)
			if tc.name == "unfiltered" {
				assert.True(t, foundLate)
			}
		})
	}
}
