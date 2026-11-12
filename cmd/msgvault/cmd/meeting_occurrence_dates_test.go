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
	persist := func(source int64, key string, at time.Time) int64 {
		raw := `{"id":"` + key + `","start":{"dateTime":"` + at.Format(time.RFC3339) + `"},"end":{"dateTime":"` + at.Add(time.Hour).Format(time.RFC3339) + `"}}`
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, SourceMessageID: key, MessageType: "calendar_event", SentAt: sql.NullTime{Time: at, Valid: true}, SenderID: sql.NullInt64{Int64: owner, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(raw), RawFormat: "gcal_json"})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{person}, []string{""}))
		return id
	}
	dec := time.Date(2024, 12, 31, 10, 0, 0, 0, time.UTC)
	jan := dec.Add(24 * time.Hour)
	left, right := persist(first.ID, "first", dec), persist(second.ID, "second", jan)
	_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "evt", MessageID: left}, store.MeetingLinkRef{Kind: "evt", MessageID: right}, "link")
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analytics, true)
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine(t.Context(), analytics, "", nil)
	require.NoError(t, err)
	defer engine.Close()
	after := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
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
			people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Explore: query.ExploreRequest{Context: tc.scope}})
			require.NoError(t, err)
			got = 0
			for _, p := range people.Rows {
				if p.ID == person {
					got = p.MeetingCount
				}
			}
			assert.Equal(t, tc.want, got)
			rel, err := engine.Relationships(t.Context(), query.RelationshipsRequest{Context: tc.scope, ShowAll: true})
			require.NoError(t, err)
			got = 0
			for _, p := range rel.Rows {
				if p.CanonicalID == person {
					got = p.Signals.MeetingCount
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
