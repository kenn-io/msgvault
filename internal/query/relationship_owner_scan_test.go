package query

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilteredRelationshipOwnerLookupStaysWithinSelectedGroups(t *testing.T) {
	b := NewTestDataBuilder(t)
	a := b.AddSourceWithType("first@example.test", "granola")
	z := b.AddSourceWithType("second@example.test", "zoom")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	person := b.AddParticipant("alice@example.test", "example.test", "Alice")
	b.AddOwnerParticipant(a, owner)
	b.AddOwnerParticipant(z, owner)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	first := b.AddMessage(MessageOpt{SourceID: a, MessageType: "meeting_transcript", SentAt: now.AddDate(-1, 0, 0)})
	last := b.AddMessage(MessageOpt{SourceID: z, MessageType: "meeting_transcript", SentAt: now})
	b.AddFrom(first, owner, "Owner")
	b.AddTo(first, person, "Alice")
	b.AddTo(last, person, "Alice")
	b.meetingGroups = fmt.Sprintf("(SELECT %d::BIGINT AS message_id, %d::BIGINT AS group_id UNION ALL SELECT %d, %d)", first, first, last, first)
	for range 1000 {
		id := b.AddMessage(MessageOpt{SourceID: a, MessageType: "meeting_transcript", SentAt: now.AddDate(-1, 0, 0)})
		b.AddFrom(id, owner, "Owner")
		b.AddTo(id, person, "Alice")
	}
	e := b.BuildEngine()
	checkOwnerReads := func(run func()) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "profile.json")
		_, err := e.db.ExecContext(t.Context(), "SET enable_profiling = 'json'")
		require.NoError(t, err)
		_, err = e.db.ExecContext(t.Context(), "SET profiling_output = '"+escapePath(path)+"'")
		require.NoError(t, err)
		run()
		_, err = e.db.ExecContext(t.Context(), "PRAGMA disable_profiling")
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var profile any
		require.NoError(t, json.Unmarshal(data, &profile))
		var ownerScans int
		var ownerRows float64
		var visit func(any)
		visit = func(v any) {
			switch n := v.(type) {
			case map[string]any:
				if n["type"] == "TABLE_SCAN" {
					info, _ := n["extra_info"].(map[string]any)
					if strings.Contains(fmt.Sprint(info["Filename(s)"]), "relationship_activity") && strings.Contains(fmt.Sprint(info["Filters"]), "is_owner") {
						ownerScans++
						rows, ok := n["rows_scanned"].(float64)
						require.True(t, ok)
						ownerRows += rows
					}
				}
				for _, child := range n {
					visit(child)
				}
			case []any:
				for _, child := range n {
					visit(child)
				}
			}
		}
		visit(profile)
		require.Positive(t, ownerScans)
		require.LessOrEqual(t, ownerRows, float64(2), "owner lookup should read only copies of the selected meeting")
	}
	checkOwnerReads(func() {
		result, err := e.Relationships(t.Context(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true, Context: Context{SourceIDs: []int64{z}}})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		require.Equal(t, int64(1), result.Rows[0].Signals.MeetingCount)
	})
	checkOwnerReads(func() {
		calendar, err := e.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: 2026, Timezone: "UTC"})
		require.NoError(t, err)
		var meetings int64
		for _, day := range calendar.Days {
			meetings += day.Meetings
		}
		require.Equal(t, int64(1), meetings)
	})
}
