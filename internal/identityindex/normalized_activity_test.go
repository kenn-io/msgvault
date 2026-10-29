package identityindex

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildActivityPreservesFullExpansionContributions(t *testing.T) {
	for _, tc := range []struct {
		name, messageType, conversationType      string
		members                                  int64
		missingOwner, ownerAlias, mixed, unknown bool
	}{
		{name: "large group", messageType: "whatsapp", conversationType: "group_chat", members: 200, unknown: true},
		{name: "direct chat", messageType: "imessage", conversationType: "direct_chat", members: 3},
		{name: "owner absent", messageType: "beeper", conversationType: "group_chat", members: 20, missingOwner: true},
		{name: "owner alias", messageType: "beeper", conversationType: "group_chat", members: 20, ownerAlias: true},
		{name: "mixed modalities", messageType: "beeper", conversationType: "group_chat", members: 20, mixed: true, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			root, db := writeRelationshipBaseFixture(t, true)
			writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
				firstMessageID: 1, messageCount: 20, memberCount: tc.members,
				startDate: "2026-01-01 00:00:00", messageType: tc.messageType, conversationType: tc.conversationType,
			})
			messageType := "message_type"
			if tc.mixed {
				messageType = "CASE WHEN id % 5 = 0 THEN 'email' WHEN id % 7 = 0 THEN 'meeting_transcript' WHEN id % 6 = 0 THEN 'calendar_event' ELSE message_type END"
			}
			_, err := db.Exec(`CREATE TEMP TABLE equivalence_messages AS
				SELECT * REPLACE (
					CASE WHEN id % 4 = 0 THEN NULL ELSE 2 END::BIGINT AS sender_id,
					(id % 3 = 0) AS is_from_me,
					(1 + id % 2)::BIGINT AS source_id,
					(TIMESTAMP '2026-01-01' + (id // 4) * INTERVAL '1 second') AS sent_at,
					(id % 3)::INTEGER AS attachment_count,
					`+messageType+` AS message_type
				) FROM read_parquet(?)`, parquetDatasetGlob(root, "messages"))
			requirements.NoError(err)
			replaceRelationshipParquet(t, db, root, "messages", "SELECT * FROM equivalence_messages")
			replaceRelationshipParquet(t, db, root, "sources", `
				SELECT i::BIGINT AS id, 'owner@example.test' AS account_email, 'beeper' AS source_type FROM range(1, 3) t(i)`)
			unknownRecipient, unknownMember := "", ""
			if tc.unknown {
				unknownRecipient = " UNION ALL SELECT 1, 999999, 'to', ''"
				unknownMember = " UNION ALL SELECT 10, 999999"
			}
			replaceRelationshipParquet(t, db, root, "message_recipients", `
				SELECT id AS message_id, 3::BIGINT AS participant_id,
				       'to'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name
				FROM equivalence_messages WHERE id % 5 = 0 AND id < 19`+unknownRecipient)
			_, err = db.Exec(`CREATE TEMP TABLE aliased_participants AS
				SELECT * FROM read_parquet(?) UNION ALL
				SELECT ?, 'alias@alias.test', 'alias.test', 'Alias', ''`,
				parquetDatasetGlob(root, "participants"), tc.members+1)
			requirements.NoError(err)
			replaceRelationshipParquet(t, db, root, "participants", "SELECT * FROM aliased_participants")
			canonical := 2
			if tc.ownerAlias {
				canonical = 1
			}
			replaceRelationshipParquet(t, db, root, "participant_clusters", fmt.Sprintf(`
				SELECT %d::BIGINT AS participant_id, %d::BIGINT AS canonical_id`, tc.members+1, canonical))
			replaceRelationshipParquet(t, db, root, "conversation_participants", fmt.Sprintf(`
				SELECT 10::BIGINT AS conversation_id, i::BIGINT AS participant_id FROM range(1, %d) t(i)`, tc.members+2)+unknownMember)
			if tc.missingOwner {
				replaceRelationshipParquet(t, db, root, "owner_participants", `
					SELECT NULL::BIGINT AS source_id, NULL::BIGINT AS participant_id WHERE false`)
			}
			path := func(dataset string) string { return parquetDatasetGlob(root, dataset) }
			_, err = db.Exec("CREATE TEMP TABLE sparse AS " + buildSparseRelationshipActivitySQL(
				path, readParquetRelation([]string{path("messages")}, true), readParquetRelation([]string{path("message_recipients")}, false), 2026, ""))
			requirements.NoError(err)
			base := func(dataset string) string { return readParquetRelation([]string{path(dataset)}, false) }
			full := ExpandedActivityRelation("sparse", base("conversation_participants"),
				base("participants"), base("participant_clusters"), base("owner_participants"))
			bounded := buildActivityRelation("sparse", base("conversation_participants"),
				base("participants"), base("participant_clusters"), base("owner_participants"))
			if tc.mixed {
				var meetingFacts, earlierRosterAttendeeFacts int64
				facts := RelationshipTemperatureFactsSQL(bounded, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
				requirements.NoError(db.QueryRow(`SELECT count(*) FILTER (WHERE meeting),
					count(*) FILTER (WHERE meeting AND canonical_id = 4 AND message_id < 19)
					FROM (`+facts+`)`).Scan(&meetingFacts, &earlierRosterAttendeeFacts))
				requirements.Positive(meetingFacts)
				requirements.Positive(earlierRosterAttendeeFacts, "roster-only attendees receive credit before the chat anchor")
			}
			for _, reduction := range []struct {
				name string
				sql  func(string) string
			}{
				{"logical", buildLogicalActivityMaterializationSQL},
				{"temperature", func(activity string) string {
					return RelationshipTemperatureFactsSQL(activity, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
				}},
			} {
				t.Run(reduction.name, func(t *testing.T) {
					_, err := db.ExecContext(context.Background(), "CREATE TEMP TABLE full_"+reduction.name+" AS "+reduction.sql(full))
					require.NoError(t, err)
					var differences int64
					err = db.QueryRow(`WITH bounded AS (` + reduction.sql(bounded) + `)
						SELECT count(*) FROM (
							(SELECT * FROM bounded EXCEPT ALL SELECT * FROM full_` + reduction.name + `)
							UNION ALL
							(SELECT * FROM full_` + reduction.name + ` EXCEPT ALL SELECT * FROM bounded)
						)`).Scan(&differences)
					require.NoError(t, err)
					assert.Zero(t, differences)
				})
			}
		})
	}
}

func FuzzGroupBuildExpansionBounded(f *testing.F) {
	f.Add(uint16(0), uint16(0), false)
	f.Add(uint16(1), uint16(1), true)
	f.Add(uint16(32), uint16(200), false)
	f.Add(uint16(65535), uint16(65535), true)
	f.Fuzz(func(t *testing.T, messages, members uint16, ownerPresent bool) {
		requirements := require.New(t)
		// Draw the full input domain; bound only the materialized SQL fixture.
		messageCount := min(int64(messages), 32)
		memberCount := 2 + min(int64(members), 256)
		root, db := writeRelationshipBaseFixture(t, true)
		writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
			firstMessageID: 1, messageCount: messageCount, memberCount: memberCount,
			startDate: "2026-01-01", messageType: "beeper", conversationType: "group_chat",
		})
		_, err := db.Exec(`CREATE TEMP TABLE generated_messages AS
			SELECT * REPLACE (2::BIGINT AS sender_id) FROM read_parquet(?)`, parquetDatasetGlob(root, "messages"))
		requirements.NoError(err)
		replaceRelationshipParquet(t, db, root, "messages", "SELECT * FROM generated_messages")
		replaceRelationshipParquet(t, db, root, "message_recipients", `
			SELECT id AS message_id, 2::BIGINT AS participant_id,
			       'from'::VARCHAR AS recipient_type, ''::VARCHAR AS display_name
			FROM generated_messages`)
		if !ownerPresent {
			replaceRelationshipParquet(t, db, root, "owner_participants", `
				SELECT NULL::BIGINT AS source_id, NULL::BIGINT AS participant_id WHERE false`)
		}
		path := func(dataset string) string { return parquetDatasetGlob(root, dataset) }
		_, err = db.Exec("CREATE TEMP TABLE sparse AS " + buildSparseRelationshipActivitySQL(
			path, readParquetRelation([]string{path("messages")}, true), readParquetRelation([]string{path("message_recipients")}, false), 2026, ""))
		requirements.NoError(err)
		base := func(dataset string) string { return readParquetRelation([]string{path(dataset)}, false) }
		bounded := buildActivityRelation("sparse", base("conversation_participants"),
			base("participants"), base("participant_clusters"), base("owner_participants"))
		var rows int64
		requirements.NoError(db.QueryRow("SELECT count(*) FROM " + bounded).Scan(&rows))
		var want int64
		if messageCount > 0 {
			want = messageCount + memberCount - 1
			if ownerPresent {
				want = 2*messageCount + memberCount - 2
			}
		}
		assert.Equal(t, want, rows, "one roster per conversation plus per-message direct/owner edges")
	})
}

func TestExpandedActivityFiltersReachParquet(t *testing.T) {
	root, db := writeRelationshipBaseFixture(t, true)
	writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
		firstMessageID: 1, messageCount: 20_000, memberCount: 100,
		startDate: "2023-12-31 22:00:00", messageType: "whatsapp", conversationType: "group_chat",
	})
	path := func(dataset string) string { return parquetDatasetGlob(root, dataset) }
	writeRelationshipParquet(t, db, root, DatasetActivity, "("+buildSparseRelationshipActivitySQL(path, readParquetRelation([]string{path("messages")}, true), readParquetRelation([]string{path("message_recipients")}, false), 2023, "")+") UNION ALL ("+buildSparseRelationshipActivitySQL(path, readParquetRelation([]string{path("messages")}, true), readParquetRelation([]string{path("message_recipients")}, false), 2024, "")+")")
	relation := ExpandedActivityRelation(
		readParquetRelation([]string{filepath.Join(root, DatasetActivity, "*.parquet")}, false),
		readParquetRelation([]string{path("conversation_participants")}, false),
		readParquetRelation([]string{path("participants")}, false),
		readParquetRelation([]string{path("participant_clusters")}, false),
		readParquetRelation([]string{path("owner_participants")}, false),
	)
	for _, tc := range []struct {
		predicate string
		filter    string
		want      int64
	}{
		{"occurred_year = 2024", "occurred_year=2024", 1_280_000},
		{"message_id = 7", "message_id=7", 100},
		{"occurred_year = 2024 AND canonical_id = 5", "occurred_year=2024", 12_800},
	} {
		t.Run(tc.predicate, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var kind, plan string
			requirements.NoError(db.QueryRow("EXPLAIN (FORMAT JSON) SELECT * FROM "+relation+" WHERE "+tc.predicate).Scan(&kind, &plan))
			type node struct {
				Name      string         `json:"name"`
				ExtraInfo map[string]any `json:"extra_info"`
				Children  []node         `json:"children"`
			}
			var nodes []node
			requirements.NoError(json.Unmarshal([]byte(plan), &nodes))
			// Inspect scans carrying message payload. Membership and existence
			// joins may separately read narrow key columns. With a person filter,
			// the orphan branch disappears and all activity scans should be scoped.
			scans := 0
			var visit func([]node)
			visit = func(nodes []node) {
				for _, n := range nodes {
					projections := fmt.Sprint(n.ExtraInfo["Projections"])
					if strings.Contains(n.Name, "PARQUET") &&
						strings.Contains(projections, "message_id") &&
						(strings.Contains(projections, "source_type") || strings.Contains(tc.predicate, "canonical_id")) {
						scans++
						assert.Contains(t, strings.ReplaceAll(fmt.Sprint(n.ExtraInfo["Filters"]), " ", ""), tc.filter)
					}
					visit(n.Children)
				}
			}
			visit(nodes)
			assertions.Positive(scans)
			var count int64
			requirements.NoError(db.QueryRow("SELECT count(*) FROM " + relation + " WHERE " + tc.predicate).Scan(&count))
			assertions.Equal(tc.want, count)
		})
	}
}

func TestBuildUnsentTailKeepsSentAnchor(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, messageType, conversationType, recipientFilter string
		draftOnlyRecipient                                               bool
	}{
		{"mms", "synctech_sms", "mms", "direct_chat", "true", false},
		{"email", "gmail", "email", "email_thread", "true", true},
		{"group_chat", "beeper", "beeper", "group_chat", "participant_id <> 3", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			root, db := writeRelationshipBaseFixture(t, true)
			writeSyntheticRelationshipFanOut(t, db, root, syntheticRelationshipFanOutOptions{
				firstMessageID: 1, messageCount: 2, memberCount: 3,
				startDate: "2026-07-01", messageType: tc.messageType, conversationType: tc.conversationType,
			})
			// The group-chat member with ID 3 appears only in the conversation roster.
			replaceRelationshipParquet(t, db, root, "sources", fmt.Sprintf(`
				SELECT 1::BIGINT AS id, 'owner@example.test'::VARCHAR AS account_email, '%s'::VARCHAR AS source_type`, tc.sourceType))
			_, err := db.Exec(`CREATE TEMP TABLE messages_with_draft AS SELECT * REPLACE (
				CASE WHEN id = 1 THEN TIMESTAMP '2026-07-01' ELSE TIMESTAMP '2026-07-20' END AS sent_at,
				CASE WHEN id = 1 THEN 2::BIGINT ELSE 1::BIGINT END AS sender_id,
				(id = 2) AS is_from_me
			) FROM read_parquet(?)`, parquetDatasetGlob(root, "messages"))
			require.NoError(err)
			replaceRelationshipParquet(t, db, root, "messages", "SELECT * FROM messages_with_draft")
			replaceRelationshipParquet(t, db, root, "message_recipients", `
				SELECT * FROM (VALUES
					(1::BIGINT, 2::BIGINT, 'from'), (1::BIGINT, 1::BIGINT, 'to'), (1::BIGINT, 3::BIGINT, 'to'),
					(2::BIGINT, 1::BIGINT, 'from'), (2::BIGINT, 2::BIGINT, 'to'), (2::BIGINT, 3::BIGINT, 'to')
				) AS t(message_id, participant_id, recipient_type) WHERE `+tc.recipientFilter)
			if tc.draftOnlyRecipient {
				_, err = db.Exec(`CREATE TEMP TABLE draft_participants AS
					SELECT * FROM read_parquet(?) UNION ALL
					SELECT 4::BIGINT, 'carol@draft.example', 'draft.example', 'Carol Example', ''`, parquetDatasetGlob(root, "participants"))
				require.NoError(err)
				replaceRelationshipParquet(t, db, root, "participants", "SELECT * FROM draft_participants")
				_, err = db.Exec(`CREATE TEMP TABLE draft_recipients AS
					SELECT * FROM read_parquet(?) UNION ALL SELECT 2::BIGINT, 4::BIGINT, 'to'`, parquetDatasetGlob(root, "message_recipients"))
				require.NoError(err)
				replaceRelationshipParquet(t, db, root, "message_recipients", "SELECT * FROM draft_recipients")
			}
			_, err = Build(context.Background(), db, BuildOptions{
				Mode: ModeFull, StagedBaseRoot: root, OutputRoot: root,
				UnsentMessages: "(SELECT 2::BIGINT AS message_id)",
			})
			require.NoError(err)
			var messageID int64
			var occurredAt time.Time
			var fromMe bool
			require.NoError(db.QueryRow(logicalActivitySQL(relationshipParquetGlob(root, DatasetActivity), "true")+
				" SELECT anchor_message_id, occurred_at, is_from_me FROM logical_units").Scan(&messageID, &occurredAt, &fromMe))
			assert.Equal(int64(1), messageID)
			assert.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), occurredAt.UTC())
			assert.False(fromMe)
			var people int
			require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE last_at = TIMESTAMP '2026-07-01'`,
				relationshipParquetGlob(root, DatasetPeople)).Scan(&people))
			assert.Equal(3, people, "only sent activity contributes contact dates")
			if tc.draftOnlyRecipient {
				var draftPeople, draftDomains int
				require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE canonical_id = 4`, relationshipParquetGlob(root, DatasetPeople)).Scan(&draftPeople))
				assert.Zero(draftPeople, "draft-only Carol is absent from the people dataset")
				require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE domain = 'draft.example'`, relationshipParquetGlob(root, DatasetDomains)).Scan(&draftDomains))
				assert.Zero(draftDomains, "draft-only domains are absent from the domain dataset")
			}
		})
	}
}
