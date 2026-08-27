package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/duckdbutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPublishesFourRelationshipDatasets(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, false)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)

	for _, dataset := range RequiredDatasets {
		assertions.DirExists(filepath.Join(root, dataset), dataset)
	}
	assertions.FileExists(filepath.Join(
		root,
		DatasetActivity,
		"occurred_year=2026",
		"data_0.parquet",
	))

	assertions.Equal(int64(2), result.Activity.DirectRows)
	assertions.Equal(int64(1), result.Activity.ConversationExpandedRows)
	assertions.Equal(int64(3), result.Activity.FinalRows)
	assertions.InDelta(1.5, result.Activity.ExpansionRatio, 0.0001)
	assertions.Equal(int64(1), result.Stats.TotalMessages)
	assertions.NotEmpty(result.ConversationParticipantsFingerprint)

	var direct, conversation, author, owner bool
	requirements.NoError(db.QueryRow(`
		SELECT is_direct, is_conversation_member, is_author, is_owner
		FROM read_parquet(?, hive_partitioning=true, union_by_name=true)
		WHERE canonical_id = 2
	`, relationshipParquetGlob(root, DatasetActivity)).
		Scan(&direct, &conversation, &author, &owner))
	assertions.True(direct)
	assertions.False(conversation)
	assertions.False(author)
	assertions.False(owner)

	requirements.NoError(db.QueryRow(`
		SELECT is_direct, is_conversation_member
		FROM read_parquet(?, hive_partitioning=true, union_by_name=true)
		WHERE canonical_id = 4
	`, relationshipParquetGlob(root, DatasetActivity)).
		Scan(&direct, &conversation))
	assertions.False(direct)
	assertions.True(conversation)

	assertions.Equal(int64(2), relationshipParquetCount(t, db, root, DatasetPeople),
		"non-chat people fan-out excludes conversation-only members")
	assertions.Equal(int64(3), relationshipParquetCount(t, db, root, DatasetDomains),
		"non-chat domain fan-out includes conversation-only domains")
}

func TestBuildChunksRelationshipActivityByOccurrenceYear(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Earlier'::VARCHAR, ''::VARCHAR, TIMESTAMP '2025-12-31 10:30:00',
			 50::BIGINT, true, 1::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, NULL::BIGINT, 'email'::VARCHAR, false, 2025::INTEGER, 12::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Later'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-01-01 10:30:00',
			 50::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, NULL::BIGINT, 'email'::VARCHAR, false, 2026::INTEGER, 1::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, owner_participant_id, message_type, is_from_me, year, month)`)

	recordingDB := &relationshipActivityCopyRecorder{sqlExecutor: db}
	activityProgressCalls := 0
	_, err := Build(context.Background(), recordingDB, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		Progress: func(dataset string, _ time.Duration) {
			if dataset == DatasetActivity {
				activityProgressCalls++
			}
		},
	})
	requirements.NoError(err)

	assertions.Equal(2, recordingDB.activityCopies)
	assertions.Equal(1, activityProgressCalls)
	for _, year := range []int64{2025, 2026} {
		shard := filepath.Join(
			root,
			DatasetActivity,
			fmt.Sprintf("occurred_year=%d", year),
			"data_0.parquet",
		)
		assertions.FileExists(shard)

		var minYear, maxYear int64
		requirements.NoError(db.QueryRow(`
			SELECT min(occurred_year), max(occurred_year)
			FROM read_parquet(?, hive_partitioning=false)
		`, shard).Scan(&minYear, &maxYear))
		assertions.Equal(year, minYear)
		assertions.Equal(year, maxYear)
	}
	assertions.Equal(int64(5), relationshipParquetCount(t, db, root, DatasetActivity))
}

func TestBuildEmptyArchivePublishesSchemaCorrectActivityShard(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, true)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)

	assertions.FileExists(filepath.Join(
		root,
		DatasetActivity,
		"occurred_year=0",
		"empty.parquet",
	))
	for _, dataset := range RequiredDatasets {
		assertions.Zero(relationshipParquetCount(t, db, root, dataset), dataset)
	}
}

func TestLogicalChatReductionKeepsParticipantlessNewestMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'group_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Earlier'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 1::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, NULL::BIGINT, 'chat'::VARCHAR, false, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Later without participants'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-07-21 10:30:00',
			 10::BIGINT, false, 2::INTEGER, NULL::TIMESTAMP,
			 NULL::BIGINT, NULL::BIGINT, 'chat'::VARCHAR, true, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, owner_participant_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	replaceRelationshipParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 1::BIGINT)
		) AS t(conversation_id, participant_id)
		WHERE false`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)

	var anchorMessageID int64
	var isFromMe bool
	var attachmentCount int64
	query := logicalActivitySQL(
		relationshipParquetGlob(root, DatasetActivity),
		"true",
	) + `
		SELECT anchor_message_id, is_from_me, attachment_count
		FROM logical_units`
	requirements.NoError(db.QueryRow(query).
		Scan(&anchorMessageID, &isFromMe, &attachmentCount))
	assertions.Equal(int64(101), anchorMessageID)
	assertions.True(isFromMe)
	assertions.Equal(int64(3), attachmentCount)

	var sentinelRows int64
	requirements.NoError(db.QueryRow(`
		SELECT count(*)
		FROM read_parquet(?, hive_partitioning=true, union_by_name=true)
		WHERE message_id = 101 AND canonical_id IS NULL
	`, relationshipParquetGlob(root, DatasetActivity)).Scan(&sentinelRows))
	assertions.Equal(int64(1), sentinelRows)
}

func TestBuildIncrementalWritesActivityDeltaAndRebuildsCompactPopulation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	committedRoot, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
	})
	requirements.NoError(err)

	stagedRoot, stagedDB := writeRelationshipBaseFixture(t, false)
	rewriteRelationshipFixtureIDs(t, stagedDB, stagedRoot, 200, 20)
	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIncremental,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
	})
	requirements.NoError(err)

	assertions.Equal(int64(3), relationshipParquetCount(t, db, stagedRoot, DatasetActivity))
	assertions.FileExists(filepath.Join(
		stagedRoot,
		DatasetActivity,
		"occurred_year=2027",
		"data_0.parquet",
	))
	assertions.Equal(int64(2), result.Stats.TotalMessages)
	var activityCount int64
	requirements.NoError(db.QueryRow(`
		SELECT activity_count FROM read_parquet(?) WHERE canonical_id = 2
	`, relationshipParquetGlob(stagedRoot, DatasetPeople)).Scan(&activityCount))
	assertions.Equal(int64(2), activityCount)
}

func TestBuildIndexOnlyUsesCommittedBaseWithStagedIdentityDimensions(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	committedRoot, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
	})
	requirements.NoError(err)

	stagedRoot := t.TempDir()
	writeRelationshipParquet(t, db, stagedRoot, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT),
			(4::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`)
	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIndexOnly,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
	})
	requirements.NoError(err)

	var memberIDs string
	requirements.NoError(db.QueryRow(`
		SELECT CAST(to_json(member_ids) AS VARCHAR)
		FROM read_parquet(?) WHERE canonical_id = 2
	`, relationshipParquetGlob(stagedRoot, DatasetPeople)).Scan(&memberIDs))
	assertions.JSONEq(`[2,3,4]`, memberIDs)
	assertions.Equal(CacheStatsSummary{}, result.Stats)
	assertions.Equal(int64(3), relationshipParquetCount(t, db, stagedRoot, DatasetActivity))
}

func TestBuildPeopleDirectoryPublishesTypedCompletionPrimitives(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root,
	})
	require.NoError(t, err)

	var primitivesJSON string
	err = db.QueryRow(`
		SELECT CAST(to_json(search_primitives) AS VARCHAR)
		FROM read_parquet(?) WHERE canonical_id = 2
	`, relationshipParquetGlob(root, DatasetPeople)).Scan(&primitivesJSON)
	require.NoError(t, err)
	assert.JSONEq(t, `[
		{"kind":"email","match_value":"bob@example.net","display_value":"bob@example.net","source":"observed","participant_id":2},
		{"kind":"email","match_value":"bob@example.net","display_value":"Bob Home","source":"email","participant_id":2},
		{"kind":"name","match_value":"bob alias","display_value":"Bob Alias","source":"observed","participant_id":3},
		{"kind":"email","match_value":"alias@example.net","display_value":"alias@example.net","source":"observed","participant_id":3},
		{"kind":"username","match_value":"bob-chat","display_value":"Bob Chat","source":"chat","participant_id":3}
	]`, primitivesJSON)
}

func TestBuildPeoplePublishesMessageGrainRelationshipTemperatures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Sent'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 1::BIGINT, 'email'::VARCHAR, true, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Received'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-21 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 1::BIGINT, 'email'::VARCHAR, false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, owner_participant_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Owner'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 3::BIGINT, 'cc'::VARCHAR, 'Bob Alias'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Owner'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	effectiveAt := time.Date(2026, time.July, 22, 12, 34, 56, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root,
		EffectiveAt: effectiveAt,
	})
	require.NoError(err)

	var current, peak, peakYear, scoreVersion, annualTemperature int
	var rank, population, annualRank, annualPopulation int64
	var rawScore, annualRaw, sentSignal, receivedVolume float64
	var effectiveDate, effectiveTimestamp time.Time
	err = db.QueryRow(`
		SELECT p.current_temperature, p.current_temperature_rank,
		       p.current_temperature_population, p.current_raw_score,
		       p.temperature_effective_date, p.temperature_effective_at,
		       p.temperature_score_version,
		       p.peak_temperature, p.peak_year,
		       annual.item.temperature, annual.item.rank, annual.item.population,
		       annual.item.raw_score, annual.item.sent_signal,
		       annual.item.received_volume
		FROM read_parquet(?) p,
		     unnest(p.annual_temperatures) AS annual(item)
		WHERE p.canonical_id = 2 AND annual.item.year = 2026
	`, relationshipParquetGlob(root, DatasetPeople)).Scan(
		&current, &rank, &population, &rawScore, &effectiveDate, &effectiveTimestamp, &scoreVersion,
		&peak, &peakYear, &annualTemperature, &annualRank, &annualPopulation,
		&annualRaw, &sentSignal, &receivedVolume,
	)
	require.NoError(err)
	assert.Equal(100, current)
	assert.Equal(int64(1), rank)
	assert.Equal(int64(1), population)
	currentSent := RelationshipDayWeight(2) * math.Log(2)
	currentReceived := RelationshipDayWeight(1)
	assert.InDelta(2*currentSent+math.Log1p(currentReceived), rawScore, 1e-9)
	assert.Equal(effectiveAt.Truncate(24*time.Hour), effectiveDate)
	assert.Equal(effectiveAt, effectiveTimestamp)
	assert.Equal(RelationshipScoreVersion, scoreVersion)
	assert.Equal(100, peak)
	assert.Equal(2026, peakYear)
	assert.Equal(100, annualTemperature)
	assert.Equal(int64(1), annualRank)
	assert.Equal(int64(1), annualPopulation)
	assert.InDelta(3*math.Log(2), annualRaw, 1e-9)
	assert.InDelta(math.Log(2), sentSignal, 1e-9,
		"linked aliases on one message must count once")
	assert.InDelta(1.0, receivedVolume, 0)
}

func TestValidateRejectsDuplicateActivityGrain(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)

	oldRoot := moveRelationshipDatasetAside(t, root, DatasetActivity)
	source := quoteSQLString(relationshipParquetGlob(oldRoot, DatasetActivity))
	writeRelationshipParquet(t, db, root, DatasetActivity, `
		SELECT * EXCLUDE (occurred_year), occurred_year
		FROM read_parquet('`+source+`', hive_partitioning=true, union_by_name=true)
		UNION ALL
		SELECT * EXCLUDE (occurred_year), occurred_year
		FROM read_parquet('`+source+`', hive_partitioning=true, union_by_name=true)`)

	err = Validate(context.Background(), db, ValidationOptions{
		OutputRoot:             root,
		RequiredOutputDatasets: RequiredDatasets,
	})
	require.ErrorContains(t, err, "duplicate message/canonical/domain keys")
}

func TestValidateRejectsInvalidRelationshipTemperatureSummary(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root,
	})
	require.NoError(t, err)

	oldRoot := moveRelationshipDatasetAside(t, root, DatasetPeople)
	source := quoteSQLString(relationshipParquetGlob(oldRoot, DatasetPeople))
	writeRelationshipParquet(t, db, root, DatasetPeople, `
		SELECT * REPLACE (101::INTEGER AS current_temperature)
		FROM read_parquet('`+source+`')`)

	err = Validate(context.Background(), db, ValidationOptions{
		OutputRoot:             root,
		RequiredOutputDatasets: RequiredDatasets,
	})
	require.ErrorContains(t, err, "invalid relationship temperature summary")
}

func writeRelationshipBaseFixture(t *testing.T, empty bool) (string, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	db, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(root, "duckdb-tmp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	where := ""
	if empty {
		where = " WHERE false"
	}
	writeRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-20 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, NULL::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, owner_participant_id, message_type, is_from_me, year, month)`+where)
	writeRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.com'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`+where)
	writeRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`+where)
	writeRelationshipParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'bob@example.net'::VARCHAR, 'example.net'::VARCHAR, ''::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'alias@example.net'::VARCHAR, 'example.net'::VARCHAR, 'Bob Alias'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@community.test'::VARCHAR, 'community.test'::VARCHAR, 'Member'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`+where)
	writeRelationshipParquet(t, db, root, "participant_identifiers", `
		SELECT * FROM (VALUES
			(2::BIGINT, 'email'::VARCHAR, 'bob@example.net'::VARCHAR, 'Bob Home'::VARCHAR, true),
			(3::BIGINT, 'chat'::VARCHAR, 'bob-chat'::VARCHAR, 'Bob Chat'::VARCHAR, true)
		) AS t(participant_id, identifier_type, identifier_value, display_value, is_primary)`+where)
	writeRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`+where)
	writeRelationshipParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`+where)
	writeRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`+where)
	writeRelationshipParquet(t, db, root, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`+where)
	writeRelationshipParquet(t, db, root, "attachments", `
		SELECT * FROM (VALUES
			(1::BIGINT, 100::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`+where)
	return root, db
}

func rewriteRelationshipFixtureIDs(
	t *testing.T,
	db *sql.DB,
	root string,
	messageID, conversationID int64,
) {
	t.Helper()
	replaceRelationshipParquet(t, db, root, "messages", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 1::BIGINT, 'm-%d'::VARCHAR, %d::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2027-07-21 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, NULL::BIGINT, 'email'::VARCHAR,
			 false, 2027::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, owner_participant_id, message_type, is_from_me, year, month)`,
		messageID, messageID, conversationID))
	replaceRelationshipParquet(t, db, root, "conversations", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 'thread-%d'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`,
		conversationID, conversationID))
	replaceRelationshipParquet(t, db, root, "message_recipients", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`,
		messageID, messageID, messageID))
	replaceRelationshipParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`,
		conversationID))
	replaceRelationshipParquet(t, db, root, "attachments", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(2::BIGINT, %d::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`,
		messageID))
}

type relationshipActivityCopyRecorder struct {
	sqlExecutor

	activityCopies int
}

func (r *relationshipActivityCopyRecorder) ExecContext(
	ctx context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	if strings.Contains(query, "PARTITION_BY (occurred_year)") {
		r.activityCopies++
	}
	return r.sqlExecutor.ExecContext(ctx, query, args...)
}

func replaceRelationshipParquet(
	t *testing.T,
	db *sql.DB,
	root, dataset, query string,
) {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	writeRelationshipParquet(t, db, root, dataset, query)
}

func writeRelationshipParquet(
	t *testing.T,
	db *sql.DB,
	root, dataset, query string,
) {
	t.Helper()
	directory := filepath.Join(root, dataset)
	require.NoError(t, os.MkdirAll(directory, 0o755))
	path := filepath.Join(directory, dataset+".parquet")
	_, err := db.Exec(fmt.Sprintf(
		"COPY (%s) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')",
		query,
		strings.ReplaceAll(path, "'", "''"),
	))
	require.NoError(t, err, dataset)
}

func relationshipParquetCount(
	t *testing.T,
	db *sql.DB,
	root, dataset string,
) int64 {
	t.Helper()
	query := "SELECT count(*) FROM read_parquet(?)"
	if dataset == DatasetActivity {
		query = "SELECT count(*) FROM read_parquet(?, hive_partitioning=true, union_by_name=true)"
	}
	var count int64
	require.NoError(t, db.QueryRow(query, relationshipParquetGlob(root, dataset)).
		Scan(&count), dataset)
	return count
}

func relationshipParquetGlob(root, dataset string) string {
	if dataset == DatasetActivity {
		return filepath.Join(root, dataset, "**", "*.parquet")
	}
	return filepath.Join(root, dataset, "*.parquet")
}

func moveRelationshipDatasetAside(t *testing.T, root, dataset string) string {
	t.Helper()
	oldRoot := t.TempDir()
	require.NoError(t, os.Rename(
		filepath.Join(root, dataset),
		filepath.Join(oldRoot, dataset),
	))
	return oldRoot
}
