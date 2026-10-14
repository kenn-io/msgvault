package identityindex

import (
	"context"
	"database/sql"
	"math"
	"strconv"
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
        2026::INTEGER AS year, 7::INTEGER AS month, meeting_key::VARCHAR AS meeting_key
 FROM (VALUES
    (100,1,'meeting_transcript','ical:planning@example.org'),
    (101,2,'meeting_transcript','ical:planning@example.org'),
    (102,1,'meeting_transcript','msg:102'),
    (103,2,'calendar_event',NULL),
    (104,1,'meeting_transcript','ical:planning@example.org')
 ) t(id,source_id,message_type,meeting_key)`)
	replaceRelationshipParquet(t, db, root, "sources", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.com'::VARCHAR,'granola'::VARCHAR),
 (2::BIGINT,'copy@example.org'::VARCHAR,'twenty'::VARCHAR)
 ) t(id,account_email,source_type)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `SELECT id::BIGINT AS message_id,
  2::BIGINT AS participant_id,'to'::VARCHAR AS recipient_type
  FROM (VALUES(100),(101),(102),(103),(104)) t(id)`)
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

func TestSecondCopyArrivesIncrementally(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	committed, db := writeRelationshipBaseFixture(t, false)
	setCopy := func(root string, connection *sql.DB, source int64) {
		_, err := connection.Exec("CREATE OR REPLACE TEMP TABLE copy_messages AS SELECT * FROM read_parquet(?)", relationshipParquetGlob(root, "messages"))
		requirements.NoError(err)
		replaceRelationshipParquet(t, connection, root, "messages", `SELECT * REPLACE(
  'meeting_transcript'::VARCHAR AS message_type,'ical:planning@example.org'::VARCHAR AS meeting_key,
  `+strconv.FormatInt(source, 10)+`::BIGINT AS source_id) FROM copy_messages`)
		replaceRelationshipParquet(t, connection, root, "sources", `SELECT * FROM (VALUES
  (1::BIGINT,'first@example.com'::VARCHAR,'granola'::VARCHAR),
  (2::BIGINT,'second@example.org'::VARCHAR,'twenty'::VARCHAR)) t(id,account_email,source_type)`)
	}
	setCopy(committed, db, 1)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: committed, OutputRoot: committed})
	requirements.NoError(err)
	staged, stagedDB := writeRelationshipBaseFixture(t, false)
	rewriteRelationshipFixtureIDs(t, stagedDB, staged, 200, 20)
	setCopy(staged, stagedDB, 2)
	_, err = Build(t.Context(), db, BuildOptions{Mode: ModeIncremental, CommittedRoot: committed, StagedBaseRoot: staged, OutputRoot: staged})
	requirements.NoError(err)
	var meetings, activity int64
	requirements.NoError(db.QueryRow(`SELECT meeting_count,activity_count FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(staged, DatasetPeople)).Scan(&meetings, &activity))
	assertions.Equal(int64(1), meetings)
	assertions.Equal(int64(2), activity, "activity stays at transcript-message grain")
}

func TestRoomOnlyIdentityExcludedFromPersonPopulation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "participants", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Owner'::VARCHAR,''::VARCHAR),
 (2::BIGINT,'room@resource.calendar.google.com'::VARCHAR,'resource.calendar.google.com'::VARCHAR,'Example Room'::VARCHAR,''::VARCHAR),
 (3::BIGINT,'alias@resource.calendar.google.com'::VARCHAR,'resource.calendar.google.com'::VARCHAR,'Example Room Alias'::VARCHAR,''::VARCHAR),
 (4::BIGINT,'member@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Member'::VARCHAR,''::VARCHAR),
 (5::BIGINT,'first-human@example.org'::VARCHAR,'example.org'::VARCHAR,'First Example Human'::VARCHAR,''::VARCHAR),
 (6::BIGINT,'second-human@example.org'::VARCHAR,'example.org'::VARCHAR,'Second Example Human'::VARCHAR,''::VARCHAR)
 ) t(id,email_address,domain,display_name,phone_number)`)
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `SELECT 2::BIGINT AS participant_id,'email'::VARCHAR AS identifier_type,
 'room@resource.calendar.google.com'::VARCHAR AS identifier_value,'Example Room'::VARCHAR AS display_value,true AS is_primary`)
	writeRelationshipParquet(t, db, root, "room_addresses", `SELECT ''::VARCHAR AS email WHERE false`)
	writeRelationshipParquet(t, db, root, "person_contacts", `SELECT 0::BIGINT AS participant_id,''::VARCHAR AS address_kind,''::VARCHAR AS normalized_value WHERE false`)
	_, err := db.Exec(`CREATE TEMP TABLE room_messages AS SELECT * FROM read_parquet(?)`, relationshipParquetGlob(root, "messages"))
	requirements.NoError(err)
	replaceRelationshipParquet(t, db, root, "messages", `SELECT m.* REPLACE('meeting_transcript'::VARCHAR AS message_type,
 ('msg:' || (m.id+i))::VARCHAR AS meeting_key,(m.id+i)::BIGINT AS id) FROM room_messages m CROSS JOIN range(3) r(i)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `SELECT * FROM (VALUES
 (100::BIGINT,1::BIGINT,'from'::VARCHAR,''::VARCHAR),(101::BIGINT,1::BIGINT,'from'::VARCHAR,''::VARCHAR),(102::BIGINT,1::BIGINT,'from'::VARCHAR,''::VARCHAR),
 (100::BIGINT,2::BIGINT,'to'::VARCHAR,''::VARCHAR),(101::BIGINT,2::BIGINT,'to'::VARCHAR,''::VARCHAR),(102::BIGINT,2::BIGINT,'to'::VARCHAR,''::VARCHAR),
 (100::BIGINT,5::BIGINT,'to'::VARCHAR,''::VARCHAR),(100::BIGINT,6::BIGINT,'to'::VARCHAR,''::VARCHAR),(101::BIGINT,6::BIGINT,'to'::VARCHAR,''::VARCHAR)
 ) t(message_id,participant_id,recipient_type,display_name)`)
	replaceRelationshipParquet(t, db, root, "conversation_participants", `SELECT 0::BIGINT AS conversation_id,0::BIGINT AS participant_id WHERE false`)
	_, err = Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root, EffectiveAt: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)})
	requirements.NoError(err)
	var count int64
	requirements.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetRelationshipDaily)).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE canonical_id=2 AND relation_kind=1`, relationshipParquetGlob(root, DatasetLogicalContributions)).Scan(&count))
	assertions.Equal(int64(3), count, "contributions keep message-level evidence for rebuilds")
	var population, temperature int64
	var annualPopulation, annualTemperature int64
	requirements.NoError(db.QueryRow(`SELECT current_temperature_population,current_temperature,
 (SELECT item.population FROM unnest(annual_temperatures) a(item) WHERE item.year=2026),
 (SELECT item.temperature FROM unnest(annual_temperatures) a(item) WHERE item.year=2026)
 FROM read_parquet(?) WHERE canonical_id=6`, relationshipParquetGlob(root, DatasetPeople)).Scan(&population, &temperature, &annualPopulation, &annualTemperature))
	assertions.Equal(int64(2), population)
	assertions.Equal(int64(100), temperature, "room scores must not enter the percentile denominator")
	assertions.Equal(int64(2), annualPopulation)
	assertions.Equal(int64(100), annualTemperature)
	var signal float64
	var dailyUnits, retainedMeetings int64
	requirements.NoError(db.QueryRow(`SELECT current_meeting_signal FROM read_parquet(?) WHERE canonical_id=6`, relationshipParquetGlob(root, DatasetPeople)).Scan(&signal))
	assertions.InDelta(2*math.Exp(-RelationshipDecayRate), signal, 1e-9, "human message signals keep their original grain")
	requirements.NoError(db.QueryRow(`SELECT sum(meeting_units) FROM read_parquet(?) WHERE canonical_id=6`, relationshipParquetGlob(root, DatasetRelationshipDaily)).Scan(&dailyUnits))
	assertions.Equal(int64(2), dailyUnits)
	requirements.NoError(db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id=6`, relationshipParquetGlob(root, DatasetTemperatureContributions)).Scan(&retainedMeetings))
	assertions.Equal(int64(2), retainedMeetings)
}

func TestMeetingCountValidatorUsesRetainedKeyUnion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	root, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	requirements.NoError(err)
	_, err = db.Exec(`CREATE TEMP TABLE invalid_meeting_people AS SELECT * FROM read_parquet(?)`, relationshipParquetGlob(root, DatasetPeople))
	requirements.NoError(err)
	replaceRelationshipParquet(t, db, root, DatasetPeople, `SELECT * REPLACE(1::BIGINT AS meeting_count, list_transform(source_rollups, item -> struct_pack(source_id := item.source_id, source_type := item.source_type, activity_count := item.activity_count, meeting_count := 1::BIGINT, file_count := item.file_count, first_at := item.first_at, last_at := item.last_at)) AS source_rollups) FROM invalid_meeting_people`)
	err = Validate(t.Context(), db, ValidationOptions{OutputRoot: root})
	requirements.Error(err)
	assertions.Contains(err.Error(), "meeting totals disagree")
}

func TestMixedRoomCanonicalPreservesHumanSearchAndLabel(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "participants", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Owner'::VARCHAR,''::VARCHAR),
 (2::BIGINT,'room@resource.calendar.google.com'::VARCHAR,'resource.calendar.google.com'::VARCHAR,'Example Room'::VARCHAR,''::VARCHAR),
 (3::BIGINT,'human@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Human'::VARCHAR,''::VARCHAR)
 ) t(id,email_address,domain,display_name,phone_number)`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `SELECT * FROM (VALUES(2::BIGINT,2::BIGINT),(3::BIGINT,2::BIGINT)) t(participant_id,canonical_id)`)
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `SELECT * FROM (VALUES
 (2::BIGINT,'email'::VARCHAR,'room@resource.calendar.google.com'::VARCHAR,'Example Room'::VARCHAR,true),
 (3::BIGINT,'email'::VARCHAR,'human@example.org'::VARCHAR,'Example Human'::VARCHAR,true)
 ) t(participant_id,identifier_type,identifier_value,display_value,is_primary)`)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	requirements.NoError(err)
	var label, primitives, members string
	requirements.NoError(db.QueryRow(`SELECT display_label,CAST(to_json(search_primitives) AS VARCHAR),CAST(to_json(member_ids) AS VARCHAR) FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&label, &primitives, &members))
	assertions.Equal("Example Human", label)
	assertions.NotContains(primitives, "resource.calendar.google.com")
	assertions.NotContains(primitives, "Example Room")
	assertions.Contains(primitives, "human@example.org")
	assertions.JSONEq(`[2,3]`, members)
}

func TestRoomFilteringPreservesHumanCanonicalFallbackPolicy(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "participants", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Owner'::VARCHAR,''::VARCHAR),
 (2::BIGINT,'canonical@example.org'::VARCHAR,'example.org'::VARCHAR,''::VARCHAR,''::VARCHAR),
 (3::BIGINT,''::VARCHAR,''::VARCHAR,''::VARCHAR,'+12025550123'::VARCHAR)
 ) t(id,email_address,domain,display_name,phone_number)`)
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `SELECT 0::BIGINT AS participant_id,''::VARCHAR AS identifier_type,''::VARCHAR AS identifier_value,''::VARCHAR AS display_value,false AS is_primary WHERE false`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `SELECT * FROM (VALUES(2::BIGINT,2::BIGINT),(3::BIGINT,2::BIGINT)) t(participant_id,canonical_id)`)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	require.NoError(t, err)
	var label string
	require.NoError(t, db.QueryRow(`SELECT display_label FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&label))
	assert.Equal(t, "canonical@example.org", label, "human canonical identifiers keep their existing fallback priority")
}

func TestRoomOnlyClusterWithAddresslessMemberIsHidden(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "participants", `SELECT * FROM (VALUES
 (1::BIGINT,'owner@example.org'::VARCHAR,'example.org'::VARCHAR,'Example Owner'::VARCHAR,''::VARCHAR),
 (2::BIGINT,'room@resource.calendar.google.com'::VARCHAR,'resource.calendar.google.com'::VARCHAR,'Example Room'::VARCHAR,''::VARCHAR),
 (3::BIGINT,''::VARCHAR,''::VARCHAR,'Example Addressless Alias'::VARCHAR,''::VARCHAR)
 ) t(id,email_address,domain,display_name,phone_number)`)
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `SELECT 0::BIGINT AS participant_id,''::VARCHAR AS identifier_type,''::VARCHAR AS identifier_value,''::VARCHAR AS display_value,false AS is_primary WHERE false`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `SELECT * FROM (VALUES(2::BIGINT,2::BIGINT),(3::BIGINT,2::BIGINT)) t(participant_id,canonical_id)`)
	_, err := Build(t.Context(), db, BuildOptions{Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root})
	require.NoError(t, err)
	var count int64
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE canonical_id=2`, relationshipParquetGlob(root, DatasetPeople)).Scan(&count))
	assert.Zero(t, count)
}
