package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/duckdbutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPublishesFourRelationshipDatasets(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{
		DatasetActivity,
		DatasetPeople,
		DatasetDomains,
		DatasetRelationshipDaily,
	}, RequiredDatasets)
	for _, dataset := range RequiredDatasets {
		assert.DirExists(t, filepath.Join(root, dataset), dataset)
	}
	assert.FileExists(t, filepath.Join(
		root,
		DatasetActivity,
		"occurred_year=2026",
		"data_0.parquet",
	))
	for _, removed := range []string{
		DatasetEntryFacts,
		DatasetDirectEdges,
		DatasetConversationEdges,
		DatasetDirectory,
		DatasetRollups,
		DatasetDomainRollups,
		DatasetRelationships,
	} {
		assert.NoDirExists(t, filepath.Join(root, removed), removed)
	}

	assert.Equal(t, int64(2), result.Activity.DirectRows)
	assert.Equal(t, int64(1), result.Activity.ConversationExpandedRows)
	assert.Equal(t, int64(3), result.Activity.FinalRows)
	assert.InDelta(t, 1.5, result.Activity.ExpansionRatio, 0.0001)
	assert.Equal(t, int64(1), result.Stats.TotalMessages)
	assert.NotEmpty(t, result.ConversationParticipantsFingerprint)

	var direct, conversation, author, owner bool
	require.NoError(t, db.QueryRow(`
		SELECT is_direct, is_conversation_member, is_author, is_owner
		FROM read_parquet(?, hive_partitioning=true, union_by_name=true)
		WHERE canonical_id = 2
	`, relationshipParquetGlob(root, DatasetActivity)).
		Scan(&direct, &conversation, &author, &owner))
	assert.True(t, direct)
	assert.False(t, conversation)
	assert.False(t, author)
	assert.False(t, owner)

	require.NoError(t, db.QueryRow(`
		SELECT is_direct, is_conversation_member
		FROM read_parquet(?, hive_partitioning=true, union_by_name=true)
		WHERE canonical_id = 4
	`, relationshipParquetGlob(root, DatasetActivity)).
		Scan(&direct, &conversation))
	assert.False(t, direct)
	assert.True(t, conversation)

	assert.Equal(t, int64(2), relationshipParquetCount(t, db, root, DatasetPeople),
		"non-chat people fan-out excludes conversation-only members")
	assert.Equal(t, int64(3), relationshipParquetCount(t, db, root, DatasetDomains),
		"non-chat domain fan-out includes conversation-only domains")
}

func TestBuildEmptyArchivePublishesSchemaCorrectActivityShard(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, true)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(
		root,
		DatasetActivity,
		"occurred_year=0",
		"empty.parquet",
	))
	for _, dataset := range RequiredDatasets {
		assert.Zero(t, relationshipParquetCount(t, db, root, dataset), dataset)
	}
	expectedActivityColumns := []string{
		"message_id", "conversation_id", "source_id", "source_type",
		"occurred_at", "message_type", "conversation_type", "entry_kind",
		"is_chat", "is_from_me", "attachment_count", "deleted_from_source",
		"canonical_id", "participant_domain", "is_direct",
		"is_conversation_member", "is_sender", "is_author", "is_owner",
		"occurred_year",
	}
	rows, err := db.Query(
		"SELECT * FROM read_parquet(?, hive_partitioning=true, union_by_name=true) LIMIT 0",
		relationshipParquetGlob(root, DatasetActivity),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })
	columns, err := rows.Columns()
	require.NoError(t, err)
	require.NoError(t, rows.Err())
	assert.Equal(t, expectedActivityColumns, columns)
}

func TestBuildMergesAuthoredAliasBeforeIncomingCredit(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-20 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 2::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 3::BIGINT, 'to'::VARCHAR, 'Bob Alias'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)

	var received, activityCount, fileCount int64
	require.NoError(t, db.QueryRow(`
		SELECT received_units
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, relationshipParquetGlob(root, DatasetRelationshipDaily)).Scan(&received))
	assert.Equal(t, int64(1), received,
		"the canonical author receives one incoming unit, not zero or two")
	require.NoError(t, db.QueryRow(`
		SELECT activity_count, file_count
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, relationshipParquetGlob(root, DatasetPeople)).Scan(&activityCount, &fileCount))
	assert.Equal(t, int64(1), activityCount)
	assert.Equal(t, int64(1), fileCount)
}

func TestLogicalChatReductionUsesNewestFilteredMessage(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, false)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'group_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Earlier'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 'chat'::VARCHAR, false, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Later'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-21 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'chat'::VARCHAR, true, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)
	paths := ActivityPaths{Activity: relationshipParquetGlob(root, DatasetActivity)}
	assertUnit := func(t *testing.T, filter string, wantID int64, wantFromMe bool) {
		t.Helper()
		var id int64
		var fromMe bool
		query := LogicalActivitySQL(paths, filter) +
			" SELECT anchor_message_id, is_from_me FROM logical_units"
		require.NoError(t, db.QueryRow(query).Scan(&id, &fromMe))
		assert.Equal(t, wantID, id)
		assert.Equal(t, wantFromMe, fromMe)
	}
	assertUnit(t, "true", 101, true)
	assertUnit(t, "f.message_id = 100", 100, false)
}

func TestBuildIncrementalWritesActivityDeltaAndRebuildsCompactPopulation(t *testing.T) {
	committedRoot, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
	})
	require.NoError(t, err)

	stagedRoot, stagedDB := writeRelationshipBaseFixture(t, false)
	rewriteRelationshipFixtureIDs(t, stagedDB, stagedRoot, 200, 20)
	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIncremental,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(3), relationshipParquetCount(t, db, stagedRoot, DatasetActivity))
	assert.Equal(t, int64(2), result.Stats.TotalMessages)
	var activityCount int64
	require.NoError(t, db.QueryRow(`
		SELECT activity_count FROM read_parquet(?) WHERE canonical_id = 2
	`, relationshipParquetGlob(stagedRoot, DatasetPeople)).Scan(&activityCount))
	assert.Equal(t, int64(2), activityCount)
}

func TestBuildIndexOnlyUsesCommittedBaseWithStagedIdentityDimensions(t *testing.T) {
	committedRoot, db := writeRelationshipBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
	})
	require.NoError(t, err)

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
	require.NoError(t, err)

	var memberIDs string
	require.NoError(t, db.QueryRow(`
		SELECT CAST(to_json(member_ids) AS VARCHAR)
		FROM read_parquet(?) WHERE canonical_id = 2
	`, relationshipParquetGlob(stagedRoot, DatasetPeople)).Scan(&memberIDs))
	assert.JSONEq(t, `[2,3,4]`, memberIDs)
	assert.Equal(t, CacheStatsSummary{}, result.Stats)
	assert.Equal(t, int64(3), relationshipParquetCount(t, db, stagedRoot, DatasetActivity))
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
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`+where)
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
			 TIMESTAMP '2026-07-21 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`,
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
