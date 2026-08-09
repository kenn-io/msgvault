package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stressRelationshipActivity100MEnv = "MSGVAULT_STRESS_RELATIONSHIP_ACTIVITY_100M"

func TestRelationshipActivityMatchesLegacyRowSet(t *testing.T) {
	root, db := writeRelationshipEquivalenceFixture(t)
	path := func(dataset string) string {
		return parquetDatasetGlob(root, dataset)
	}

	query := `
		WITH legacy AS (` + buildLegacyRelationshipActivitySQL(path) + `),
		production AS (` + buildRelationshipActivitySQL(path, 2026) + `)
		SELECT
			(SELECT count(*) FROM (SELECT * FROM legacy EXCEPT SELECT * FROM production)),
			(SELECT count(*) FROM (SELECT * FROM production EXCEPT SELECT * FROM legacy))`
	var legacyOnly, productionOnly int64
	require.NoError(t, db.QueryRow(query).Scan(&legacyOnly, &productionOnly))
	assert.Zero(t, legacyOnly)
	assert.Zero(t, productionOnly)
}

func TestRelationshipActivityYearQueryExcludesOffYearEdgesUnderLowMemory(t *testing.T) {
	type activityRow struct {
		messageID            int64
		conversationID       int64
		occurredYear         int64
		canonicalID          int64
		participantDomain    string
		isDirect             bool
		isConversationMember bool
		isSender             bool
		isAuthor             bool
		isOwner              bool
	}

	requirements := require.New(t)
	root, db := writeRelationshipBaseFixture(t, true)
	writeRelationshipYearScopeFixture(t, db, root)
	requirements.NoError(setRelationshipTestMemoryLimit(db, "128MB"))

	path := func(dataset string) string {
		return parquetDatasetGlob(root, dataset)
	}
	output := filepath.Join(t.TempDir(), "relationship-activity-2025.parquet")
	_, err := db.Exec(`COPY (` + buildRelationshipActivitySQL(path, 2025) + `) TO '` +
		quoteSQLString(output) + `' (FORMAT PARQUET)`)
	requirements.NoError(err)

	rows, err := db.Query(`
		SELECT message_id, conversation_id, occurred_year, canonical_id,
		       participant_domain, is_direct, is_conversation_member,
		       is_sender, is_author, is_owner
		FROM read_parquet(?)
		ORDER BY canonical_id`, output)
	requirements.NoError(err)
	t.Cleanup(func() {
		requirements.NoError(rows.Close())
	})

	var got []activityRow
	for rows.Next() {
		var row activityRow
		requirements.NoError(rows.Scan(
			&row.messageID,
			&row.conversationID,
			&row.occurredYear,
			&row.canonicalID,
			&row.participantDomain,
			&row.isDirect,
			&row.isConversationMember,
			&row.isSender,
			&row.isAuthor,
			&row.isOwner,
		))
		got = append(got, row)
	}
	requirements.NoError(rows.Err())
	assert.Equal(t, []activityRow{
		{
			messageID:         1,
			conversationID:    10,
			occurredYear:      2025,
			canonicalID:       1,
			participantDomain: "example.test",
			isDirect:          true,
			isSender:          true,
			isAuthor:          true,
			isOwner:           true,
		},
		{
			messageID:            1,
			conversationID:       10,
			occurredYear:         2025,
			canonicalID:          2,
			participantDomain:    "example.test",
			isConversationMember: true,
		},
	}, got)
}

func TestBuildStreamsRelationshipActivityUnderLowMemory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
		firstMessageID:   1,
		messageCount:     5_000,
		memberCount:      200,
		startDate:        "2026-01-01",
		messageType:      "email",
		conversationType: "email_thread",
	})
	requirements.NoError(setRelationshipTestMemoryLimit(db, "96MB"))

	path := func(dataset string) string {
		return parquetDatasetGlob(root, dataset)
	}
	legacyOutput := filepath.Join(t.TempDir(), "legacy.parquet")
	_, err := db.Exec(`COPY (` + buildLegacyRelationshipActivitySQL(path) + `) TO '` +
		quoteSQLString(legacyOutput) + `' (FORMAT PARQUET)`)
	requirements.Error(err)
	assertions.Contains(strings.ToLower(err.Error()), "out of memory")

	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)
	assertions.Equal(int64(1_000_000), result.Activity.FinalRows)
	assertions.Equal(int64(1_000_000), result.Activity.ConversationExpandedRows)
}

func TestBuildIncrementalAppendsIntervalOverFanOut(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	committedRoot, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, committedRoot, syntheticRelationshipFanOutOptions{
		firstMessageID:   1,
		messageCount:     4,
		memberCount:      5,
		startDate:        "2026-01-01",
		messageType:      "email",
		conversationType: "email_thread",
	})
	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: committedRoot,
		OutputRoot:     committedRoot,
	})
	requirements.NoError(err)

	stagedRoot := t.TempDir()
	writeSyntheticRelationshipFanOut(t, db, stagedRoot, syntheticRelationshipFanOutOptions{
		firstMessageID:   101,
		messageCount:     3,
		memberCount:      5,
		startDate:        "2026-02-01",
		messageType:      "email",
		conversationType: "email_thread",
	})
	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeIncremental,
		CommittedRoot:  committedRoot,
		StagedBaseRoot: stagedRoot,
		OutputRoot:     stagedRoot,
	})
	requirements.NoError(err)

	assertions.Equal(int64(15), relationshipParquetCount(
		t, db, stagedRoot, DatasetActivity,
	))
	assertions.Equal(int64(7), result.Activity.DirectRows)
	assertions.Equal(int64(35), result.Activity.ConversationExpandedRows)
	assertions.Equal(int64(35), result.Activity.FinalRows)
	assertions.Equal(int64(7), result.Stats.TotalMessages)

	var activityCount int64
	requirements.NoError(db.QueryRow(`
		SELECT activity_count
		FROM read_parquet(?)
		WHERE canonical_id = 1
	`, relationshipParquetGlob(stagedRoot, DatasetPeople)).Scan(&activityCount))
	assertions.Equal(int64(7), activityCount)
}

func TestLogicalChatReductionPreservesCanonicalAliasDomains(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, true)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'message-100'::VARCHAR, 10::BIGINT,
			 'Earlier message'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-01 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'chat'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER),
			(101::BIGINT, 1::BIGINT, 'message-101'::VARCHAR, 10::BIGINT,
			 'Newest message'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-02 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 2::BIGINT, 'chat'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@alpha.test'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'conversation-10'::VARCHAR,
			 'Synthetic group chat'::VARCHAR, 'group_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@alpha.test'::VARCHAR, 'alpha.test'::VARCHAR,
			 'Owner'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'alias@beta.test'::VARCHAR, 'beta.test'::VARCHAR,
			 'Owner Alias'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Owner'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'to'::VARCHAR, 'Owner Alias'::VARCHAR),
			(101::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Owner Alias'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Owner'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	replaceRelationshipParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 1::BIGINT),
			(10::BIGINT, 2::BIGINT)
		) AS t(conversation_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 1::BIGINT)
		) AS t(participant_id, canonical_id)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)

	query := logicalActivitySQL(
		relationshipParquetGlob(root, DatasetActivity),
		"f.source_id = ?",
	) + `
		SELECT
			(SELECT count(*) FROM logical_people),
			(SELECT canonical_id FROM logical_people),
			(SELECT anchor_message_id FROM logical_people),
			(SELECT is_author FROM logical_people),
			(SELECT is_owner FROM logical_people),
			(SELECT with_owner FROM logical_people),
			(SELECT CAST(to_json(list(struct_pack(
				canonical_id := canonical_id,
				domain := domain
			) ORDER BY canonical_id, domain)) AS VARCHAR)
			 FROM logical_person_domains),
			(SELECT CAST(to_json(list(domain ORDER BY domain)) AS VARCHAR)
			 FROM logical_domain_keys)`
	var peopleCount, canonicalID, anchorMessageID int64
	var isAuthor, isOwner, withOwner bool
	var personDomainsJSON, domainKeysJSON string
	requirements.NoError(db.QueryRow(query, int64(1)).Scan(
		&peopleCount,
		&canonicalID,
		&anchorMessageID,
		&isAuthor,
		&isOwner,
		&withOwner,
		&personDomainsJSON,
		&domainKeysJSON,
	))

	assertions.Equal(int64(1), peopleCount)
	assertions.Equal(int64(1), canonicalID)
	assertions.Equal(int64(101), anchorMessageID)
	assertions.True(isAuthor)
	assertions.True(isOwner)
	assertions.True(withOwner)
	assertions.JSONEq(`[
		{"canonical_id": 1, "domain": "alpha.test"},
		{"canonical_id": 1, "domain": "beta.test"}
	]`, personDomainsJSON)
	assertions.JSONEq(`["alpha.test", "beta.test"]`, domainKeysJSON)
}

func TestLogicalChatReductionKeepsEarlierDirectOnlyIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root, db := writeRelationshipBaseFixture(t, true)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'message-100'::VARCHAR, 10::BIGINT,
			 'Earlier direct message'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-01 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 2::BIGINT, 'chat'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER),
			(101::BIGINT, 1::BIGINT, 'message-101'::VARCHAR, 10::BIGINT,
			 'Newest member message'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-02 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'chat'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@alpha.test'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'conversation-10'::VARCHAR,
			 'Synthetic group chat'::VARCHAR, 'group_chat'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@alpha.test'::VARCHAR, 'alpha.test'::VARCHAR,
			 'Owner'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'guest@gamma.test'::VARCHAR, 'gamma.test'::VARCHAR,
			 'Earlier Guest'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Earlier Guest'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Owner'::VARCHAR),
			(101::BIGINT, 1::BIGINT, 'from'::VARCHAR, 'Owner'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	replaceRelationshipParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 1::BIGINT)
		) AS t(conversation_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`)

	_, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	requirements.NoError(err)

	query := logicalActivitySQL(
		relationshipParquetGlob(root, DatasetActivity),
		"true",
	) + `
		SELECT
			(SELECT CAST(to_json(list(struct_pack(
				canonical_id := canonical_id,
				anchor_message_id := anchor_message_id,
				is_author := is_author,
				is_owner := is_owner,
				with_owner := with_owner
			) ORDER BY canonical_id)) AS VARCHAR)
			 FROM logical_people),
			(SELECT CAST(to_json(list(struct_pack(
				canonical_id := canonical_id,
				domain := domain
			) ORDER BY canonical_id, domain)) AS VARCHAR)
			 FROM logical_person_domains),
			(SELECT CAST(to_json(list(domain ORDER BY domain)) AS VARCHAR)
			 FROM logical_domain_keys)`
	var peopleJSON, personDomainsJSON, domainKeysJSON string
	requirements.NoError(db.QueryRow(query).Scan(
		&peopleJSON,
		&personDomainsJSON,
		&domainKeysJSON,
	))

	assertions.JSONEq(`[
		{
			"canonical_id": 1,
			"anchor_message_id": 101,
			"is_author": true,
			"is_owner": true,
			"with_owner": true
		},
		{
			"canonical_id": 2,
			"anchor_message_id": 101,
			"is_author": false,
			"is_owner": false,
			"with_owner": true
		}
	]`, peopleJSON)
	assertions.JSONEq(`[
		{"canonical_id": 1, "domain": "alpha.test"},
		{"canonical_id": 2, "domain": "gamma.test"}
	]`, personDomainsJSON)
	assertions.JSONEq(`["alpha.test", "gamma.test"]`, domainKeysJSON)
}

func TestBuildRelationshipActivity100MillionEdgeAcceptance(t *testing.T) {
	if os.Getenv(stressRelationshipActivity100MEnv) != "1" {
		t.Skip("set " + stressRelationshipActivity100MEnv + "=1 to run")
	}

	root, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
		firstMessageID:   1,
		messageCount:     500_000,
		memberCount:      200,
		startDate:        "2026-01-01",
		messageType:      "chat",
		conversationType: "group_chat",
	})
	result, err := Build(context.Background(), db, BuildOptions{
		Mode:           ModeFull,
		StagedBaseRoot: root,
		OutputRoot:     root,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(100_000_000), result.Activity.ConversationExpandedRows)
	assert.Equal(t, int64(100_000_000), result.Activity.FinalRows)
}

func writeRelationshipEquivalenceFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	root, db := writeRelationshipBaseFixture(t, true)
	replaceRelationshipParquet(t, db, root, "messages", `
		SELECT * FROM (VALUES
			(100::BIGINT, 1::BIGINT, 'message-100'::VARCHAR, 10::BIGINT,
			 'Synthetic subject'::VARCHAR, 'Synthetic preview'::VARCHAR,
			 TIMESTAMP '2026-01-01 12:00:00', 50::BIGINT, true,
			 1::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER),
			(101::BIGINT, 1::BIGINT, 'message-101'::VARCHAR, 11::BIGINT,
			 'No valid edge'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-02 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER),
			(102::BIGINT, 1::BIGINT, 'message-102'::VARCHAR, 12::BIGINT,
			 'Unresolved conversation member'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2026-01-03 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2026::INTEGER, 1::INTEGER)
		) AS t(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)`)
	replaceRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.test'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'conversation-10'::VARCHAR, 'Synthetic thread'::VARCHAR,
			 'email_thread'::VARCHAR),
			(11::BIGINT, 'conversation-11'::VARCHAR, 'No valid edge'::VARCHAR,
			 'email_thread'::VARCHAR),
			(12::BIGINT, 'conversation-12'::VARCHAR, 'Unresolved member'::VARCHAR,
			 'email_thread'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.test'::VARCHAR, 'example.test'::VARCHAR,
			 'Owner'::VARCHAR, ''::VARCHAR),
			(2::BIGINT, 'owner-alias@example.test'::VARCHAR, 'example.test'::VARCHAR,
			 'Owner Alias'::VARCHAR, ''::VARCHAR),
			(3::BIGINT, 'direct@direct.test'::VARCHAR, 'direct.test'::VARCHAR,
			 'Direct'::VARCHAR, ''::VARCHAR),
			(4::BIGINT, 'member@member.test'::VARCHAR, 'member.test'::VARCHAR,
			 'Member'::VARCHAR, ''::VARCHAR)
		) AS t(id, email_address, domain, display_name, phone_number)`)
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `
		SELECT NULL::BIGINT AS participant_id,
		       NULL::VARCHAR AS identifier_type,
		       NULL::VARCHAR AS identifier_value,
		       NULL::VARCHAR AS display_value,
		       NULL::BOOLEAN AS is_primary
		WHERE false`)
	replaceRelationshipParquet(t, db, root, "message_recipients", `
		SELECT * FROM (VALUES
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Owner Alias'::VARCHAR),
			(100::BIGINT, 2::BIGINT, 'from'::VARCHAR, 'Owner Alias'::VARCHAR),
			(100::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Owner'::VARCHAR),
			(100::BIGINT, 3::BIGINT, 'to'::VARCHAR, 'Direct'::VARCHAR),
			(100::BIGINT, 999::BIGINT, 'cc'::VARCHAR, 'Missing'::VARCHAR),
			(101::BIGINT, 999::BIGINT, 'to'::VARCHAR, 'Missing'::VARCHAR)
		) AS t(message_id, participant_id, recipient_type, display_name)`)
	replaceRelationshipParquet(t, db, root, "conversation_participants", `
		SELECT * FROM (VALUES
			(10::BIGINT, 1::BIGINT),
			(10::BIGINT, 2::BIGINT),
			(10::BIGINT, 4::BIGINT),
			(11::BIGINT, 999::BIGINT),
			(12::BIGINT, 999::BIGINT)
		) AS t(conversation_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 2::BIGINT)
		) AS t(source_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `
		SELECT * FROM (VALUES
			(2::BIGINT, 1::BIGINT)
		) AS t(participant_id, canonical_id)`)
	replaceRelationshipParquet(t, db, root, "attachments", `
		SELECT * FROM (VALUES
			(1::BIGINT, 100::BIGINT, 12::BIGINT, 'synthetic.txt'::VARCHAR,
			 'text/plain'::VARCHAR)
		) AS t(attachment_id, message_id, size, filename, mime_type)`)
	return root, db
}

type syntheticRelationshipFanOutOptions struct {
	firstMessageID   int64
	messageCount     int64
	memberCount      int64
	startDate        string
	messageType      string
	conversationType string
}

func writeSyntheticRelationshipFanOut(
	t *testing.T,
	db *sql.DB,
	root string,
	opts syntheticRelationshipFanOutOptions,
) {
	t.Helper()
	lastMessageID := opts.firstMessageID + opts.messageCount
	replaceRelationshipParquet(t, db, root, "messages", fmt.Sprintf(`
		SELECT (i + %d)::BIGINT AS id,
		       1::BIGINT AS source_id,
		       ('message-' || (i + %d))::VARCHAR AS source_message_id,
		       10::BIGINT AS conversation_id,
		       'Synthetic subject'::VARCHAR AS subject,
		       ''::VARCHAR AS snippet,
		       (TIMESTAMP '%s' + i * INTERVAL '1 second')::TIMESTAMP AS sent_at,
		       10::BIGINT AS size_estimate,
		       false AS has_attachments,
		       0::INTEGER AS attachment_count,
		       NULL::TIMESTAMP AS deleted_from_source_at,
		       1::BIGINT AS sender_id,
		       '%s'::VARCHAR AS message_type,
		       false AS is_from_me,
		       year(TIMESTAMP '%s' + i * INTERVAL '1 second')::INTEGER AS year,
		       month(TIMESTAMP '%s' + i * INTERVAL '1 second')::INTEGER AS month
		FROM range(0, %d) AS t(i)`,
		opts.firstMessageID, opts.firstMessageID, opts.startDate,
		quoteSQLString(opts.messageType), opts.startDate, opts.startDate, opts.messageCount))
	replaceRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.test'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	replaceRelationshipParquet(t, db, root, "conversations", `
		SELECT * FROM (VALUES
			(10::BIGINT, 'conversation-10'::VARCHAR, 'Synthetic thread'::VARCHAR,
			 '`+quoteSQLString(opts.conversationType)+`'::VARCHAR)
		) AS t(id, source_conversation_id, title, conversation_type)`)
	replaceRelationshipParquet(t, db, root, "participants", fmt.Sprintf(`
		SELECT i::BIGINT AS id,
		       ('person-' || i || '@example.test')::VARCHAR AS email_address,
		       'example.test'::VARCHAR AS domain,
		       ('Person ' || i)::VARCHAR AS display_name,
		       ''::VARCHAR AS phone_number
		FROM range(1, %d) AS t(i)`, opts.memberCount+1))
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `
		SELECT NULL::BIGINT AS participant_id,
		       NULL::VARCHAR AS identifier_type,
		       NULL::VARCHAR AS identifier_value,
		       NULL::VARCHAR AS display_value,
		       NULL::BOOLEAN AS is_primary
		WHERE false`)
	replaceRelationshipParquet(t, db, root, "message_recipients", fmt.Sprintf(`
		SELECT message_id::BIGINT AS message_id,
		       participant_id::BIGINT AS participant_id,
		       recipient_type::VARCHAR AS recipient_type,
		       ''::VARCHAR AS display_name
		FROM range(%d, %d) AS messages(message_id)
		CROSS JOIN (VALUES (1, 'from'), (1, 'to')) AS edges(participant_id, recipient_type)`,
		opts.firstMessageID, lastMessageID))
	replaceRelationshipParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
		SELECT 10::BIGINT AS conversation_id, i::BIGINT AS participant_id
		FROM range(1, %d) AS t(i)`, opts.memberCount+1))
	replaceRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `
		SELECT NULL::BIGINT AS participant_id, NULL::BIGINT AS canonical_id
		WHERE false`)
	replaceRelationshipParquet(t, db, root, "attachments", `
		SELECT NULL::BIGINT AS attachment_id,
		       NULL::BIGINT AS message_id,
		       NULL::BIGINT AS size,
		       NULL::VARCHAR AS filename,
		       NULL::VARCHAR AS mime_type
		WHERE false`)
}

func writeRelationshipYearScopeFixture(t *testing.T, db *sql.DB, root string) {
	t.Helper()
	const (
		offYearMessages     = 200_000
		offYearParticipants = 100
	)

	replaceRelationshipParquet(t, db, root, "messages", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT, 'selected-message'::VARCHAR, 10::BIGINT,
			 'Selected year'::VARCHAR, ''::VARCHAR,
			 TIMESTAMP '2025-01-01 12:00:00', 10::BIGINT, false,
			 0::INTEGER, NULL::TIMESTAMP, 1::BIGINT, 'email'::VARCHAR,
			 false, 2025::INTEGER, 1::INTEGER)
		) AS selected(id, source_id, source_message_id, conversation_id, subject,
			snippet, sent_at, size_estimate, has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type, is_from_me, year, month)

		UNION ALL

		SELECT (1000 + i)::BIGINT AS id,
		       1::BIGINT AS source_id,
		       ('off-year-message-' || i)::VARCHAR AS source_message_id,
		       (1000 + i)::BIGINT AS conversation_id,
		       'Off year'::VARCHAR AS subject,
		       ''::VARCHAR AS snippet,
		       (TIMESTAMP '2024-01-01' + i * INTERVAL '1 second')::TIMESTAMP AS sent_at,
		       10::BIGINT AS size_estimate,
		       false AS has_attachments,
		       0::INTEGER AS attachment_count,
		       NULL::TIMESTAMP AS deleted_from_source_at,
		       NULL::BIGINT AS sender_id,
		       'email'::VARCHAR AS message_type,
		       false AS is_from_me,
		       2024::INTEGER AS year,
		       1::INTEGER AS month
		FROM range(0, %d) AS off_year(i)`, offYearMessages))
	replaceRelationshipParquet(t, db, root, "sources", `
		SELECT * FROM (VALUES
			(1::BIGINT, 'owner@example.test'::VARCHAR, 'gmail'::VARCHAR)
		) AS t(id, account_email, source_type)`)
	replaceRelationshipParquet(t, db, root, "conversations", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(10::BIGINT, 'selected-conversation'::VARCHAR,
			 'Selected year'::VARCHAR, 'email_thread'::VARCHAR)
		) AS selected(id, source_conversation_id, title, conversation_type)

		UNION ALL

		SELECT (1000 + i)::BIGINT AS id,
		       ('off-year-conversation-' || i)::VARCHAR AS source_conversation_id,
		       'Off year'::VARCHAR AS title,
		       'email_thread'::VARCHAR AS conversation_type
		FROM range(0, %d) AS off_year(i)`, offYearMessages))
	replaceRelationshipParquet(t, db, root, "participants", fmt.Sprintf(`
		SELECT i::BIGINT AS id,
		       ('person-' || i || '@example.test')::VARCHAR AS email_address,
		       'example.test'::VARCHAR AS domain,
		       ('Person ' || i)::VARCHAR AS display_name,
		       ''::VARCHAR AS phone_number
		FROM range(1, %d) AS participants(i)`, offYearParticipants+1))
	replaceRelationshipParquet(t, db, root, "participant_identifiers", `
		SELECT NULL::BIGINT AS participant_id,
		       NULL::VARCHAR AS identifier_type,
		       NULL::VARCHAR AS identifier_value,
		       NULL::VARCHAR AS display_value,
		       NULL::BOOLEAN AS is_primary
		WHERE false`)
	replaceRelationshipParquet(t, db, root, "message_recipients", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT, 'to'::VARCHAR, 'Person 1'::VARCHAR)
		) AS selected(message_id, participant_id, recipient_type, display_name)

		UNION ALL

		SELECT (1000 + message_index)::BIGINT AS message_id,
		       participant_id::BIGINT AS participant_id,
		       'to'::VARCHAR AS recipient_type,
		       ''::VARCHAR AS display_name
		FROM range(0, %d) AS messages(message_index)
		CROSS JOIN range(1, %d) AS participants(participant_id)`,
		offYearMessages, offYearParticipants+1))
	replaceRelationshipParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
		SELECT * FROM (VALUES
			(10::BIGINT, 2::BIGINT)
		) AS selected(conversation_id, participant_id)

		UNION ALL

		SELECT (1000 + conversation_index)::BIGINT AS conversation_id,
		       participant_id::BIGINT AS participant_id
		FROM range(0, %d) AS conversations(conversation_index)
		CROSS JOIN range(1, %d) AS participants(participant_id)`,
		offYearMessages, offYearParticipants+1))
	replaceRelationshipParquet(t, db, root, "owner_participants", `
		SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(source_id, participant_id)`)
	replaceRelationshipParquet(t, db, root, "participant_clusters", `
		SELECT NULL::BIGINT AS participant_id, NULL::BIGINT AS canonical_id
		WHERE false`)
	replaceRelationshipParquet(t, db, root, "attachments", `
		SELECT NULL::BIGINT AS attachment_id,
		       NULL::BIGINT AS message_id,
		       NULL::BIGINT AS size,
		       NULL::VARCHAR AS filename,
		       NULL::VARCHAR AS mime_type
		WHERE false`)
}

func setRelationshipTestMemoryLimit(db *sql.DB, limit string) error {
	_, err := db.Exec("SET memory_limit = '" + limit + "'")
	return err
}

func buildLegacyRelationshipActivitySQL(path func(string) string) string {
	return fmt.Sprintf(`
WITH message_facts AS (
	SELECT m.id::BIGINT AS message_id,
	       m.conversation_id::BIGINT AS conversation_id,
	       m.source_id::BIGINT AS source_id,
	       s.source_type::VARCHAR AS source_type,
	       m.sent_at::TIMESTAMP AS occurred_at,
	       m.message_type::VARCHAR AS message_type,
	       coalesce(c.conversation_type, '')::VARCHAR AS conversation_type,
	       %s AS entry_kind,
	       (%s) AS is_chat,
	       m.is_from_me::BOOLEAN AS is_from_me,
	       m.attachment_count::INTEGER AS attachment_count,
	       coalesce(m.has_attachments::BOOLEAN, false) AS has_attachments,
	       (m.deleted_from_source_at IS NOT NULL) AS deleted_from_source,
	       year(m.sent_at)::SMALLINT AS occurred_year,
	       m.sender_id::BIGINT AS sender_id
	FROM read_parquet('%s', hive_partitioning=true, union_by_name=true) m
	JOIN read_parquet('%s') s ON s.id = m.source_id
	LEFT JOIN read_parquet('%s') c ON c.id = m.conversation_id
), canon AS (
	SELECT p.id::BIGINT AS participant_id,
	       coalesce(c.canonical_id, p.id)::BIGINT AS canonical_id,
	       lower(coalesce(p.domain, ''))::VARCHAR AS participant_domain
	FROM read_parquet('%s') p
	LEFT JOIN read_parquet('%s') c ON c.participant_id = p.id
), owner_canon AS (
	SELECT DISTINCT c.canonical_id
	FROM read_parquet('%s') o
	JOIN canon c ON c.participant_id = o.participant_id
), raw_edges AS (
	SELECT mr.message_id::BIGINT AS message_id,
	       mr.participant_id::BIGINT AS participant_id,
	       true AS is_direct,
	       false AS is_conversation_member,
	       false AS is_sender,
	       (mr.recipient_type = 'from') AS is_author
	FROM read_parquet('%s') mr

	UNION ALL

	SELECT m.message_id, m.sender_id, true, false, true, true
	FROM message_facts m
	WHERE m.sender_id IS NOT NULL

	UNION ALL

	SELECT m.message_id, cp.participant_id::BIGINT,
	       false, true, false, false
	FROM message_facts m
	JOIN read_parquet('%s') cp
	  ON cp.conversation_id = m.conversation_id
)
SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
	   m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
	   m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
	   m.deleted_from_source,
	   c.canonical_id, c.participant_domain,
	   coalesce(bool_or(e.is_direct)
	       FILTER (WHERE c.canonical_id IS NOT NULL), false) AS is_direct,
	   coalesce(bool_or(e.is_conversation_member)
	       FILTER (WHERE c.canonical_id IS NOT NULL), false)
	       AS is_conversation_member,
	   coalesce(bool_or(e.is_sender)
	       FILTER (WHERE c.canonical_id IS NOT NULL), false) AS is_sender,
	   coalesce(bool_or(e.is_author)
	       FILTER (WHERE c.canonical_id IS NOT NULL), false) AS is_author,
	   (o.canonical_id IS NOT NULL) AS is_owner,
	   m.occurred_year
FROM message_facts m
LEFT JOIN raw_edges e USING (message_id)
LEFT JOIN canon c USING (participant_id)
LEFT JOIN owner_canon o USING (canonical_id)
GROUP BY m.message_id, m.conversation_id, m.source_id, m.source_type,
	     m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
	     m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
	     m.deleted_from_source,
	     c.canonical_id, c.participant_domain, o.canonical_id, m.occurred_year`,
		EntryKindSQL("m.message_type"),
		IsChatSQL("m.message_type", "coalesce(c.conversation_type, '')"),
		quoteSQLString(path("messages")),
		quoteSQLString(path("sources")),
		quoteSQLString(path("conversations")),
		quoteSQLString(path("participants")),
		quoteSQLString(path("participant_clusters")),
		quoteSQLString(path("owner_participants")),
		quoteSQLString(path("message_recipients")),
		quoteSQLString(path("conversation_participants")),
	)
}
