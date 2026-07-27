package query

import (
	"context"
	"fmt"
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

func TestRelationshipRollupMatchesLegacyOracleForOneSentEntry(t *testing.T) {
	b := NewTestDataBuilder(t)
	now := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	b.SetRelationshipAnchor(now)
	sourceID := b.AddSource("owner@example.com")
	ownerID := b.AddParticipant("owner@example.com", "example.com", "Owner")
	personID := b.AddParticipant("person@example.com", "example.com", "Person")
	b.AddOwnerParticipant(sourceID, ownerID)
	messageID := b.AddMessage(MessageOpt{
		SourceID: sourceID,
		IsFromMe: true,
		SentAt:   now,
	})
	b.AddFrom(messageID, ownerID, "Owner")
	b.AddTo(messageID, personID, "Person")
	engine := b.BuildEngine()

	conditions, args := buildExploreConditions(ExploreRequest{})
	queryText := buildLegacyRelationshipsSQLForEquivalence(
		conditions,
		engine.parquetPath(datasetParticipantClusters),
		engine.parquetPath(datasetOwnerParticipants),
	)
	args = append(args, identityindex.RelationshipDecayRate, now)

	var legacy RelationshipRow
	var memberIDsJSON string
	err := engine.db.QueryRowContext(
		context.Background(),
		queryText,
		args...,
	).Scan(
		&legacy.CanonicalID,
		&legacy.DisplayLabel,
		&memberIDsJSON,
		&legacy.Signals.SentToThem,
		&legacy.Signals.SentCount,
		&legacy.Signals.ReceivedFromThem,
		&legacy.Signals.MeetingsTogether,
		&legacy.Signals.MeetingCount,
		&legacy.Signals.Modalities,
		&legacy.LastAt,
	)
	require.NoError(t, err)

	indexed, err := engine.Relationships(
		context.Background(),
		RelationshipsRequest{Now: now, Limit: 10, ShowAll: true},
	)
	require.NoError(t, err)
	require.Len(t, indexed.Rows, 1)
	assert.Equal(t, legacy.CanonicalID, indexed.Rows[0].CanonicalID)
	assert.Equal(t, legacy.DisplayLabel, indexed.Rows[0].DisplayLabel)
	assert.Equal(t, legacy.Signals.SentCount, indexed.Rows[0].Signals.SentCount)
	assert.Equal(t, legacy.Signals.Modalities, indexed.Rows[0].Signals.Modalities)
	assert.Equal(t, legacy.LastAt, indexed.Rows[0].LastAt)
	assert.InDelta(t,
		legacy.Signals.SentToThem,
		indexed.Rows[0].Signals.SentToThem,
		1e-12,
	)
}
