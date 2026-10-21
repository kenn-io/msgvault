package identityindex

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMeetingCountDedupesCopiesGlobalAndSourceFiltered(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

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
 FROM (VALUES
    (100,1,'meeting_transcript'),
    (101,2,'meeting_transcript'),
    (102,1,'meeting_transcript'),
    (103,2,'calendar_event'),
    (104,1,'meeting_transcript')
 ) t(id,source_id,message_type)`)
	replaceRelationshipParquet(t, db, root, "sources", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.com'::VARCHAR,'granola'::VARCHAR),
 (2::BIGINT,'copy@example.org'::VARCHAR,'twenty'::VARCHAR)
 ) t(id,account_email,source_type)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `SELECT id::BIGINT AS message_id,
  2::BIGINT AS participant_id,'to'::VARCHAR AS recipient_type
  FROM (VALUES(100),(101),(102),(103),(104)) t(id)`)
	replaceRelationshipParquet(t, db, root, "meeting_keys", `SELECT * FROM (VALUES
 (100::BIGINT,'uid:planning@example.org/'::VARCHAR),
 (101::BIGINT,'uid:planning@example.org/'::VARCHAR),
 (104::BIGINT,'uid:planning@example.org/'::VARCHAR)) t(message_id,meeting_key)`)
	_, err := Build(context.Background(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	requirements.NoError(err)
	var count int64
	requirements.NoError(db.QueryRow(`SELECT meeting_count FROM read_parquet('` + root + `/relationship_people/*.parquet') WHERE canonical_id=2`).Scan(&count))
	assertions.Equal(int64(2), count)
	var first, second int64
	requirements.NoError(db.QueryRow(`SELECT (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=1),
 (SELECT item.meeting_count FROM unnest(source_rollups) r(item) WHERE item.source_id=2)
 FROM read_parquet('`+root+`/relationship_people/*.parquet') WHERE canonical_id=2`).Scan(&first, &second))
	assertions.Equal(int64(2), first)
	assertions.Equal(int64(1), second)
}

func TestMeetingContributionsDeduplicateAcrossAppend(t *testing.T) {
	require := require.New(t)
	committed, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, committed, syntheticRelationshipFanOutOptions{
		firstMessageID: 1, messageCount: 1, memberCount: 5, startDate: "2026-01-01",
		messageType: "meeting_transcript", conversationType: "meeting",
	})
	replaceRelationshipParquet(t, db, committed, "message_recipients", `SELECT 1::BIGINT AS message_id, participant_id::BIGINT AS participant_id, 'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name FROM (VALUES (1), (2)) t(participant_id)`)
	replaceRelationshipParquet(t, db, committed, "meeting_keys", `SELECT 1::BIGINT AS message_id, 'uid:shared@example.org/'::VARCHAR AS meeting_key`)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: committed, OutputRoot: committed})
	require.NoError(err)
	staged := t.TempDir()
	writeRelationshipParquet(t, db, staged, "person_display_names", `SELECT 0::BIGINT AS participant_id, 0::BIGINT AS person_id, NULL::VARCHAR AS display_name WHERE false`)
	writeRelationshipParquet(t, db, staged, "meeting_keys", `SELECT message_id::BIGINT AS message_id, 'uid:shared@example.org/'::VARCHAR AS meeting_key FROM (VALUES (1), (101)) t(message_id)`)
	writeSyntheticRelationshipFanOut(t, db, staged, syntheticRelationshipFanOutOptions{
		firstMessageID: 101, messageCount: 1, memberCount: 5, startDate: "2026-01-01",
		messageType: "meeting_transcript", conversationType: "meeting",
	})
	replaceRelationshipParquet(t, db, staged, "message_recipients", `SELECT 101::BIGINT AS message_id, participant_id::BIGINT AS participant_id, 'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name FROM (VALUES (1), (2)) t(participant_id)`)
	_, err = Build(t.Context(), db, BuildOptions{Mode: ModeIncremental, CommittedRoot: committed, StagedBaseRoot: staged, OutputRoot: staged})
	require.NoError(err)
	var count int64
	require.NoError(db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id = 2`, relationshipParquetGlob(staged, DatasetTemperatureContributions)).Scan(&count))
	assert.Equal(t, int64(1), count)
	require.NoError(db.QueryRow(`SELECT sum(meeting_units) FROM read_parquet(?) WHERE canonical_id = 2`, relationshipParquetGlob(staged, DatasetRelationshipDaily)).Scan(&count))
	assert.Equal(t, int64(1), count)
}

func TestValidateRejectsMeetingTotalBelowSourceCount(t *testing.T) {
	require := require.New(t)
	root, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	require.NoError(err)
	old := moveRelationshipDatasetAside(t, root, DatasetPeople)
	writeRelationshipParquet(t, db, root, DatasetPeople, `SELECT * REPLACE (
		0::BIGINT AS meeting_count,
		list_transform(source_rollups, item -> struct_update(item, meeting_count := 1::BIGINT)) AS source_rollups
	) FROM read_parquet('`+quoteSQLString(relationshipParquetGlob(old, DatasetPeople))+`')`)
	err = Validate(t.Context(), db, ValidationOptions{OutputRoot: root, RequiredOutputDatasets: RequiredDatasets})
	require.ErrorContains(err, "source rollups do not decompose identity totals")
}

func TestMeetingKeyChangesControlTemperatureReuse(t *testing.T) {
	require := require.New(t)
	committed, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, committed, syntheticRelationshipFanOutOptions{
		firstMessageID: 100, messageCount: 1, memberCount: 2, startDate: "2026-01-01",
		messageType: "meeting_transcript", conversationType: "meeting",
	})
	replaceRelationshipParquet(t, db, committed, "meeting_keys", `SELECT 100::BIGINT AS message_id, 'uid:first@example.org/'::VARCHAR AS meeting_key`)
	at := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: committed, OutputRoot: committed, EffectiveAt: at})
	require.NoError(err)
	staged := t.TempDir()
	for _, tc := range []struct {
		name, keys string
		reuse      bool
	}{
		{"unchanged", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR)`, true},
		{"late link", `(100::BIGINT, 'uid:second@example.org/'::VARCHAR)`, false},
		{"split", `(101::BIGINT, 'uid:second@example.org/'::VARCHAR)`, false},
		{"new unrelated meeting", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR),(101::BIGINT, 'uid:second@example.org/'::VARCHAR)`, true},
		{"new copy of cached meeting", `(100::BIGINT, 'uid:first@example.org/'::VARCHAR),(101::BIGINT, 'uid:first@example.org/'::VARCHAR)`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replaceRelationshipParquet(t, db, staged, "meeting_keys", `SELECT * FROM (VALUES `+tc.keys+`) t(message_id,meeting_key)`)
			b := builder{db: db, opts: BuildOptions{Mode: ModeIncremental, CommittedRoot: committed, StagedBaseRoot: staged}}
			reuse, err := b.canReuseTemperatureContributions(t.Context(), at)
			require.NoError(err)
			assert.Equal(t, tc.reuse, reuse)
		})
	}
}
