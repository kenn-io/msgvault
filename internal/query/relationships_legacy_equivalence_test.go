package query

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

// buildLegacyRelationshipsSQLForEquivalence is the pre-index relationship
// ranking query retained only as a Task 8 equivalence oracle. Production code
// must never call it.
func buildLegacyRelationshipsSQLForEquivalence(
	conditions string,
	clustersGlob string,
	ownersGlob string,
) string {
	labelExpr := sqlPersonDisplayLabelExpr(sqlClusterBestNameExpr(
		"pbn.id IN (SELECT cnl.participant_id FROM canon cnl WHERE cnl.canonical_id = p2.id)"), "p2")
	return buildExploreLogicalSQL(conditions) + fmt.Sprintf(`
), clusters AS (
    SELECT participant_id, canonical_id FROM read_parquet('%s')
), owners AS (
    SELECT DISTINCT participant_id FROM read_parquet('%s')
), canon AS (
    SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
    FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
), owner_canon AS (
    SELECT DISTINCT cn.canonical_id FROM owners o JOIN canon cn ON cn.participant_id = o.participant_id
), owner_participant_ids AS (
    SELECT DISTINCT cn.participant_id FROM canon cn
    WHERE cn.canonical_id IN (SELECT canonical_id FROM owner_canon)
), le_with_owner AS (
    SELECT le.*, list_has_any(le.participant_ids, (SELECT list(participant_id) FROM owner_participant_ids)) AS with_owner
    FROM logical_entries le
), author_links AS (
    SELECT mr.message_id, cn.canonical_id
    FROM message_recipients mr
    JOIN canon cn ON cn.participant_id = mr.participant_id
    WHERE mr.recipient_type = 'from'
    UNION
    SELECT m.id AS message_id, cn.canonical_id
    FROM messages m
    JOIN canon cn ON cn.participant_id = m.sender_id
    WHERE m.sender_id IS NOT NULL
), interactions AS (
    SELECT DISTINCT
        le.entry_key,
        cn.canonical_id,
        le.entry_kind,
        le.occurred_at,
        le.is_from_me,
        le.with_owner,
        EXISTS (SELECT 1 FROM author_links al
                WHERE al.message_id = le.anchor_message_id
                  AND al.canonical_id = cn.canonical_id) AS is_author,
        exp(-? * GREATEST(0, date_diff('day', le.occurred_at, CAST(? AS TIMESTAMP)))) AS decay
    FROM le_with_owner le
    CROSS JOIN UNNEST(le.participant_ids) AS pid(participant_id)
    JOIN canon cn ON cn.participant_id = pid.participant_id
    WHERE cn.canonical_id NOT IN (SELECT canonical_id FROM owner_canon)
      AND NOT (le.entry_kind IN ('event','meeting') AND NOT le.with_owner)
), aggregated AS (
    SELECT
        canonical_id,
        SUM(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN decay ELSE 0 END) AS sent_decayed,
        COUNT(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN 1 END) AS sent_count,
        SUM(CASE WHEN NOT is_from_me
                  AND (entry_kind = 'conversation' OR (entry_kind IN ('email','item') AND is_author))
                 THEN decay ELSE 0 END) AS received_decayed,
        SUM(CASE WHEN entry_kind IN ('event','meeting') AND with_owner THEN decay ELSE 0 END) AS meetings_decayed,
        COUNT(CASE WHEN entry_kind IN ('event','meeting') AND with_owner THEN 1 END) AS meeting_count,
        COUNT(DISTINCT CASE
            WHEN entry_kind IN ('event','meeting') AND with_owner THEN 'meeting'
            WHEN entry_kind IN ('event','meeting') THEN NULL
            WHEN entry_kind = 'conversation' THEN 'chat'
            ELSE 'email' END) AS modalities,
        MAX(occurred_at) AS last_at
    FROM interactions
    GROUP BY canonical_id
)
SELECT
    a.canonical_id,
    (SELECT %s
     FROM participants p2 WHERE p2.id = a.canonical_id) AS display_label,
    CAST(COALESCE((SELECT to_json(list(cn2.participant_id ORDER BY cn2.participant_id))
        FROM canon cn2 WHERE cn2.canonical_id = a.canonical_id), '[]') AS VARCHAR) AS member_ids,
    a.sent_decayed, a.sent_count, a.received_decayed, a.meetings_decayed, a.meeting_count, a.modalities, a.last_at
FROM aggregated a`, clustersGlob, ownersGlob, labelExpr)
}

func legacyRelationshipRowsForEquivalence(
	t *testing.T,
	engine *DuckDBEngine,
	request RelationshipsRequest,
) []RelationshipRow {
	t.Helper()
	requirementsForTest := require.New(t)
	now := request.Now.UTC()
	explore, err := engine.expandParticipantFilterClusters(
		t.Context(),
		ExploreRequest{Context: request.Context},
	)
	requirementsForTest.NoError(err)
	conditions, args := buildExploreConditions(explore)
	queryText := buildLegacyRelationshipsSQLForEquivalence(
		conditions,
		engine.parquetPath(datasetParticipantClusters),
		engine.parquetPath(datasetOwnerParticipants),
	)
	args = append(args, identityindex.RelationshipDecayRate, duckDBDateParam(now))

	rows, err := engine.db.QueryContext(t.Context(), queryText, args...)
	requirementsForTest.NoError(err)
	defer func() { require.NoError(t, rows.Close()) }()

	result := make([]RelationshipRow, 0)
	for rows.Next() {
		var row RelationshipRow
		var memberIDsJSON string
		requirementsForTest.NoError(rows.Scan(
			&row.CanonicalID,
			&row.DisplayLabel,
			&memberIDsJSON,
			&row.Signals.SentToThem,
			&row.Signals.SentCount,
			&row.Signals.ReceivedFromThem,
			&row.Signals.MeetingsTogether,
			&row.Signals.MeetingCount,
			&row.Signals.Modalities,
			&row.LastAt,
		))
		requirementsForTest.NoError(json.Unmarshal([]byte(memberIDsJSON), &row.MemberIDs))
		row.Signals.LastInteractionAt = row.LastAt
		row.Score = RelationshipScore(row.Signals)
		if request.ShowAll || row.Signals.SentCount > 0 || row.Signals.MeetingCount > 0 {
			result = append(result, row)
		}
	}
	requirementsForTest.NoError(rows.Err())
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score != result[j].Score {
			return result[i].Score > result[j].Score
		}
		if !result[i].LastAt.Equal(result[j].LastAt) {
			return result[i].LastAt.After(result[j].LastAt)
		}
		if result[i].DisplayLabel != result[j].DisplayLabel {
			return result[i].DisplayLabel < result[j].DisplayLabel
		}
		return result[i].CanonicalID < result[j].CanonicalID
	})
	return result
}

func requireRelationshipRowsEquivalent(
	t *testing.T,
	want []RelationshipRow,
	got []RelationshipRow,
) {
	t.Helper()
	assertionsForTest := assert.New(t)
	require.Len(t, got, len(want))
	for index := range want {
		assertionsForTest.Equal(want[index].CanonicalID, got[index].CanonicalID)
		assertionsForTest.Equal(want[index].DisplayLabel, got[index].DisplayLabel)
		assertionsForTest.Equal(want[index].MemberIDs, got[index].MemberIDs)
		assertionsForTest.Equal(want[index].Signals.SentCount, got[index].Signals.SentCount)
		assertionsForTest.Equal(want[index].Signals.MeetingCount, got[index].Signals.MeetingCount)
		assertionsForTest.Equal(want[index].Signals.Modalities, got[index].Signals.Modalities)
		assertionsForTest.Equal(want[index].LastAt, got[index].LastAt)
		assertionsForTest.Equal(want[index].Signals.LastInteractionAt, got[index].Signals.LastInteractionAt)
		assertionsForTest.InDelta(want[index].Signals.SentToThem, got[index].Signals.SentToThem, 1e-12)
		assertionsForTest.InDelta(want[index].Signals.ReceivedFromThem, got[index].Signals.ReceivedFromThem, 1e-12)
		assertionsForTest.InDelta(want[index].Signals.MeetingsTogether, got[index].Signals.MeetingsTogether, 1e-12)
		assertionsForTest.InDelta(want[index].Score, got[index].Score, 1e-12)
	}
}

func compareRelationshipIndexWithLegacy(
	t *testing.T,
	engine *DuckDBEngine,
	request RelationshipsRequest,
) []RelationshipRow {
	t.Helper()
	allWant := legacyRelationshipRowsForEquivalence(t, engine, request)
	indexed, err := engine.Relationships(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, int64(len(allWant)), indexed.TotalCount)
	limit := request.Limit
	if limit == 0 {
		limit = defaultRelationshipsLimit
	}
	var want []RelationshipRow
	if request.Offset < len(allWant) {
		want = allWant[request.Offset:min(request.Offset+limit, len(allWant))]
	}
	requireRelationshipRowsEquivalent(t, want, indexed.Rows)
	return indexed.Rows
}

func TestRelationshipIndexMatchesLegacyForAuthoredLinkedCoRecipient(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	sourceID := builder.AddSource("owner@example.com")
	ownerID := builder.AddParticipant("owner@example.com", "example.com", "Owner")
	authorID := builder.AddParticipant("author@example.com", "example.com", "Author")
	coRecipientID := builder.AddParticipant("alias@example.net", "example.net", "Alias")
	builder.AddOwnerParticipant(sourceID, ownerID)
	builder.LinkCluster(authorID, coRecipientID)

	messageID := builder.AddMessage(MessageOpt{
		SourceID: sourceID,
		SentAt:   now,
	})
	builder.AddFrom(messageID, authorID, "Author")
	builder.AddTo(messageID, ownerID, "Owner")
	builder.AddCc(messageID, coRecipientID, "Alias")
	engine := builder.BuildEngine()

	rows := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
		Now: now, Limit: 25, ShowAll: true,
	})
	requirements.Len(rows, 1)
	assertions.Equal(authorID, rows[0].CanonicalID)
	assertions.Equal([]int64{authorID, coRecipientID}, rows[0].MemberIDs)
	assertions.InDelta(float64(1), rows[0].Signals.ReceivedFromThem, 1e-12,
		"bool-or merged authorship must credit one incoming unit, not zero or two")
}

func TestRelationshipIndexMatchesLegacyAcrossAdversarialSemantics(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	builder := NewTestDataBuilder(t)
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	sourceID := builder.AddSource("owner@example.com")
	ownerID := builder.AddParticipant("owner@example.com", "example.com", "Owner")
	ownerAliasID := builder.AddParticipant("owner@alias.example", "alias.example", "Owner Alias")
	contactID := builder.AddParticipant("contact@example.com", "example.com", "Contact")
	contactAliasID := builder.AddParticipant("contact@chat.example", "chat.example", "Contact Chat")
	conversationOnlyID := builder.AddParticipant("member@room.example", "room.example", "Room Member")
	bystanderID := builder.AddParticipant("bystander@example.net", "example.net", "Bystander")
	unnamedID := builder.AddParticipant("unnamed@example.org", "example.org", "")
	builder.AddOwnerParticipant(sourceID, ownerID)
	builder.LinkCluster(ownerID, ownerAliasID)
	builder.LinkCluster(contactID, contactAliasID)

	nonChatConversationID := int64(900)
	incomingID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: nonChatConversationID,
		SentAt: now.AddDate(0, 0, -4),
	})
	builder.AddFrom(incomingID, contactID, "Contact")
	builder.AddTo(incomingID, ownerID, "Owner")
	builder.AddConversationParticipant(nonChatConversationID, conversationOnlyID)

	outgoingID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, IsFromMe: true,
		SentAt: now.AddDate(0, 0, -3),
	})
	builder.AddFrom(outgoingID, ownerAliasID, "Owner Alias")
	builder.AddTo(outgoingID, contactAliasID, "Contact Chat")

	chatConversationID := int64(901)
	chatAt := now.AddDate(0, 0, -2).Add(10 * time.Hour)
	incomingChatID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: chatConversationID,
		MessageType: "imessage", ConversationType: "imessage",
		SentAt: chatAt, IsFromMe: false,
	})
	builder.AddFrom(incomingChatID, contactAliasID, "Contact Chat")
	builder.AddTo(incomingChatID, ownerID, "Owner")
	outgoingChatID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, ConversationID: chatConversationID,
		MessageType: "imessage", ConversationType: "imessage",
		SentAt: chatAt, IsFromMe: true,
	})
	builder.AddFrom(outgoingChatID, ownerID, "Owner")
	builder.AddTo(outgoingChatID, contactAliasID, "Contact Chat")

	meetingID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "calendar_event",
		SentAt: now.AddDate(0, 0, -1),
	})
	builder.AddFrom(meetingID, ownerID, "Owner")
	builder.AddTo(meetingID, contactID, "Contact")

	ownerlessMeetingID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "meeting_note",
		SentAt: now.Add(-12 * time.Hour),
	})
	builder.AddFrom(ownerlessMeetingID, contactID, "Contact")
	builder.AddTo(ownerlessMeetingID, bystanderID, "Bystander")

	itemID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "slack",
		SentAt: now.Add(-6 * time.Hour),
	})
	builder.AddFrom(itemID, contactID, "Contact")
	builder.AddTo(itemID, ownerID, "Owner")

	futureID := builder.AddMessage(MessageOpt{
		SourceID: sourceID, IsFromMe: true,
		SentAt: now.AddDate(0, 0, 2),
	})
	builder.AddFrom(futureID, ownerID, "Owner")
	builder.AddTo(futureID, contactID, "Contact")

	unnamedIDMessage := builder.AddMessage(MessageOpt{
		SourceID: sourceID, IsFromMe: true,
		SentAt: now.AddDate(0, 0, -5),
	})
	builder.AddFrom(unnamedIDMessage, ownerID, "Owner")
	builder.AddTo(unnamedIDMessage, unnamedID, "")

	engine := builder.BuildEngine()
	_, err := engine.db.ExecContext(t.Context(), "SET TimeZone = 'America/New_York'")
	requirementsForTest.NoError(err,
		"equivalence must not depend on DuckDB's session or host timezone")
	allRows := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
		Now: now, Limit: 25, ShowAll: true,
	})
	requirementsForTest.Len(allRows, 2)
	assertionsForTest.Equal(3, allRows[0].Signals.Modalities)
	assertionsForTest.Equal(futureIDTime(builder, futureID), allRows[0].LastAt)
	page := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
		Now: now, Limit: 1, Offset: 1, ShowAll: true,
	})
	requirementsForTest.Len(page, 1)
	assertionsForTest.Equal(allRows[1].CanonicalID, page[0].CanonicalID)

	conversationFiltered := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
		Context: Context{ParticipantIDs: []int64{conversationOnlyID}},
		Now:     now, Limit: 25, ShowAll: true,
	})
	requirementsForTest.Len(conversationFiltered, 1)
	assertionsForTest.Equal(contactID, conversationFiltered[0].CanonicalID)
	assertionsForTest.Equal(int64(0), conversationFiltered[0].Signals.SentCount)
	assertionsForTest.Positive(conversationFiltered[0].Signals.ReceivedFromThem)
	assertionsForTest.Less(conversationFiltered[0].Signals.ReceivedFromThem, float64(1))

	chatFiltered := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
		Context: Context{MessageTypes: []string{"imessage"}},
		Now:     now, Limit: 25, ShowAll: true,
	})
	requirementsForTest.Len(chatFiltered, 1)
	assertionsForTest.Equal(int64(1), chatFiltered[0].Signals.SentCount,
		"the higher message ID wins an equal-timestamp chat direction tie")
	assertionsForTest.InDelta(float64(0), chatFiltered[0].Signals.ReceivedFromThem, 1e-12)
	assertionsForTest.Equal(1, chatFiltered[0].Signals.Modalities)
}

func futureIDTime(builder *TestDataBuilder, messageID int64) time.Time {
	for _, message := range builder.messages {
		if message.ID == messageID {
			return message.SentAt
		}
	}
	return time.Time{}
}

func TestRelationshipIndexDeletionScopesMatchLegacy(t *testing.T) {
	builder := NewTestDataBuilder(t)
	sourceID := builder.AddSourceWithType("archive@example.com", "gmail")
	ownerID := builder.AddParticipant("owner@example.com", "example.com", "Owner")
	activeID := builder.AddParticipant("active@example.com", "example.com", "Active")
	deletedID := builder.AddParticipant("deleted@example.com", "example.com", "Deleted")
	builder.AddOwnerParticipant(sourceID, ownerID)
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)

	activeMessage := builder.AddMessage(MessageOpt{
		SourceID: sourceID,
		IsFromMe: true,
		SentAt:   now.Add(-24 * time.Hour),
	})
	builder.AddFrom(activeMessage, ownerID, "Owner")
	builder.AddTo(activeMessage, activeID, "Active")
	deletedAt := now.Add(-time.Hour)
	deletedMessage := builder.AddMessage(MessageOpt{
		SourceID:            sourceID,
		IsFromMe:            true,
		SentAt:              now.Add(-48 * time.Hour),
		DeletedFromSourceAt: &deletedAt,
	})
	builder.AddFrom(deletedMessage, ownerID, "Owner")
	builder.AddTo(deletedMessage, deletedID, "Deleted")
	engine := builder.BuildEngine()

	for _, deletion := range []DeletionFilter{DeletionActive, DeletionDeleted} {
		rows := compareRelationshipIndexWithLegacy(t, engine, RelationshipsRequest{
			Context: Context{Deletion: deletion},
			Now:     now,
			Limit:   25,
			ShowAll: true,
		})
		require.Len(t, rows, 1)
	}
}
