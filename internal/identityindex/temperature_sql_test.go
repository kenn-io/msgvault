package identityindex

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestRelationshipTemperatureDailyAttributesDirectAndGroupInteractions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.ExecContext(t.Context(), `
		CREATE TEMP TABLE synthetic_temperature_activity AS
		SELECT *, false AS unsent FROM (VALUES
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
	err = db.QueryRowContext(t.Context(), `
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

func TestRelationshipTemperatureDailyExcludesUnsent(t *testing.T) {
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.ExecContext(t.Context(), `
		CREATE TEMP TABLE synthetic_unsent_activity AS
		SELECT * FROM (VALUES
			(8::BIGINT, 1::BIGINT, TIMESTAMP '2026-01-02 19:00:00', true, 'item'::VARCHAR, 'direct_chat'::VARCHAR, true, false, true, true, true, true),
			(8::BIGINT, 2::BIGINT, TIMESTAMP '2026-01-02 19:00:00', true, 'item'::VARCHAR, 'direct_chat'::VARCHAR, true, true, true, false, false, true)
		) AS t(message_id, canonical_id, occurred_at, is_chat, entry_kind,
			conversation_type, is_from_me, is_direct, is_conversation_member,
			is_author, is_owner, unsent)
	`)
	require.NoError(err)
	query := buildRelationshipTemperatureDailySQL("synthetic_unsent_activity", time.Date(2026, time.January, 3, 0, 0, 0, 0, time.UTC))
	var count int
	require.NoError(db.QueryRowContext(t.Context(), "SELECT count(*) FROM ("+query+")").Scan(&count))
	assert.Zero(t, count, "an owner-authored unsent chat contributes no daily contact activity")
}

func TestRelationshipTemperatureCurrentHalfLifeKeepsOlderSignalsInWholeGraph(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.ExecContext(t.Context(), `
		CREATE TEMP TABLE `+temperatureBuildRelation+` (
			canonical_id BIGINT, event_date DATE, sent_count BIGINT,
			received_count BIGINT, meeting_count BIGINT, email_count BIGINT,
			chat_count BIGINT, total_count BIGINT, modality_mask UTINYINT,
			last_at TIMESTAMP
		);
		INSERT INTO `+temperatureBuildRelation+` VALUES
			(2, DATE '2025-01-01', 10, 0, 0, 10, 0, 10, 1, TIMESTAMP '2025-01-01 12:00:00'),
			(3, DATE '2026-01-01', 1, 0, 0, 1, 0, 1, 1, TIMESTAMP '2026-01-01 12:00:00')
	`)
	require.NoError(err)

	query := `WITH anchor AS (SELECT 1)` + relationshipTemperatureCTEs(
		time.Date(2026, time.January, 1, 23, 59, 59, 0, time.UTC),
	) + ` SELECT canonical_id, population, raw_score FROM current_ranked ORDER BY canonical_id`
	rows, err := db.QueryContext(t.Context(), query)
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

func TestRelationshipTemperatureAnnualRollupsStartAtFirstYear(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.ExecContext(t.Context(), `
		CREATE TEMP TABLE `+temperatureBuildRelation+` (
			canonical_id BIGINT, event_date DATE, sent_count BIGINT,
			received_count BIGINT, meeting_count BIGINT, email_count BIGINT,
			chat_count BIGINT, total_count BIGINT, modality_mask UTINYINT,
			last_at TIMESTAMP
		);
		INSERT INTO `+temperatureBuildRelation+` VALUES
			(2, DATE '1969-12-31', 1, 0, 0, 1, 0, 1, 1, TIMESTAMP '1969-12-31 23:59:59'),
			(2, DATE '1970-01-01', 1, 0, 0, 1, 0, 1, 1, TIMESTAMP '1970-01-01 00:00:00'),
			(3, DATE '1904-01-04', 0, 1, 0, 1, 0, 1, 1, TIMESTAMP '1904-01-04 09:00:00')
	`)
	require.NoError(err)

	query := `WITH anchor AS (SELECT 1)` + relationshipTemperatureCTEs(
		time.Date(1970, time.January, 2, 0, 0, 0, 0, time.UTC),
	)

	func() {
		rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id,
			CAST(to_json(list_transform(annual_temperatures, lambda x: x.year)) AS VARCHAR)
			FROM annual_rollups ORDER BY canonical_id`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		require.True(rows.Next())
		var canonicalID int64
		var years string
		require.NoError(rows.Scan(&canonicalID, &years))
		assert.Equal(int64(2), canonicalID)
		assert.JSONEq(`[1970]`, years)
		assert.False(rows.Next())
		require.NoError(rows.Err())
	}()

	func() {
		rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id, peak_year
			FROM peaks ORDER BY canonical_id`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		require.True(rows.Next())
		var canonicalID, peakYear int64
		require.NoError(rows.Scan(&canonicalID, &peakYear))
		assert.Equal(int64(2), canonicalID)
		assert.Equal(int64(1970), peakYear)
		assert.False(rows.Next())
		require.NoError(rows.Err())
	}()

	func() {
		rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id, population, raw_score
			FROM current_ranked ORDER BY canonical_id`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		require.True(rows.Next())
		var canonicalID, population int64
		var rawScore float64
		require.NoError(rows.Scan(&canonicalID, &population, &rawScore))
		assert.Equal(int64(2), canonicalID)
		assert.Equal(int64(2), population)
		assert.InDelta(2*(RelationshipDayWeight(2)+RelationshipDayWeight(1))*math.Log(2), rawScore, 1e-9)
		require.True(rows.Next())
		require.NoError(rows.Scan(&canonicalID, &population, &rawScore))
		assert.Equal(int64(3), canonicalID)
		assert.Equal(int64(2), population)
		assert.False(rows.Next())
		require.NoError(rows.Err())
	}()
}

func TestHegelAnnualTemperatureRollups(t *testing.T) {
	_, db := writeRelationshipBaseFixture(t, true)
	_, err := db.ExecContext(t.Context(), `CREATE TEMP TABLE `+temperatureBuildRelation+` (
		canonical_id BIGINT, event_date DATE, sent_count BIGINT, received_count BIGINT,
		meeting_count BIGINT, email_count BIGINT, chat_count BIGINT, total_count BIGINT,
		modality_mask UTINYINT, last_at TIMESTAMP)`)
	require.NoError(t, err)
	effective := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	query := `WITH anchor AS (SELECT 1)` + relationshipTemperatureCTEs(effective)
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		assert := assert.New(ht)
		_, err := db.ExecContext(t.Context(), `DELETE FROM `+temperatureBuildRelation)
		require.NoError(err)
		// One contact has equal temperatures in two years to exercise peak tie-breaking.
		dates := []time.Time{
			time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC),
		}
		ids := []int64{4, 4}
		for n := hegel.Draw(ht, hegel.Integers(1, 6)); n > 0; n-- {
			var date time.Time
			if hegel.Draw(ht, hegel.Booleans()) {
				date = hegel.Draw(ht, hegel.Dates().Min(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)).Max(time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)))
			} else {
				date = hegel.Draw(ht, hegel.Dates().Min(time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)).Max(effective))
				if date.Year() == 2000 {
					date = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
				}
			}
			dates = append(dates, date)
			ids = append(ids, hegel.Draw(ht, hegel.Integers(int64(5), int64(7))))
		}
		ht.Logf("ids=%v dates=%v", ids, dates)
		expected := map[int64]map[int]bool{}
		seen := map[string]bool{}
		for i, date := range dates {
			id := ids[i]
			key := fmt.Sprintf("%d/%s", id, date.Format("2006-01-02"))
			if seen[key] {
				continue
			}
			seen[key] = true
			if date.Year() >= 1970 {
				if expected[id] == nil {
					expected[id] = map[int]bool{}
				}
				expected[id][date.Year()] = true
			}
			_, err := db.ExecContext(t.Context(), `INSERT INTO `+temperatureBuildRelation+` VALUES (?, ?, 1, 0, 0, 1, 0, 1, 1, ?)`, id, date.Format("2006-01-02"), date.Format("2006-01-02"))
			require.NoError(err)
		}
		actual := map[int64][]int{}
		func() {
			rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id, CAST(to_json(list_transform(annual_temperatures, lambda x: x.year)) AS VARCHAR) FROM annual_rollups`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var id int64
				var raw string
				require.NoError(rows.Scan(&id, &raw))
				var years []int
				require.NoError(json.Unmarshal([]byte(raw), &years))
				actual[id] = years
			}
			require.NoError(rows.Err())
		}()
		want := map[int64][]int{}
		for id, years := range expected {
			for year := range years {
				want[id] = append(want[id], year)
			}
			sort.Ints(want[id])
		}
		assert.Equal(want, actual)
		current := map[int64]bool{}
		func() {
			rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id FROM current_ranked`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var id int64
				require.NoError(rows.Scan(&id))
				assert.False(current[id])
				current[id] = true
			}
			require.NoError(rows.Err())
		}()
		wantCurrent := map[int64]bool{}
		for _, id := range ids {
			wantCurrent[id] = true
		}
		assert.Equal(wantCurrent, current)
		peaks := map[int64]int{}
		func() {
			rows, err := db.QueryContext(t.Context(), query+` SELECT canonical_id, peak_year FROM peaks`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var id int64
				var year int
				require.NoError(rows.Scan(&id, &year))
				peaks[id] = year
			}
			require.NoError(rows.Err())
		}()
		assert.Len(peaks, len(expected))
		for id := range expected {
			assert.Contains(peaks, id)
		}
		assert.Equal(2000, peaks[4])
	})
}
