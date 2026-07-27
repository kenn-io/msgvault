package identityindex

import (
	"fmt"
	"strings"
	"time"
)

// ActivityPaths identifies the fact, edge, and identity inputs used by the
// shared logical-entry reduction. Each value is normally a Parquet glob. The
// cache builder may pass a trusted read_parquet relation to union committed
// and staged append datasets during an incremental build.
type ActivityPaths struct {
	Facts             string
	DirectEdges       string
	ConversationEdges string
	Directory         string
	Clusters          string
	Owners            string
}

// LogicalActivitySQL returns CTEs named canonical_message_edges,
// logical_units, logical_people, and logical_domains. filterSQL is trusted SQL
// rendered by the query layer and may refer to the fact alias f.
func LogicalActivitySQL(paths ActivityPaths, filterSQL string) string {
	if strings.TrimSpace(filterSQL) == "" {
		filterSQL = "true"
	}
	facts := activityRelation(paths.Facts, true)
	directEdges := activityRelation(paths.DirectEdges, false)
	conversationEdges := activityRelation(paths.ConversationEdges, false)
	clusters := activityRelation(paths.Clusters, false)
	owners := activityRelation(paths.Owners, false)

	return fmt.Sprintf(`
WITH clusters AS (
	SELECT participant_id::BIGINT AS participant_id,
	       canonical_id::BIGINT AS canonical_id
	FROM %s
), owner_canon AS (
	SELECT DISTINCT coalesce(c.canonical_id, try_cast(o.participant_id AS BIGINT))::BIGINT
	       AS canonical_id
	FROM %s o
	LEFT JOIN clusters c ON c.participant_id = o.participant_id
), filtered_facts AS (
	SELECT f.*
	FROM %s f
	WHERE %s
), direct_edges AS (
	SELECT d.message_id::BIGINT AS message_id,
	       d.participant_id::BIGINT AS participant_id,
	       d.participant_domain::VARCHAR AS participant_domain,
	       d.is_sender::BOOLEAN AS is_sender,
	       d.is_author::BOOLEAN AS is_author
	FROM %s d
	JOIN filtered_facts f ON f.message_id = d.message_id
), conversation_edges AS (
	SELECT d.conversation_id::BIGINT AS conversation_id,
	       d.participant_id::BIGINT AS participant_id,
	       d.participant_domain::VARCHAR AS participant_domain
	FROM %s d
	JOIN (
		SELECT DISTINCT conversation_id
		FROM filtered_facts
		WHERE conversation_id IS NOT NULL
	) f ON f.conversation_id = d.conversation_id
), canonical_message_domain_edges AS (
	SELECT d.message_id::BIGINT AS message_id,
	       coalesce(c.canonical_id, d.participant_id)::BIGINT AS canonical_id,
	       d.participant_domain::VARCHAR AS participant_domain,
	       bool_or(d.is_sender) AS is_sender,
	       bool_or(d.is_author) AS is_author
	FROM direct_edges d
	LEFT JOIN clusters c ON c.participant_id = d.participant_id
	GROUP BY d.message_id, coalesce(c.canonical_id, d.participant_id),
	         d.participant_domain
), canonical_message_edges AS (
	SELECT message_id, canonical_id,
	       bool_or(is_sender) AS is_sender,
	       bool_or(is_author) AS is_author
	FROM canonical_message_domain_edges
	GROUP BY message_id, canonical_id
), canonical_conversation_domain_edges AS (
	SELECT d.conversation_id::BIGINT AS conversation_id,
	       coalesce(c.canonical_id, d.participant_id)::BIGINT AS canonical_id,
	       d.participant_domain::VARCHAR AS participant_domain
	FROM conversation_edges d
	LEFT JOIN clusters c ON c.participant_id = d.participant_id
	GROUP BY d.conversation_id, coalesce(c.canonical_id, d.participant_id),
	         d.participant_domain
), canonical_conversation_edges AS (
	SELECT DISTINCT conversation_id, canonical_id
	FROM canonical_conversation_domain_edges
), nonchat_units AS (
	SELECT ('message:' || f.message_id)::VARCHAR AS entry_key,
	       f.message_id::BIGINT AS anchor_message_id,
	       f.conversation_id::BIGINT AS conversation_id,
	       f.source_id::BIGINT AS source_id,
	       f.source_type::VARCHAR AS source_type,
	       f.occurred_at::TIMESTAMP AS occurred_at,
	       f.entry_kind::VARCHAR AS entry_kind,
	       f.is_from_me::BOOLEAN AS is_from_me,
	       f.attachment_count::BIGINT AS attachment_count
	FROM filtered_facts f
	WHERE NOT f.is_chat
), chat_units AS (
	SELECT ('conversation:' || f.source_id || ':' || f.conversation_id)::VARCHAR AS entry_key,
	       arg_max(f.message_id,
	           struct_pack(occurred_at := f.occurred_at, message_id := f.message_id))::BIGINT
	           AS anchor_message_id,
	       f.conversation_id::BIGINT AS conversation_id,
	       f.source_id::BIGINT AS source_id,
	       arg_max(f.source_type,
	           struct_pack(occurred_at := f.occurred_at, message_id := f.message_id))::VARCHAR
	           AS source_type,
	       max(f.occurred_at)::TIMESTAMP AS occurred_at,
	       'conversation'::VARCHAR AS entry_kind,
	       arg_max(f.is_from_me,
	           struct_pack(occurred_at := f.occurred_at, message_id := f.message_id))::BOOLEAN
	           AS is_from_me,
	       coalesce(sum(f.attachment_count), 0)::BIGINT AS attachment_count
	FROM filtered_facts f
	WHERE f.is_chat
	GROUP BY f.source_id, f.conversation_id
), logical_units AS (
	SELECT * FROM nonchat_units
	UNION ALL
	SELECT * FROM chat_units
), logical_people_candidates AS (
	SELECT u.entry_key, d.canonical_id, d.is_author
	FROM nonchat_units u
	JOIN canonical_message_edges d ON d.message_id = u.anchor_message_id

	UNION ALL

	SELECT u.entry_key, d.canonical_id,
	       (d.message_id = u.anchor_message_id AND d.is_author) AS is_author
	FROM chat_units u
	JOIN filtered_facts f
	  ON f.source_id = u.source_id AND f.conversation_id = u.conversation_id
	 AND f.is_chat
	JOIN canonical_message_edges d ON d.message_id = f.message_id

	UNION ALL

	SELECT u.entry_key, d.canonical_id, false AS is_author
	FROM chat_units u
	JOIN canonical_conversation_edges d ON d.conversation_id = u.conversation_id
), logical_people_grouped AS (
	SELECT entry_key, canonical_id, bool_or(is_author) AS is_author
	FROM logical_people_candidates
	GROUP BY entry_key, canonical_id
), logical_owner_presence AS (
	SELECT DISTINCT p.entry_key
	FROM logical_people_grouped p
	JOIN owner_canon o USING (canonical_id)
), logical_people AS (
	SELECT u.*, p.canonical_id, p.is_author,
	       (o.canonical_id IS NOT NULL) AS is_owner,
	       (op.entry_key IS NOT NULL) AS with_owner
	FROM logical_people_grouped p
	JOIN logical_units u USING (entry_key)
	LEFT JOIN owner_canon o USING (canonical_id)
	LEFT JOIN logical_owner_presence op USING (entry_key)
), logical_person_domain_candidates AS (
	SELECT u.entry_key, d.canonical_id, d.participant_domain AS domain
	FROM nonchat_units u
	JOIN canonical_message_domain_edges d ON d.message_id = u.anchor_message_id

	UNION ALL

	SELECT u.entry_key, d.canonical_id, d.participant_domain AS domain
	FROM chat_units u
	JOIN filtered_facts f
	  ON f.source_id = u.source_id AND f.conversation_id = u.conversation_id
	 AND f.is_chat
	JOIN canonical_message_domain_edges d ON d.message_id = f.message_id

	UNION ALL

	SELECT u.entry_key, d.canonical_id, d.participant_domain AS domain
	FROM chat_units u
	JOIN canonical_conversation_domain_edges d
	  ON d.conversation_id = u.conversation_id
), logical_person_domains AS (
	SELECT DISTINCT entry_key, canonical_id, domain
	FROM logical_person_domain_candidates
), logical_domain_candidates AS (
	SELECT u.entry_key, d.participant_domain AS domain
	FROM nonchat_units u
	JOIN canonical_message_domain_edges d ON d.message_id = u.anchor_message_id

	UNION ALL

	SELECT u.entry_key, d.participant_domain AS domain
	FROM nonchat_units u
	JOIN canonical_conversation_domain_edges d ON d.conversation_id = u.conversation_id

	UNION ALL

	SELECT u.entry_key, d.participant_domain AS domain
	FROM chat_units u
	JOIN filtered_facts f
	  ON f.source_id = u.source_id AND f.conversation_id = u.conversation_id
	 AND f.is_chat
	JOIN canonical_message_domain_edges d ON d.message_id = f.message_id

	UNION ALL

	SELECT u.entry_key, d.participant_domain AS domain
	FROM chat_units u
	JOIN canonical_conversation_domain_edges d ON d.conversation_id = u.conversation_id
), logical_domain_keys AS (
	SELECT DISTINCT entry_key, domain
	FROM logical_domain_candidates
), logical_domains AS (
	SELECT u.*, k.domain,
	       coalesce(
	           list(DISTINCT pd.canonical_id ORDER BY pd.canonical_id)
	               FILTER (WHERE pd.canonical_id IS NOT NULL),
	           []::BIGINT[]
	       ) AS canonical_ids
	FROM logical_domain_keys k
	JOIN logical_units u USING (entry_key)
	LEFT JOIN logical_person_domains pd
	  ON pd.entry_key = k.entry_key AND pd.domain = k.domain
	GROUP BY ALL
)`,
		clusters,
		owners,
		facts,
		filterSQL,
		directEdges,
		conversationEdges,
	)
}

func buildIdentityRollupsSQL(paths ActivityPaths) string {
	return LogicalActivitySQL(paths, "true") + `,
people_totals AS (
	SELECT canonical_id,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_people
	GROUP BY canonical_id
), people_source_counts AS (
	SELECT canonical_id, source_type, count(*)::BIGINT AS source_count
	FROM logical_people
	GROUP BY canonical_id, source_type
), people_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM people_source_counts
	GROUP BY canonical_id
)
SELECT t.canonical_id, t.activity_count, t.file_count, t.first_at, t.last_at,
       s.source_counts
FROM people_totals t
JOIN people_sources s USING (canonical_id)
ORDER BY t.canonical_id`
}

func buildDomainRollupsSQL(paths ActivityPaths) string {
	return LogicalActivitySQL(paths, "true") + `,
domain_totals AS (
	SELECT domain,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_domains
	WHERE domain <> ''
	GROUP BY domain
), domain_source_counts AS (
	SELECT domain, source_type, count(*)::BIGINT AS source_count
	FROM logical_domains
	WHERE domain <> ''
	GROUP BY domain, source_type
), domain_sources AS (
	SELECT domain,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM domain_source_counts
	GROUP BY domain
), domain_people AS (
	SELECT domain, count(DISTINCT canonical_id)::BIGINT AS person_count
	FROM logical_domains
	CROSS JOIN unnest(canonical_ids) AS person(canonical_id)
	WHERE domain <> ''
	GROUP BY domain
)
SELECT t.domain, t.activity_count, coalesce(p.person_count, 0)::BIGINT AS person_count,
       t.file_count, t.first_at, t.last_at, s.source_counts
FROM domain_totals t
JOIN domain_sources s USING (domain)
LEFT JOIN domain_people p USING (domain)
ORDER BY t.domain`
}

func buildRelationshipRollupsSQL(paths ActivityPaths, anchor time.Time) string {
	anchorDate := anchor.UTC().Format(time.DateOnly)
	return LogicalActivitySQL(paths, "true") + fmt.Sprintf(`,
relationship_interactions AS (
	SELECT p.*,
	       CASE WHEN p.is_from_me
	                 AND p.entry_kind IN ('email','conversation','item')
	            THEN 1::BIGINT ELSE 0::BIGINT END AS sent_units,
	       CASE WHEN NOT p.is_from_me
	                 AND (p.entry_kind = 'conversation'
	                      OR (p.entry_kind IN ('email','item') AND p.is_author))
	            THEN 1::BIGINT ELSE 0::BIGINT END AS received_units,
	       CASE WHEN p.entry_kind IN ('event','meeting') AND p.with_owner
	            THEN 1::BIGINT ELSE 0::BIGINT END AS meeting_units,
	       CASE
	           WHEN p.entry_kind IN ('event','meeting') AND p.with_owner THEN %d::UTINYINT
	           WHEN p.entry_kind = 'conversation' THEN %d::UTINYINT
	           WHEN p.entry_kind IN ('email','item') THEN %d::UTINYINT
	           ELSE 0::UTINYINT
	       END AS modality_mask
	FROM logical_people p
	WHERE NOT p.is_owner
	  AND NOT (p.entry_kind IN ('event','meeting') AND NOT p.with_owner)
)
SELECT canonical_id,
       DATE '%s' AS anchor_date,
       sum(CASE WHEN occurred_at::DATE <= DATE '%s'
	                THEN sent_units * exp(-%.17g * greatest(
	                     0, date_diff('day', occurred_at,
	                                  CAST(DATE '%s' AS TIMESTAMP))))
	                ELSE 0 END)::DOUBLE AS sent_decayed,
       sum(CASE WHEN occurred_at::DATE <= DATE '%s'
	                THEN received_units * exp(-%.17g * greatest(
	                     0, date_diff('day', occurred_at,
	                                  CAST(DATE '%s' AS TIMESTAMP))))
	                ELSE 0 END)::DOUBLE AS received_decayed,
       sum(CASE WHEN occurred_at::DATE <= DATE '%s'
	                THEN meeting_units * exp(-%.17g * greatest(
	                     0, date_diff('day', occurred_at,
	                                  CAST(DATE '%s' AS TIMESTAMP))))
	                ELSE 0 END)::DOUBLE AS meetings_decayed,
       sum(sent_units)::BIGINT AS sent_count,
       sum(meeting_units)::BIGINT AS meeting_count,
       bit_or(modality_mask)::UTINYINT AS modality_mask,
       max(occurred_at)::TIMESTAMP AS last_at
FROM relationship_interactions
GROUP BY canonical_id
ORDER BY canonical_id`,
		ModalityMeeting,
		ModalityChat,
		ModalityEmail,
		anchorDate,
		anchorDate,
		RelationshipDecayRate,
		anchorDate,
		anchorDate,
		RelationshipDecayRate,
		anchorDate,
		anchorDate,
		RelationshipDecayRate,
		anchorDate,
	)
}

func buildRelationshipFutureSQL(paths ActivityPaths, anchor time.Time) string {
	anchorDate := anchor.UTC().Format(time.DateOnly)
	return LogicalActivitySQL(paths, "true") + fmt.Sprintf(`,
relationship_interactions AS (
	SELECT p.*,
	       CASE WHEN p.is_from_me
	                 AND p.entry_kind IN ('email','conversation','item')
	            THEN 1::BIGINT ELSE 0::BIGINT END AS sent_units,
	       CASE WHEN NOT p.is_from_me
	                 AND (p.entry_kind = 'conversation'
	                      OR (p.entry_kind IN ('email','item') AND p.is_author))
	            THEN 1::BIGINT ELSE 0::BIGINT END AS received_units,
	       CASE WHEN p.entry_kind IN ('event','meeting') AND p.with_owner
	            THEN 1::BIGINT ELSE 0::BIGINT END AS meeting_units,
	       CASE
	           WHEN p.entry_kind IN ('event','meeting') AND p.with_owner THEN %d::UTINYINT
	           WHEN p.entry_kind = 'conversation' THEN %d::UTINYINT
	           WHEN p.entry_kind IN ('email','item') THEN %d::UTINYINT
	           ELSE 0::UTINYINT
	       END AS modality_mask
	FROM logical_people p
	WHERE NOT p.is_owner
	  AND NOT (p.entry_kind IN ('event','meeting') AND NOT p.with_owner)
)
SELECT canonical_id,
       occurred_at::DATE AS event_date,
       sum(sent_units)::BIGINT AS sent_units,
       sum(received_units)::BIGINT AS received_units,
       sum(meeting_units)::BIGINT AS meeting_units,
       sum(sent_units)::BIGINT AS sent_count,
       sum(meeting_units)::BIGINT AS meeting_count,
       bit_or(modality_mask)::UTINYINT AS modality_mask,
       max(occurred_at)::TIMESTAMP AS last_at
FROM relationship_interactions
WHERE occurred_at::DATE > DATE '%s'
GROUP BY canonical_id, event_date
ORDER BY canonical_id, event_date`,
		ModalityMeeting,
		ModalityChat,
		ModalityEmail,
		anchorDate,
	)
}

func activityRelation(path string, hivePartitioning bool) string {
	if strings.HasPrefix(strings.TrimSpace(path), "read_parquet(") {
		return path
	}
	options := ""
	if hivePartitioning {
		options = ", hive_partitioning=true, union_by_name=true"
	}
	return "read_parquet('" + quoteSQLString(path) + "'" + options + ")"
}

func buildEntryFactsSQL(path func(string) string) string {
	return fmt.Sprintf(`
		SELECT
			m.id::BIGINT AS message_id,
			m.conversation_id::BIGINT AS conversation_id,
			m.source_id::BIGINT AS source_id,
			s.source_type::VARCHAR AS source_type,
			m.sent_at::TIMESTAMP AS occurred_at,
			m.message_type::VARCHAR AS message_type,
			coalesce(c.conversation_type, '')::VARCHAR AS conversation_type,
			%s AS entry_kind,
			(%s) AS is_chat,
			m.is_from_me::BOOLEAN AS is_from_me,
			m.has_attachments::BOOLEAN AS has_attachments,
			m.attachment_count::INTEGER AS attachment_count,
			(m.deleted_from_source_at IS NOT NULL) AS deleted_from_source,
			year(m.sent_at)::SMALLINT AS occurred_year
		FROM read_parquet('%s', hive_partitioning=true, union_by_name=true) m
		JOIN read_parquet('%s') s ON s.id = m.source_id
		LEFT JOIN read_parquet('%s') c ON c.id = m.conversation_id
	`,
		EntryKindSQL("m.message_type"),
		IsChatSQL("m.message_type", "coalesce(c.conversation_type, '')"),
		quoteSQLString(path("messages")),
		quoteSQLString(path("sources")),
		quoteSQLString(path("conversations")),
	)
}

func buildDirectEdgesSQL(path func(string) string, factsPath string) string {
	return fmt.Sprintf(`
		WITH raw AS (
			SELECT mr.message_id::BIGINT AS message_id,
			       mr.participant_id::BIGINT AS participant_id,
			       false AS is_sender,
			       mr.recipient_type = 'from' AS is_author
			FROM read_parquet('%s') mr
			UNION ALL
			SELECT m.id::BIGINT, m.sender_id::BIGINT, true, true
			FROM read_parquet('%s', hive_partitioning=true, union_by_name=true) m
			WHERE m.sender_id IS NOT NULL
		)
		SELECT r.message_id,
		       f.occurred_year,
		       r.participant_id,
		       lower(coalesce(p.domain, ''))::VARCHAR AS participant_domain,
		       bool_or(r.is_sender) AS is_sender,
		       bool_or(r.is_author) AS is_author
		FROM raw r
		JOIN read_parquet('%s', hive_partitioning=true, union_by_name=true) f
		  ON f.message_id = r.message_id
		JOIN read_parquet('%s') p ON p.id = r.participant_id
		GROUP BY r.message_id, f.occurred_year, r.participant_id, participant_domain
	`,
		quoteSQLString(path("message_recipients")),
		quoteSQLString(path("messages")),
		quoteSQLString(factsPath),
		quoteSQLString(path("participants")),
	)
}

func buildConversationEdgesSQL(path func(string) string) string {
	return fmt.Sprintf(`
		SELECT cp.conversation_id::BIGINT AS conversation_id,
		       cp.participant_id::BIGINT AS participant_id,
		       lower(coalesce(p.domain, ''))::VARCHAR AS participant_domain
		FROM read_parquet('%s') cp
		JOIN read_parquet('%s') p ON p.id = cp.participant_id
		GROUP BY cp.conversation_id, cp.participant_id, participant_domain
	`,
		quoteSQLString(path("conversation_participants")),
		quoteSQLString(path("participants")),
	)
}

func buildDirectorySQL(path func(string) string) string {
	return fmt.Sprintf(`
		WITH canon AS (
			SELECT p.id::BIGINT AS participant_id,
			       coalesce(c.canonical_id, p.id)::BIGINT AS canonical_id,
			       trim(coalesce(p.display_name, ''))::VARCHAR AS display_name,
			       trim(coalesce(p.phone_number, ''))::VARCHAR AS phone_number,
			       trim(coalesce(p.email_address, ''))::VARCHAR AS email_address
			FROM read_parquet('%s') p
			LEFT JOIN read_parquet('%s') c ON c.participant_id = p.id
		), members AS (
			SELECT canonical_id, list(participant_id ORDER BY participant_id) AS member_ids
			FROM canon
			GROUP BY canonical_id
		), named AS (
			SELECT canonical_id, display_name
			FROM (
				SELECT canonical_id, display_name,
				       row_number() OVER (
				           PARTITION BY canonical_id ORDER BY participant_id
				       ) AS position
				FROM canon
				WHERE display_name != ''
			)
			WHERE position = 1
		), fallback_candidates AS (
			SELECT canonical_id, participant_id, 1 AS priority,
			       ''::VARCHAR AS identifier_type, phone_number AS value
			FROM canon WHERE phone_number != ''
			UNION ALL
			SELECT canonical_id, participant_id, 2,
			       'email', email_address
			FROM canon WHERE email_address != ''
			UNION ALL
			SELECT c.canonical_id, c.participant_id,
			       CASE WHEN pi.is_primary THEN 3 ELSE 4 END,
			       lower(trim(pi.identifier_type)),
			       coalesce(nullif(trim(pi.display_value), ''),
			                trim(pi.identifier_value))
			FROM canon c
			JOIN read_parquet('%s') pi ON pi.participant_id = c.participant_id
			WHERE coalesce(nullif(trim(pi.display_value), ''),
			               trim(pi.identifier_value)) != ''
		), fallback AS (
			SELECT canonical_id, value AS display_label
			FROM (
				SELECT *,
				       row_number() OVER (
				           PARTITION BY canonical_id
				           ORDER BY priority, participant_id,
				                    identifier_type, value
				       ) AS position
				FROM fallback_candidates
			)
			WHERE position = 1
		), raw_searches AS (
			SELECT canonical_id, lower(display_name) AS value
			FROM canon WHERE display_name != ''
			UNION
			SELECT canonical_id, lower(email_address)
			FROM canon WHERE email_address != ''
			UNION
			SELECT canonical_id, lower(phone_number)
			FROM canon WHERE phone_number != ''
			UNION
			SELECT c.canonical_id, lower(trim(pi.identifier_value))
			FROM canon c
			JOIN read_parquet('%s') pi ON pi.participant_id = c.participant_id
			WHERE trim(coalesce(pi.identifier_value, '')) != ''
			UNION
			SELECT c.canonical_id, lower(trim(pi.display_value))
			FROM canon c
			JOIN read_parquet('%s') pi ON pi.participant_id = c.participant_id
			WHERE trim(coalesce(pi.display_value, '')) != ''
		), searches AS (
			SELECT canonical_id, list(value ORDER BY value) AS search_values
			FROM raw_searches
			GROUP BY canonical_id
		), owners AS (
			SELECT DISTINCT c.canonical_id
			FROM canon c
			JOIN read_parquet('%s') o ON o.participant_id = c.participant_id
		)
		SELECT m.canonical_id,
		       coalesce(n.display_name, f.display_label,
		                'Unknown person #' || m.canonical_id)::VARCHAR AS display_label,
		       (n.display_name IS NULL) AS partial_label,
		       m.member_ids,
		       coalesce(s.search_values, []::VARCHAR[]) AS search_values,
		       (o.canonical_id IS NOT NULL) AS is_owner
		FROM members m
		LEFT JOIN named n USING (canonical_id)
		LEFT JOIN fallback f USING (canonical_id)
		LEFT JOIN searches s USING (canonical_id)
		LEFT JOIN owners o USING (canonical_id)
		ORDER BY m.canonical_id
	`,
		quoteSQLString(path("participants")),
		quoteSQLString(path("participant_clusters")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("owner_participants")),
	)
}
