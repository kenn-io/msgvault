package identityindex

import "fmt"

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
		JOIN read_parquet('%s') f ON f.message_id = r.message_id
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
