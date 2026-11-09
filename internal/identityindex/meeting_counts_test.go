package identityindex

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCounts(t *testing.T) {
	for _, tc := range []struct {
		name, messages, recipients, keys, sources string
		meetings, activities                      int64
		sourceCounts                              []int64
		dailyOnly                                 bool
	}{
		{name: "multiple occurrences", messages: "(100,1,'meeting_transcript')", recipients: "(100,1),(100,2)", keys: "(100,'uid:weekly/first'),(100,'uid:weekly/second')", meetings: 2, activities: 1},
		{name: "copies across sources", messages: "(100,1,'meeting_transcript'),(101,2,'meeting_transcript'),(102,1,'meeting_transcript'),(103,2,'calendar_event'),(104,1,'meeting_transcript')", recipients: "(100,2),(101,2),(102,2),(103,2),(104,2)", keys: "(100,'uid:planning@example.org/'),(101,'uid:planning@example.org/'),(104,'uid:planning@example.org/')", sources: "(1,'owner@example.com','granola'),(2,'copy@example.org','twenty')", meetings: 2, sourceCounts: []int64{2, 1}},
		{name: "calendar only", messages: "(100,1,'calendar_event')", recipients: "(100,1),(100,2)", dailyOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			root, db := writeRelationshipBaseFixture(t, false)
			replaceRelationshipParquet(t, db, root, "messages", `
 SELECT id::BIGINT AS id, source_id::BIGINT AS source_id,
        'synthetic-' || id AS source_message_id, 10::BIGINT AS conversation_id,
        'Planning'::VARCHAR AS subject, ''::VARCHAR AS snippet,
        TIMESTAMP '2026-07-20 10:30:00' AS sent_at,
        50::BIGINT AS size_estimate, false AS has_attachments,
        0::INTEGER AS attachment_count, NULL::TIMESTAMP AS deleted_from_source_at,
        1::BIGINT AS sender_id, NULL::BIGINT AS owner_participant_id,
        message_type::VARCHAR AS message_type, false AS is_from_me,
        2026::INTEGER AS year, 7::INTEGER AS month
 FROM (VALUES `+tc.messages+`) t(id,source_id,message_type)`)
			if tc.sources != "" {
				replaceRelationshipParquet(t, db, root, "sources", `SELECT id::BIGINT AS id, account_email::VARCHAR AS account_email, source_type::VARCHAR AS source_type FROM (VALUES `+tc.sources+`) t(id,account_email,source_type)`)
			}
			replaceRelationshipParquet(t, db, root, "message_recipients", `SELECT message_id::BIGINT AS message_id, participant_id::BIGINT AS participant_id, 'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name FROM (VALUES `+tc.recipients+`) t(message_id,participant_id)`)
			if tc.keys != "" {
				replaceRelationshipParquet(t, db, root, "meeting_keys", `SELECT message_id::BIGINT AS message_id, meeting_key::VARCHAR AS meeting_key FROM (VALUES `+tc.keys+`) t(message_id,meeting_key)`)
			}
			_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
			requirements.NoError(err)
			if tc.dailyOnly {
				var units int64
				requirements.NoError(db.QueryRow(`SELECT coalesce(sum(meeting_units),0) FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetRelationshipDaily)).Scan(&units))
				assertions.Zero(units)
				return
			}
			var meetings, activities int64
			requirements.NoError(db.QueryRow(`SELECT meeting_count, activity_count FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&meetings, &activities))
			assertions.Equal(tc.meetings, meetings)
			if tc.activities > 0 {
				assertions.Equal(tc.activities, activities)
			}
			if len(tc.sourceCounts) > 0 {
				var first, second int64
				requirements.NoError(db.QueryRow(`SELECT (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=1),
 (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=2)
 FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&first, &second))
				assertions.Equal(tc.sourceCounts, []int64{first, second})
			}
		})
	}
}

func TestValidateRejectsMeetingTotalsOutsideSourceBounds(t *testing.T) {
	for _, count := range []int64{0, 100} {
		t.Run(strconv.FormatInt(count, 10), func(t *testing.T) {
			require := require.New(t)
			root, db := writeRelationshipBaseFixture(t, false)
			_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
			require.NoError(err)
			old := moveRelationshipDatasetAside(t, root, DatasetPeople)
			writeRelationshipParquet(t, db, root, DatasetPeople, fmt.Sprintf(`SELECT * REPLACE (
				%d::BIGINT AS meeting_count,
				list_transform(source_rollups, lambda item: struct_update(item, meeting_count := 1::BIGINT)) AS source_rollups
			) FROM read_parquet('%s')`, count, quoteSQLString(relationshipParquetGlob(old, DatasetPeople))))
			err = Validate(t.Context(), db, ValidationOptions{OutputRoot: root, RequiredOutputDatasets: RequiredDatasets})
			require.ErrorContains(err, "source rollups do not decompose identity totals")
		})
	}
}

func TestMeetingKeyChangesControlTemperatureReuse(t *testing.T) {
	requirements := require.New(t)
	committed, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, committed, syntheticRelationshipFanOutOptions{
		firstMessageID: 100, messageCount: 1, memberCount: 2, startDate: "2026-01-01",
		messageType: "meeting_transcript", conversationType: "meeting",
	})
	replaceRelationshipParquet(t, db, committed, "message_recipients", `SELECT 100::BIGINT AS message_id, participant_id::BIGINT AS participant_id, 'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name FROM (VALUES (1), (2)) t(participant_id)`)
	replaceRelationshipParquet(t, db, committed, "meeting_keys", `SELECT 100::BIGINT AS message_id, 'uid:first@example.org/'::VARCHAR AS meeting_key`)
	at := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: committed, OutputRoot: committed, EffectiveAt: at})
	requirements.NoError(err)
	for _, tc := range []struct {
		name, keys           string
		reuse, appendMessage bool
	}{
		{"unchanged", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR)`, true, false},
		{"key removed", "", false, false},
		{"new unrelated meeting", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR),(101::BIGINT, 'uid:second@example.org/'::VARCHAR)`, true, true},
		{"new copy of cached meeting", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR),(101::BIGINT, 'uid:first@example.org/'::VARCHAR)`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			staged := t.TempDir()
			keysSQL := `SELECT 0::BIGINT AS message_id, ''::VARCHAR AS meeting_key WHERE false`
			if tc.keys != "" {
				keysSQL = `SELECT * FROM (VALUES ` + tc.keys + `) t(message_id,meeting_key)`
			}
			replaceRelationshipParquet(t, db, staged, "meeting_keys", keysSQL)
			if tc.appendMessage {
				writeRelationshipParquet(t, db, staged, "person_display_names", `SELECT 0::BIGINT AS participant_id, 0::BIGINT AS person_id, NULL::VARCHAR AS display_name WHERE false`)
				writeSyntheticRelationshipFanOut(t, db, staged, syntheticRelationshipFanOutOptions{
					firstMessageID: 101, messageCount: 1, memberCount: 2, startDate: "2026-01-01",
					messageType: "meeting_transcript", conversationType: "meeting",
				})
				replaceRelationshipParquet(t, db, staged, "message_recipients", `SELECT 101::BIGINT AS message_id, participant_id::BIGINT AS participant_id, 'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name FROM (VALUES (1), (2)) t(participant_id)`)
			}
			b := builder{db: db, opts: BuildOptions{Mode: ModeIncremental, CommittedRoot: committed, StagedBaseRoot: staged}}
			reuse, err := b.canReuseTemperatureContributions(t.Context(), at)
			requirements.NoError(err)
			assert.Equal(t, tc.reuse, reuse)
			if tc.appendMessage && !tc.reuse {
				_, err = Build(t.Context(), db, BuildOptions{Mode: ModeIncremental, CommittedRoot: committed, StagedBaseRoot: staged, OutputRoot: staged, EffectiveAt: at})
				requirements.NoError(err)
				var count int64
				requirements.NoError(db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id = 2`, relationshipParquetGlob(staged, DatasetTemperatureContributions)).Scan(&count))
				assert.Equal(t, int64(1), count)
				requirements.NoError(db.QueryRow(`SELECT sum(meeting_units) FROM read_parquet(?) WHERE canonical_id = 2`, relationshipParquetGlob(staged, DatasetRelationshipDaily)).Scan(&count))
				assert.Equal(t, int64(1), count)
			}
		})
	}
}
