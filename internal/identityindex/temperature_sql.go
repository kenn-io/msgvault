package identityindex

import (
	"fmt"
	"time"
)

const temperatureBuildRelation = "relationship_build_temperature_daily"

// relationshipTemperatureFirstYear is the first UTC calendar year that
// receives an annual temperature, matching the relationship calendar's year
// floor. Older qualifying activity still decays into the current score and
// stays in the daily datasets.
const relationshipTemperatureFirstYear = 1970

// MeetingOwnerGroupsSQL finds owner evidence for the selected transcript groups.
// selected must project message_id; all inputs are trusted DuckDB relations.
func MeetingOwnerGroupsSQL(selected, ownerActivity, groups string) string {
	return fmt.Sprintf(`
WITH selected_meetings AS (
	SELECT s.message_id, coalesce(g.group_id, s.message_id) AS meeting_group
	FROM %s s
	LEFT JOIN %s g ON g.message_id = s.message_id
), meeting_copies AS (
	SELECT g.message_id FROM %s g
	WHERE EXISTS (SELECT 1 FROM selected_meetings s WHERE s.meeting_group = g.group_id)
	UNION
	SELECT message_id FROM selected_meetings
)
SELECT coalesce(g.group_id, a.message_id) AS meeting_group
FROM %s a
JOIN meeting_copies c ON c.message_id = a.message_id
LEFT JOIN %s g ON g.message_id = a.message_id
WHERE a.is_owner AND a.is_direct AND a.message_type = 'meeting_transcript' AND NOT a.unsent AND NOT a.email_invite
GROUP BY meeting_group`, selected, groups, groups, ownerActivity, groups)
}

// RelationshipTemperatureFactsSQL selects one qualifying row per stored
// message and canonical person. The relations are trusted DuckDB SQL rendered
// by the cache/query packages. ownerActivity includes meeting copies outside
// activity's date window so the calendar retains group-level owner evidence.
func RelationshipTemperatureFactsSQL(activity, ownerActivity, groups string, effectiveAt time.Time) string {
	cutoff := effectiveAt.UTC().Format("2006-01-02 15:04:05.999999999")
	return fmt.Sprintf(`
WITH message_owner AS (
	SELECT message_id, bool_or(is_owner) AS owner_present
	FROM %[1]s
	GROUP BY message_id
), person_messages AS (
	SELECT f.message_id, f.canonical_id,
	       min(f.occurred_at)::TIMESTAMP AS occurred_at,
	       bool_or(f.is_chat) AS is_chat,
	       arg_min(f.entry_kind, f.occurred_at)::VARCHAR AS entry_kind,
	       arg_min(f.message_type, f.occurred_at)::VARCHAR AS message_type,
	       arg_min(f.conversation_type, f.occurred_at)::VARCHAR AS conversation_type,
	       bool_or(f.is_from_me) AS is_from_me,
	       bool_or(f.is_direct) AS is_direct,
	       bool_or(f.is_conversation_member) AS is_conversation_member,
	       bool_or(f.is_author) AS is_author,
	       bool_or(f.is_owner) AS person_is_owner,
	       bool_or(o.owner_present) AS owner_present
	FROM %[1]s f
	JOIN message_owner o USING (message_id)
	WHERE f.canonical_id IS NOT NULL
	  AND NOT f.is_owner
	  AND NOT f.unsent
	  AND NOT f.email_invite
	  AND f.occurred_at <= TIMESTAMP '%[2]s'
	  AND (
	      (f.entry_kind IN ('event', 'meeting'))
	      OR (f.is_chat AND lower(f.conversation_type) = 'direct_chat'
	          AND (f.is_conversation_member OR f.is_direct)
	          AND (f.is_from_me OR f.is_author))
	      OR (f.is_chat AND lower(f.conversation_type) <> 'direct_chat'
	          AND ((NOT f.is_from_me AND f.is_author)
	               OR (f.is_from_me AND f.is_direct)))
	      OR (NOT f.is_chat AND f.entry_kind = 'email'
	          AND f.is_direct AND (f.is_from_me OR f.is_author))
	  )
	GROUP BY f.message_id, f.canonical_id
), meeting_owners AS (
	%[7]s
), classified AS (
	SELECT *,
	       entry_kind IN ('event', 'meeting') AS is_meeting,
	       CASE
	           WHEN entry_kind IN ('event', 'meeting') THEN
	               CASE WHEN message_type = 'meeting_transcript' THEN EXISTS (
	                   SELECT 1 FROM meeting_owners o
	                   LEFT JOIN %[6]s g ON g.message_id = person_messages.message_id
	                   WHERE o.meeting_group = coalesce(g.group_id, person_messages.message_id))
	               ELSE owner_present END
	           WHEN is_chat AND lower(conversation_type) = 'direct_chat' THEN
	               owner_present AND (is_conversation_member OR is_direct)
	               AND (is_from_me OR is_author)
	           WHEN is_chat THEN
	               owner_present AND (
	                   (NOT is_from_me AND is_author)
	                   OR (is_from_me AND is_direct)
	               )
	           WHEN entry_kind = 'email' THEN
	               owner_present AND is_direct AND (is_from_me OR is_author)
	           ELSE false
	       END AS qualifies
	FROM person_messages
	WHERE NOT person_is_owner
), qualifying AS (
	SELECT *,
	       (NOT is_meeting AND is_from_me) AS sent,
	       (NOT is_meeting AND NOT is_from_me AND is_author) AS received,
	       CASE
	           WHEN is_meeting THEN %[3]d::UTINYINT
	           WHEN is_chat THEN %[4]d::UTINYINT
	           ELSE %[5]d::UTINYINT
	       END AS modality_mask
	FROM classified
	WHERE qualifies
)
SELECT message_id, canonical_id, occurred_at,
	   sent, received,
	   (NOT is_meeting AND NOT is_chat) AS email,
	   (NOT is_meeting AND is_chat) AS chat,
	   is_meeting AS meeting,
	   modality_mask
FROM qualifying
WHERE NOT is_meeting OR message_type <> 'meeting_transcript'
   OR (canonical_id, message_id) IN (%[8]s)
`,
		activity,
		cutoff,
		ModalityMeeting,
		ModalityChat,
		ModalityEmail,
		groups,
		MeetingOwnerGroupsSQL("(SELECT message_id FROM person_messages WHERE message_type = 'meeting_transcript')", ownerActivity, groups),
		meetingCopyRepresentativesSQL(ownerActivity, groups, cutoff),
	)
}

// meetingCopyRepresentativesSQL selects the transcript copy that represents
// each linked meeting group for a person. It ranks every copy in
// ownerActivity, not just those in activity's date window, so a windowed
// caller keeps the representative the annual score assigns. It reads the
// person_messages CTE of RelationshipTemperatureFactsSQL.
func meetingCopyRepresentativesSQL(ownerActivity, groups, cutoff string) string {
	return fmt.Sprintf(`
SELECT canonical_id, message_id FROM (
	SELECT c.canonical_id, c.message_id, `+MeetingCopyRank("c.canonical_id", "c.message_id", "c.message_type", "c.occurred_at")+` AS meeting_rank
	FROM (
		SELECT canonical_id, message_id, min(occurred_at)::TIMESTAMP AS occurred_at,
		       any_value(message_type)::VARCHAR AS message_type
		FROM %[1]s
		WHERE canonical_id IN (SELECT canonical_id FROM person_messages)
		  AND NOT is_owner AND NOT unsent AND NOT email_invite
		  AND occurred_at <= TIMESTAMP '%[3]s'
		  AND entry_kind IN ('event', 'meeting')
		  AND message_type = 'meeting_transcript'
		GROUP BY canonical_id, message_id
	) c
	LEFT JOIN %[2]s g ON g.message_id = c.message_id
) WHERE meeting_rank = 1`, ownerActivity, groups, cutoff)
}

// buildRelationshipTemperatureDailySQL reduces qualifying facts to one
// canonical-person UTC day. It deliberately does not reuse
// relationship_daily: that dataset is logical-entry grain and collapses a
// chat conversation to its newest event.
func buildRelationshipTemperatureDailySQL(activity, groups string, effectiveAt time.Time) string {
	return `
WITH relationship_facts AS (
` + RelationshipTemperatureFactsSQL(activity, activity, groups, effectiveAt) + `
)
SELECT canonical_id,
	   occurred_at::DATE AS event_date,
	   count(*) FILTER (WHERE sent)::BIGINT AS sent_count,
	   count(*) FILTER (WHERE received)::BIGINT AS received_count,
	   count(*) FILTER (WHERE meeting)::BIGINT AS meeting_count,
	   count(*) FILTER (WHERE email)::BIGINT AS email_count,
	   count(*) FILTER (WHERE chat)::BIGINT AS chat_count,
	   count(*)::BIGINT AS total_count,
	   bit_or(modality_mask)::UTINYINT AS modality_mask,
	   max(occurred_at)::TIMESTAMP AS last_at
FROM relationship_facts
GROUP BY canonical_id, event_date
ORDER BY canonical_id, event_date`
}

func relationshipTemperatureCTEs(effectiveAt time.Time) string {
	effectiveDate := effectiveAt.UTC().Format(time.DateOnly)
	return fmt.Sprintf(`
, current_signals AS (
	SELECT canonical_id,
	       sum(exp(-%[1]g * date_diff('day', event_date, DATE '%[2]s'))
	           * ln(1 + sent_count))::DOUBLE AS sent_signal,
	       sum(exp(-%[1]g * date_diff('day', event_date, DATE '%[2]s'))
	           * received_count)::DOUBLE AS received_volume,
	       sum(exp(-%[1]g * date_diff('day', event_date, DATE '%[2]s'))
	           * meeting_count)::DOUBLE AS meeting_signal,
	       bit_count(bit_or(modality_mask))::INTEGER AS modalities
	FROM %[3]s
	GROUP BY canonical_id
), current_scored AS (
	SELECT *,
	       ((%[4]g * sent_signal + %[5]g * ln(1 + received_volume)
	          + %[6]g * meeting_signal)
	        * (1 + %[7]g * greatest(modalities - 1, 0)))::DOUBLE AS raw_score
	FROM current_signals
), current_windowed AS (
	SELECT *,
	       rank() OVER (ORDER BY raw_score DESC)::BIGINT AS score_rank,
	       dense_rank() OVER (ORDER BY raw_score ASC)::BIGINT AS dense_ascending,
	       count(*) OVER ()::BIGINT AS population
	FROM current_scored
), current_distinct AS (
	SELECT count(DISTINCT raw_score)::BIGINT AS distinct_scores
	FROM current_scored
), current_ranked AS (
	SELECT w.*,
	       CASE WHEN d.distinct_scores = 1 THEN 100
	            ELSE round(100.0 * (w.dense_ascending - 1)
	                 / (d.distinct_scores - 1))::INTEGER END AS temperature
	FROM current_windowed w CROSS JOIN current_distinct d
), annual_signals AS (
	SELECT canonical_id, year(event_date)::INTEGER AS score_year,
	       sum(ln(1 + sent_count))::DOUBLE AS sent_signal,
	       sum(received_count)::DOUBLE AS received_volume,
	       sum(meeting_count)::DOUBLE AS meeting_signal,
	       bit_count(bit_or(modality_mask))::INTEGER AS modalities
	FROM %[3]s
	WHERE year(event_date) >= %[8]d
	GROUP BY canonical_id, score_year
), annual_scored AS (
	SELECT *,
	       ((%[4]g * sent_signal + %[5]g * ln(1 + received_volume)
	          + %[6]g * meeting_signal)
	        * (1 + %[7]g * greatest(modalities - 1, 0)))::DOUBLE AS raw_score
	FROM annual_signals
), annual_windowed AS (
	SELECT *,
	       rank() OVER (PARTITION BY score_year ORDER BY raw_score DESC)::BIGINT AS score_rank,
	       dense_rank() OVER (PARTITION BY score_year ORDER BY raw_score ASC)::BIGINT AS dense_ascending,
	       count(*) OVER (PARTITION BY score_year)::BIGINT AS population
	FROM annual_scored
), annual_distinct AS (
	SELECT score_year, count(DISTINCT raw_score)::BIGINT AS distinct_scores
	FROM annual_scored GROUP BY score_year
), annual_ranked AS (
	SELECT w.*,
	       CASE WHEN d.distinct_scores = 1 THEN 100
	            ELSE round(100.0 * (w.dense_ascending - 1)
	                 / (d.distinct_scores - 1))::INTEGER END AS temperature
	FROM annual_windowed w JOIN annual_distinct d USING (score_year)
), annual_rollups AS (
	SELECT canonical_id,
	       list(struct_pack(
	           year := score_year,
	           temperature := temperature,
	           rank := score_rank,
	           population := population,
	           raw_score := raw_score,
	           sent_signal := sent_signal,
	           received_volume := received_volume,
	           meeting_signal := meeting_signal,
	           modalities := modalities
	       ) ORDER BY score_year) AS annual_temperatures
	FROM annual_ranked
	GROUP BY canonical_id
), peaks AS (
	SELECT canonical_id,
	       arg_max(temperature, struct_pack(temperature := temperature, year := score_year))::INTEGER
	           AS peak_temperature,
	       arg_max(score_year, struct_pack(temperature := temperature, year := score_year))::INTEGER
	           AS peak_year
	FROM annual_ranked
	GROUP BY canonical_id
)`,
		RelationshipDecayRate,
		effectiveDate,
		temperatureBuildRelation,
		temperatureWeightSent,
		temperatureWeightReceived,
		temperatureWeightMeetings,
		temperatureBreadthStep,
		relationshipTemperatureFirstYear,
	)
}
