package store

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

const projectionMigrationName = "meeting_projection_v1"

func removeMeetingProjectionSchema(t *testing.T, st *Store) {
	t.Helper()
	for _, statement := range []string{`DROP TABLE meeting_action_items`, `DROP TABLE meeting_details`, `DELETE FROM applied_migrations WHERE name = 'meeting_projection_v1'`} {
		_, err := st.db.Exec(statement)
		require.NoError(t, err)
	}
}

func TestMeetingProjectionUpgradeAllFormatsTwice(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	fixtures := []struct {
		format, raw string
		coverage    meetingcontent.Coverage
	}{
		{"meeting_json", meetingProjectionRaw, meetingcontent.CoverageAvailable},
		{"granola_json", `{"summary_text":"Granola summary","transcript":[]}`, meetingcontent.CoverageUnsupported},
		{"circleback_json", `{"meeting":{"notes":"Circleback summary","actionItems":[]}}`, meetingcontent.CoverageAvailable},
		{"notion_meeting_json", `{"schema_version":1,"discovery":{},"canonical":{"summary":"Notion summary"}}`, meetingcontent.CoverageUnsupported},
	}
	ids := make([]int64, len(fixtures))
	rawBefore := make([][]byte, len(fixtures))
	for i, fixture := range fixtures {
		_, ids[i] = projectionFixture(t, st, strconv.Itoa(i), fixture.format, fixture.raw)
		requirements.NoError(st.db.QueryRow(`SELECT raw_data FROM message_raw WHERE message_id = ?`, ids[i]).Scan(&rawBefore[i]))
	}
	removeMeetingProjectionSchema(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	for i, fixture := range fixtures {
		content, _ := readProjection(t, st, ids[i])
		assertions.Equal(fixture.coverage, content.ActionCoverage, fixture.format)
		assertions.Equal(meetingcontent.StateAvailable, content.Summary.State, fixture.format)
		var rawAfter []byte
		requirements.NoError(st.db.QueryRow(`SELECT raw_data FROM message_raw WHERE message_id = ?`, ids[i]).Scan(&rawAfter))
		assertions.Equal(rawBefore[i], rawAfter)
	}
	applied, err := st.IsMigrationAppliedContext(t.Context(), projectionMigrationName, 1)
	requirements.NoError(err)
	assertions.True(applied)
	rejectMeetingProjectionWrites(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()), "completed upgrade must not rewrite projections")
}

func TestMeetingProjectionCopySubsetRebuildsFromLegacyEvidence(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("CopySubset accepts SQLite archives")
	}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "source.db")
			st, err := OpenForTest(path)
			requirements.NoError(err)
			requirements.NoError(st.InitSchema())
			_, id := projectionFixture(t, st, "subset", "meeting_json", meetingProjectionRaw)
			if legacy {
				removeMeetingProjectionSchema(t, st)
			}
			requirements.NoError(st.Close())
			dest := filepath.Join(t.TempDir(), "subset")
			_, err = CopySubset(path, dest, 100, false)
			requirements.NoError(err)
			copied, err := OpenForTest(filepath.Join(dest, "msgvault.db"))
			requirements.NoError(err)
			t.Cleanup(func() { _ = copied.Close() })
			content, _ := readProjection(t, copied, id)
			requirements.Len(content.Actions, 1)
			assertions.Equal("Send brief", content.Actions[0].Title)
			applied, err := copied.IsMigrationAppliedContext(t.Context(), projectionMigrationName, 1)
			requirements.NoError(err)
			assertions.True(applied)
		})
	}
}

// Existing archives permit the whole signed message-ID range. An initial zero
// cursor would skip nonpositive rows yet incorrectly mark this upgrade complete.
func TestMeetingProjectionUpgradeIncludesNonpositiveIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "signed-ids@example.com")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "signed-ids", "Synthetic signed IDs")
	requirements.NoError(err)
	insert := `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
  VALUES (?, ?, ?, ?, 'meeting_transcript')`
	if st.IsPostgreSQL() {
		insert = `INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type)
   OVERRIDING SYSTEM VALUE VALUES (?, ?, ?, ?, 'meeting_transcript')`
	}
	ids := []int64{math.MinInt64, -1, 0, 7}
	for _, id := range ids {
		_, err := st.db.Exec(insert, id, conversationID, source.ID, fmt.Sprintf("signed-%d", id))
		requirements.NoError(err)
		requirements.NoError(upsertMessageRawWithFormat(st.db, id, []byte(meetingProjectionRaw), "meeting_json"))
	}
	removeMeetingProjectionSchema(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	var projected int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_details`).Scan(&projected))
	assertions.Equal(len(ids), projected, "completion must include the entire signed ID range")
	for _, id := range ids {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			content, _ := readProjection(t, st, id)
			assertions.Equal("Source summary", content.Summary.Text)
			requirements.Len(content.Actions, 1)
			assertions.Equal("Send brief", content.Actions[0].Title)
			var title string
			requirements.NoError(st.db.QueryRow(`SELECT title FROM meeting_action_items WHERE message_id = ?`, id).Scan(&title))
			assertions.Equal("Send brief", title)
		})
	}
	applied, err := st.IsMigrationAppliedContext(t.Context(), projectionMigrationName, 1)
	requirements.NoError(err)
	assertions.True(applied)
	rejectMeetingProjectionWrites(t, st)
	requirements.NoError(st.InitSchemaContext(t.Context()), "completed signed-ID upgrade must not rewrite projections")
}
