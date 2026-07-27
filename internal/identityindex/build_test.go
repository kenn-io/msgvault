package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/duckdbutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFactsEdgesAndDirectory(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.NotEmpty(t, result.ConversationParticipantsFingerprint)
	assert.Equal(t, int64(1), result.Stats.TotalMessages)
	assert.Equal(t, int64(1), result.Stats.Sources)
	assert.Equal(t, int64(1), result.Stats.UniqueSenders)
	assert.Equal(t, int64(1), result.Stats.UniqueDomains)
	assert.Equal(t, int64(50), result.Stats.TotalSizeBytes)
	assert.Equal(t, int64(12), result.Stats.AttachmentSizeBytes)
	require.NotNil(t, result.Stats.MinYear)
	require.NotNil(t, result.Stats.MaxYear)
	assert.Equal(t, int64(2026), *result.Stats.MinYear)
	assert.Equal(t, int64(2026), *result.Stats.MaxYear)

	assert.Equal(t, int64(1), parquetCount(t, db, root, DatasetEntryFacts))
	assert.Equal(t, int64(2), parquetCount(t, db, root, DatasetDirectEdges))
	assert.Equal(t, int64(1), parquetCount(t, db, root, DatasetConversationEdges))

	var authored, sender bool
	require.NoError(t, db.QueryRow(identityParquetSQL(root, DatasetDirectEdges)+
		" WHERE participant_id = 1").Scan(new(int64), new(int16), new(int64), new(string), &sender, &authored))
	assert.True(t, sender)
	assert.True(t, authored)

	var conversationID, conversationParticipantID int64
	require.NoError(t, db.QueryRow(identityParquetSQL(root, DatasetConversationEdges)).
		Scan(&conversationID, &conversationParticipantID, new(string)))
	assert.Equal(t, int64(10), conversationID)
	assert.Equal(t, int64(4), conversationParticipantID)

	var label, memberIDs, searchValues string
	var partial, owner bool
	require.NoError(t, db.QueryRow(`
		SELECT display_label, partial_label,
		       CAST(to_json(member_ids) AS VARCHAR),
		       CAST(to_json(search_values) AS VARCHAR),
		       is_owner
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, identityParquetGlob(root, DatasetDirectory)).
		Scan(&label, &partial, &memberIDs, &searchValues, &owner))
	assert.Equal(t, "Bob Alias", label)
	assert.False(t, partial)
	assert.JSONEq(t, `[2,3]`, memberIDs)
	assert.Contains(t, searchValues, "bob@example.net")
	assert.Contains(t, searchValues, "bob alias")
	assert.False(t, owner)
}

func TestBuildEmptySchemas(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, true)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	expected := map[string][]string{
		DatasetEntryFacts: {
			"message_id", "conversation_id", "source_id", "source_type",
			"occurred_at", "message_type", "conversation_type", "entry_kind",
			"is_chat", "is_from_me", "has_attachments", "attachment_count",
			"deleted_from_source", "occurred_year",
		},
		DatasetDirectEdges: {
			"message_id", "occurred_year", "participant_id",
			"participant_domain", "is_sender", "is_author",
		},
		DatasetConversationEdges: {
			"conversation_id", "participant_id", "participant_domain",
		},
		DatasetDirectory: {
			"canonical_id", "display_label", "partial_label",
			"member_ids", "search_values", "is_owner",
		},
		DatasetRollups: {
			"canonical_id", "activity_count", "file_count",
			"first_at", "last_at", "source_counts",
		},
		DatasetDomainRollups: {
			"domain", "activity_count", "person_count", "file_count",
			"first_at", "last_at", "source_counts",
		},
		DatasetRelationships: {
			"canonical_id", "anchor_date", "sent_decayed", "received_decayed",
			"meetings_decayed", "sent_count", "meeting_count",
			"modality_mask", "last_at",
		},
		DatasetRelationshipFuture: {
			"canonical_id", "event_date", "sent_units", "received_units",
			"meeting_units", "sent_count", "meeting_count",
			"modality_mask", "last_at",
		},
	}
	for dataset, wantColumns := range expected {
		assert.Equal(t, int64(0), parquetCount(t, db, root, dataset), dataset)
		t.Run(dataset, func(t *testing.T) {
			rows, queryErr := db.Query(identityParquetSQL(root, dataset) + " LIMIT 0")
			require.NoError(t, queryErr)
			defer func() {
				require.NoError(t, rows.Close())
			}()
			gotColumns, columnsErr := rows.Columns()
			require.NoError(t, columnsErr)
			require.NoError(t, rows.Err())
			assert.Equal(t, wantColumns, gotColumns)
		})
	}
}

func TestAuthoredAliasRollupReceivesOnceAndPreservesFutureSignals(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients"} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-30 17:45:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 2::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 3::BIGINT, 'to'::VARCHAR, 'Bob Alias'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	var relationshipCount, receivedUnits int64
	require.NoError(t, db.QueryRow(`
		SELECT count(*), coalesce(sum(f.received_units), 0)::BIGINT
		FROM read_parquet(?) r
		LEFT JOIN read_parquet(?) f USING (canonical_id)
	`, identityParquetGlob(root, DatasetRelationships),
		identityParquetGlob(root, DatasetRelationshipFuture)).
		Scan(&relationshipCount, &receivedUnits))
	assert.Equal(t, int64(1), relationshipCount)
	assert.Equal(t, int64(1), receivedUnits)

	var activityCount, fileCount int64
	require.NoError(t, db.QueryRow(`
		SELECT activity_count, file_count
		FROM read_parquet(?) WHERE canonical_id = 2
	`, identityParquetGlob(root, DatasetRollups)).Scan(&activityCount, &fileCount))
	assert.Equal(t, int64(1), activityCount)
	assert.Equal(t, int64(1), fileCount)

	var domainActivity, domainPeople int64
	require.NoError(t, db.QueryRow(`
		SELECT activity_count, person_count
		FROM read_parquet(?) WHERE domain = 'community.test'
	`, identityParquetGlob(root, DatasetDomainRollups)).
		Scan(&domainActivity, &domainPeople))
	assert.Equal(t, int64(1), domainActivity,
		"conversation membership contributes non-chat domain activity")
	assert.Equal(t, int64(0), domainPeople,
		"conversation-only non-chat members do not contribute people fan-out")
}

func TestLogicalChatReductionUsesNewestFilteredMessage(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients", "conversations"} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 ''::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 'sms'::VARCHAR, false, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 ''::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-20 10:30:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'sms'::VARCHAR, true, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	writeIdentityParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'direct_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	paths := ActivityPaths{
		Facts:             identityParquetGlob(root, DatasetEntryFacts),
		DirectEdges:       identityParquetGlob(root, DatasetDirectEdges),
		ConversationEdges: identityParquetGlob(root, DatasetConversationEdges),
		Directory:         identityParquetGlob(root, DatasetDirectory),
		Clusters:          identityParquetGlob(root, "participant_clusters"),
		Owners:            identityParquetGlob(root, "owner_participants"),
	}
	assertLogicalUnit := func(t *testing.T, filter string, wantID int64, wantFromMe bool) {
		t.Helper()
		var gotID int64
		var gotFromMe bool
		query := LogicalActivitySQL(paths, filter) +
			" SELECT anchor_message_id, is_from_me FROM logical_units"
		require.NoError(t, db.QueryRow(query).Scan(&gotID, &gotFromMe))
		assert.Equal(t, wantID, gotID)
		assert.Equal(t, wantFromMe, gotFromMe)
	}
	assertLogicalUnit(t, "true", 101, true)
	assertLogicalUnit(t, "f.message_id = 100", 100, false)

	var chatMemberActivity, chatMemberPeople int64
	require.NoError(t, db.QueryRow(`
		SELECT r.activity_count, d.person_count
		FROM read_parquet(?) r
		JOIN read_parquet(?) d ON d.domain = 'community.test'
		WHERE r.canonical_id = 4
	`, identityParquetGlob(root, DatasetRollups),
		identityParquetGlob(root, DatasetDomainRollups)).
		Scan(&chatMemberActivity, &chatMemberPeople))
	assert.Equal(t, int64(1), chatMemberActivity,
		"conversation membership contributes chat people fan-out")
	assert.Equal(t, int64(1), chatMemberPeople)
}

func TestFutureRelationshipRollupKeepsRawGateMaskAndTimestamp(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"messages", "message_recipients"} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Sent'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-28 09:00:00',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'email'::VARCHAR, true, 2026::INTEGER, 7::INTEGER),
			(101::BIGINT, 1::BIGINT, 'm-101'::VARCHAR, 10::BIGINT,
			 'Meeting'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-29 15:45:12',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 1::BIGINT, 'calendar_event'::VARCHAR, true, 2026::INTEGER, 7::INTEGER),
			(102::BIGINT, 1::BIGINT, 'm-102'::VARCHAR, 10::BIGINT,
			 'Owner absent'::VARCHAR, ''::VARCHAR, TIMESTAMP '2026-07-31 22:11:09',
			 10::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP,
			 2::BIGINT, 'meeting_note'::VARCHAR, false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Bob'::VARCHAR),
			(102::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Bob'::VARCHAR),
			(102::BIGINT, 4::BIGINT, 'to'::VARCHAR, 'Member'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	var canonicalID, sentCount, meetingCount int64
	var modalityMask uint8
	var lastAt time.Time
	require.NoError(t, db.QueryRow(`
		SELECT canonical_id, sent_count, meeting_count, modality_mask, last_at
		FROM read_parquet(?)
	`, identityParquetGlob(root, DatasetRelationships)).
		Scan(&canonicalID, &sentCount, &meetingCount, &modalityMask, &lastAt))
	assert.Equal(t, int64(2), canonicalID)
	assert.Equal(t, int64(1), sentCount)
	assert.Equal(t, int64(1), meetingCount)
	assert.Equal(t, ModalityEmail|ModalityMeeting, modalityMask)
	assert.Equal(t, time.Date(2026, 7, 29, 15, 45, 12, 0, time.UTC), lastAt)

	var futureRows, futureSent, futureMeetings int64
	var futureMask uint8
	var futureLastAt time.Time
	require.NoError(t, db.QueryRow(`
		SELECT count(*), sum(sent_units)::BIGINT, sum(meeting_units)::BIGINT,
		       bit_or(modality_mask)::UTINYINT, max(last_at)
		FROM read_parquet(?)
	`, identityParquetGlob(root, DatasetRelationshipFuture)).
		Scan(&futureRows, &futureSent, &futureMeetings, &futureMask, &futureLastAt))
	assert.Equal(t, int64(2), futureRows)
	assert.Equal(t, int64(1), futureSent)
	assert.Equal(t, int64(1), futureMeetings)
	assert.Equal(t, ModalityEmail|ModalityMeeting, futureMask)
	assert.Equal(t, lastAt, futureLastAt)
}

func TestValidateRejectsFutureIdentityWithoutRelationshipRollup(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	anchor := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     anchor,
	})
	require.NoError(t, err)

	oldRelationships := filepath.Join(root, DatasetRelationships+"-old")
	require.NoError(t, os.Rename(
		filepath.Join(root, DatasetRelationships),
		oldRelationships,
	))
	writeIdentityParquet(t, db, root, DatasetRelationships, `
		SELECT * FROM read_parquet('`+
		strings.ReplaceAll(filepath.Join(oldRelationships, "*.parquet"), "'", "''")+`')
		WHERE false`)

	err = Validate(context.Background(), db, ValidationOptions{
		OutputRoot:             root,
		RequiredOutputDatasets: RequiredDatasets,
		Activity: ActivityPaths{
			Facts:             identityParquetGlob(root, DatasetEntryFacts),
			DirectEdges:       identityParquetGlob(root, DatasetDirectEdges),
			ConversationEdges: identityParquetGlob(root, DatasetConversationEdges),
			Directory:         identityParquetGlob(root, DatasetDirectory),
			Clusters:          identityParquetGlob(root, "participant_clusters"),
			Owners:            identityParquetGlob(root, "owner_participants"),
		},
		Participants:  identityParquetGlob(root, "participants"),
		Conversations: identityParquetGlob(root, "conversations"),
		AnchorDate:    anchor,
	})
	require.ErrorContains(t, err,
		"validate relationship_future_daily: 1 future identities have no relationship rollup")
}

func TestBuildIncrementalEmitsDeltaAndComputesPostPublicationStats(t *testing.T) {
	committedRoot, db := writeIdentityBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	stagedRoot, stagedDB := writeIdentityBaseFixture(t, false)
	rewriteIdentityFixtureIDs(t, stagedDB, stagedRoot, 200, 20)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIncremental,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	assert.Equal(t, int64(1), parquetCount(t, db, stagedRoot, DatasetEntryFacts))
	assert.Equal(t, int64(2), parquetCount(t, db, stagedRoot, DatasetDirectEdges))
	var messageID int64
	require.NoError(t, db.QueryRow(identityParquetSQL(stagedRoot, DatasetEntryFacts)).
		Scan(&messageID, new(int64), new(int64), new(string), new(time.Time),
			new(string), new(string), new(string), new(bool), new(bool), new(bool),
			new(int32), new(bool), new(int16)))
	assert.Equal(t, int64(200), messageID)

	assert.Equal(t, int64(2), result.Stats.TotalMessages)
	assert.Equal(t, int64(1), result.Stats.Sources)
	assert.Equal(t, int64(100), result.Stats.TotalSizeBytes)
	assert.Equal(t, int64(24), result.Stats.AttachmentSizeBytes)
	assert.Equal(t, int64(1), parquetCount(t, db, committedRoot, DatasetEntryFacts))
}

func TestBuildDerivedOnlyUsesCommittedBaseAndStagedIdentityDimensions(t *testing.T) {
	committedRoot, db := writeIdentityBaseFixture(t, false)
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	stagedRoot := t.TempDir()
	writeIdentityParquet(t, db, stagedRoot, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT),
			(4::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeDerivedOnly,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
		AnchorDate:     time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	var memberIDs string
	require.NoError(t, db.QueryRow(`
		SELECT CAST(to_json(member_ids) AS VARCHAR)
		FROM read_parquet(?)
		WHERE canonical_id = 2
	`, identityParquetGlob(stagedRoot, DatasetDirectory)).Scan(&memberIDs))
	assert.JSONEq(t, `[2,3,4]`, memberIDs)
	assert.Equal(t, CacheStatsSummary{}, result.Stats,
		"derived refresh must preserve committed marker stats without rescanning raw Parquet")
	assert.NoDirExists(t, filepath.Join(stagedRoot, DatasetEntryFacts))
	assert.NoDirExists(t, filepath.Join(stagedRoot, DatasetDirectEdges))
	assert.NoDirExists(t, filepath.Join(stagedRoot, DatasetConversationEdges))
}

func TestBuildStatsPreserveMessageSourceAndSenderValueSemantics(t *testing.T) {
	root, db := writeIdentityBaseFixture(t, false)
	for _, dataset := range []string{"sources", "participants", "message_recipients"} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.com'::VARCHAR, 'gmail'::VARCHAR),
			(2::BIGINT, 'unused@example.net'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	writeIdentityParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'bob@example.net'::VARCHAR, 'example.net'::VARCHAR, ''::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'alias@example.net'::VARCHAR, 'example.net'::VARCHAR, 'Bob Alias'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@community.test'::VARCHAR, 'community.test'::VARCHAR, 'Member'::VARCHAR, ''::VARCHAR),
			(5::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice Alias'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 5::BIGINT, 'from'::VARCHAR, 'Alice Alias'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
		AnchorDate:     time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Stats.Sources)
	assert.Equal(t, int64(1), result.Stats.UniqueSenders)
	assert.Equal(t, int64(1), result.Stats.UniqueDomains)
}

func writeIdentityBaseFixture(t *testing.T, empty bool) (string, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	db, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(root, "duckdb-tmp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	where := ""
	if empty {
		where = " WHERE false"
	}
	writeIdentityParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'm-100'::VARCHAR, 10::BIGINT,
			 'Subject'::VARCHAR, 'Preview'::VARCHAR,
			 TIMESTAMP '2026-07-20 10:30:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 7::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`+where)
	writeIdentityParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.com'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`+where)
	writeIdentityParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'thread-10'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`+where)
	writeIdentityParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com'::VARCHAR, 'example.com'::VARCHAR, 'Alice'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'bob@example.net'::VARCHAR, 'example.net'::VARCHAR, ''::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'alias@example.net'::VARCHAR, 'example.net'::VARCHAR, 'Bob Alias'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@community.test'::VARCHAR, 'community.test'::VARCHAR, 'Member'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`+where)
	writeIdentityParquet(t, db, root, "participant_identifiers", `
		SELECT * FROM (VALUES
			(2::BIGINT, 'email'::VARCHAR, 'bob@example.net'::VARCHAR, 'Bob Home'::VARCHAR, true),
			(3::BIGINT, 'chat'::VARCHAR, 'bob-chat'::VARCHAR, 'Bob Chat'::VARCHAR, true)
		) AS t(participant_id, identifier_type, identifier_value, display_value, is_primary)`+where)
	writeIdentityParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`+where)
	writeIdentityParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`+where)
	writeIdentityParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`+where)
	writeIdentityParquet(t, db, root, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 2::BIGINT),
			(3::BIGINT, 2::BIGINT)
		) AS t(participant_id, canonical_id)`+where)
	writeIdentityParquet(t, db, root, "attachments", `
		SELECT * FROM (VALUES
			(1::BIGINT, 100::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`+where)
	return root, db
}

func rewriteIdentityFixtureIDs(
	t *testing.T,
	db *sql.DB,
	root string,
	messageID, conversationID int64,
) {
	t.Helper()
	for _, dataset := range []string{
		"messages",
		"conversations",
		"message_recipients",
		"conversation_participants",
		"attachments",
	} {
		require.NoError(t, os.RemoveAll(filepath.Join(root, dataset)))
	}
	writeIdentityParquet(t, db, root, "messages", fmt.Sprintf(`
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
	writeIdentityParquet(t, db, root, "conversations", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 'thread-%d'::VARCHAR, 'Thread'::VARCHAR, 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`,
		conversationID, conversationID))
	writeIdentityParquet(t, db, root, "message_recipients", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Alice'::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'to'::VARCHAR, ''::VARCHAR),
			(%d::BIGINT, 2::BIGINT, 'cc'::VARCHAR, ''::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`,
		messageID, messageID, messageID))
	writeIdentityParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(%d::BIGINT, 4::BIGINT)
		) AS t(conversation_id, participant_id)`,
		conversationID))
	writeIdentityParquet(t, db, root, "attachments", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(2::BIGINT, %d::BIGINT, 12::BIGINT, 'file.txt'::VARCHAR, 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`,
		messageID))
}

func writeIdentityParquet(t *testing.T, db *sql.DB, root, dataset, query string) {
	t.Helper()
	dir := filepath.Join(root, dataset)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, dataset+".parquet")
	_, err := db.Exec(fmt.Sprintf(
		"COPY (%s) TO '%s' (FORMAT PARQUET, COMPRESSION 'zstd')",
		query,
		strings.ReplaceAll(path, "'", "''"),
	))
	require.NoError(t, err, dataset)
}

func parquetCount(t *testing.T, db *sql.DB, root, dataset string) int64 {
	t.Helper()
	var count int64
	query := "SELECT count(*) FROM read_parquet(?)"
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		query = "SELECT count(*) FROM read_parquet(?, hive_partitioning=true, union_by_name=true)"
	}
	require.NoError(t, db.QueryRow(query, identityParquetGlob(root, dataset)).
		Scan(&count), dataset)
	return count
}

func identityParquetSQL(root, dataset string) string {
	options := ""
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		options = ", hive_partitioning=true, union_by_name=true"
	}
	return "SELECT * FROM read_parquet('" +
		strings.ReplaceAll(identityParquetGlob(root, dataset), "'", "''") + "'" +
		options + ")"
}

func identityParquetGlob(root, dataset string) string {
	if dataset == DatasetEntryFacts || dataset == DatasetDirectEdges {
		return filepath.Join(root, dataset, "**", "*.parquet")
	}
	return filepath.Join(root, dataset, "*.parquet")
}
