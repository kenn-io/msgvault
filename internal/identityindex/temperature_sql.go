package identityindex

import (
	"fmt"
	"time"
)

const temperatureBuildRelation = "relationship_build_temperature_daily"

// RelationshipTemperatureFactsSQL selects one qualifying row per stored
// message and canonical person. activity is a trusted DuckDB relation rendered
// by the cache/query packages; it is not user input. The shared SQL keeps cache
// scores and local-year calendars on one attribution contract.
func RelationshipTemperatureFactsSQL(activity string, effectiveAt time.Time) string {
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
), classified AS (
	SELECT *,
	       entry_kind IN ('event', 'meeting') AS is_meeting,
	       CASE
	           WHEN entry_kind IN ('event', 'meeting') THEN owner_present
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
`,
		activity,
		cutoff,
		ModalityMeeting,
		ModalityChat,
		ModalityEmail,
	)
}

// buildRelationshipTemperatureDailySQL reduces qualifying facts to one
// canonical-person UTC day. It deliberately does not reuse
// relationship_daily: that dataset is logical-entry grain and collapses a
// chat conversation to its newest event.
func buildRelationshipTemperatureDailySQL(activity string, effectiveAt time.Time) string {
	return `
WITH relationship_facts AS (
` + RelationshipTemperatureFactsSQL(activity, effectiveAt) + `
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
	)
}
