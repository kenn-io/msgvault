package identityindex

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipTemperatureDailyAttributesDirectAndGroupInteractions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.Exec(`
		CREATE TEMP TABLE synthetic_temperature_activity AS
		SELECT * FROM (VALUES
			-- Direct chat written by the owner: the explicit edge identifies
			-- person 2 even when the importer has no conversation roster.
			(1::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 12:00:00', true, 'item'::VARCHAR, 'direct_chat'::VARCHAR, true, false, true, false, true),
			(1::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 12:00:00', true, 'item'::VARCHAR, 'direct_chat'::VARCHAR, true, true, false, false, false),
			-- Group chat written by person 2.
			(2::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 13:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, false, true, false, true),
			(2::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 13:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, true, true, true, false),
			-- Unrelated group author: person 2 is only a roster member.
			(3::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 14:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, false, true, false, true),
			(3::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 14:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, false, true, false, false),
			(3::BIGINT, 3::BIGINT, TIMESTAMP '2026-01-02 14:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, true, true, true, false),
			-- Owner broadcast without a direct edge: person 2 must not receive credit.
			(4::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 15:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, true, true, true, true, true),
			(4::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 15:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, true, false, true, false, false),
			-- Explicit direct recipient on an owner group message counts for person 2.
			(5::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 16:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, true, true, true, true, true),
			(5::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 16:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, true, true, true, false, false),
			-- Owner-present and owner-absent meetings.
			(6::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 17:00:00', false, 'meeting'::VARCHAR, 'meeting'::VARCHAR, false, true, false, false, true),
			(6::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 17:00:00', false, 'meeting'::VARCHAR, 'meeting'::VARCHAR, false, true, false, false, false),
			(7::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 18:00:00', false, 'meeting'::VARCHAR, 'meeting'::VARCHAR, false, true, false, false, false),
			-- A duplicate linked-alias edge for canonical person 2.
			(2::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 13:00:00', true, 'item'::VARCHAR, 'group_chat'::VARCHAR, false, true, false, true, false)
		) AS t(message_id, canonical_id, occurred_at, is_chat, entry_kind,
			conversation_type, is_from_me, is_direct, is_conversation_member,
			is_author, is_owner)
	`)
	require.NoError(err)

	var sent, received, meetings, email, chat, total int64
	var modality uint8
	query := buildRelationshipTemperatureDailySQL(
		"synthetic_temperature_activity",
		time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC),
	)
	err = db.QueryRow(`
		SELECT sent_count, received_count, meeting_count, email_count,
		       chat_count, total_count, modality_mask
		FROM (`+query+`)
		WHERE canonical_id = 2
	`).Scan(&sent, &received, &meetings, &email, &chat, &total, &modality)
	require.NoError(err)
	assert.Equal(int64(2), sent)
	assert.Equal(int64(1), received)
	assert.Equal(int64(1), meetings)
	assert.Zero(email)
	assert.Equal(int64(3), chat)
	assert.Equal(int64(4), total)
	assert.Equal(ModalityChat|ModalityMeeting, modality)
}

func TestRelationshipTemperatureCurrentHalfLifeKeepsOlderSignalsInWholeGraph(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.Exec(`
		CREATE TEMP TABLE ` + temperatureBuildRelation + ` (
			canonical_id BIGINT, event_date DATE, sent_count BIGINT,
			received_count BIGINT, meeting_count BIGINT, email_count BIGINT,
			chat_count BIGINT, total_count BIGINT, modality_mask UTINYINT,
			last_at TIMESTAMP
		);
		INSERT INTO ` + temperatureBuildRelation + ` VALUES
			(2, DATE '2025-01-01', 10, 0, 0, 10, 0, 10, 1, TIMESTAMP '2025-01-01 12:00:00'),
			(3, DATE '2026-01-01', 1, 0, 0, 1, 0, 1, 1, TIMESTAMP '2026-01-01 12:00:00')
	`)
	require.NoError(err)

	query := `WITH anchor AS (SELECT 1)` + relationshipTemperatureCTEs(
		time.Date(2026, time.January, 1, 23, 59, 59, 0, time.UTC),
	) + ` SELECT canonical_id, population, raw_score FROM current_ranked ORDER BY canonical_id`
	rows, err := db.Query(query)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	require.True(rows.Next())
	var canonicalID, population int64
	var rawScore float64
	require.NoError(rows.Scan(&canonicalID, &population, &rawScore))
	assert.Equal(int64(2), canonicalID)
	assert.Equal(int64(2), population)
	assert.InDelta(2*RelationshipDayWeight(365)*math.Log(11), rawScore, 1e-9)
	require.True(rows.Next(), "recent and older qualifying contacts share one graph")
	require.NoError(rows.Scan(&canonicalID, &population, &rawScore))
	assert.Equal(int64(3), canonicalID)
	assert.Equal(int64(2), population)
	assert.InDelta(2*math.Log(2), rawScore, 1e-9)
	assert.False(rows.Next())
	require.NoError(rows.Err())
}
