package identityindex

import (
	"fmt"
	"strings"
)

// logicalActivitySQL returns CTEs named logical_units, logical_people, and
// logical_domains over the canonical activity dataset at activityPath.
// filterSQL is trusted SQL rendered by the query layer and may refer to the
// message-level alias f.
func logicalActivitySQL(activityPath, filterSQL string) string {
	if strings.TrimSpace(filterSQL) == "" {
		filterSQL = "true"
	}
	activity := activityRelation(activityPath, true)
	return fmt.Sprintf(`
WITH filtered_facts AS (
	SELECT f.message_id, f.conversation_id, f.source_id, f.source_type,
	       f.occurred_at, f.message_type, f.conversation_type, f.entry_kind,
	       f.is_chat, f.is_from_me, f.attachment_count, f.deleted_from_source
	FROM %[1]s f
	WHERE %[2]s
	GROUP BY ALL
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
	SELECT u.entry_key, f.canonical_id, f.is_author, f.is_owner
	FROM nonchat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL
	  AND f.is_direct

	UNION ALL

	SELECT u.entry_key, f.canonical_id,
	       (f.message_id = u.anchor_message_id AND f.is_author) AS is_author,
	       f.is_owner
	FROM chat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL

	UNION ALL

	SELECT u.entry_key, f.canonical_id,
	       (f.message_id = u.anchor_message_id AND f.is_author) AS is_author,
	       f.is_owner
	FROM chat_units u
	JOIN filtered_facts selected
	  ON selected.source_id = u.source_id
	 AND selected.conversation_id = u.conversation_id
	 AND selected.is_chat
	JOIN %[1]s f ON f.message_id = selected.message_id
	WHERE f.canonical_id IS NOT NULL
	  AND f.is_direct
	  AND f.message_id <> u.anchor_message_id
), logical_people_grouped AS (
	SELECT entry_key, canonical_id,
	       bool_or(is_author) AS is_author,
	       bool_or(is_owner) AS is_owner
	FROM logical_people_candidates
	GROUP BY entry_key, canonical_id
), logical_owner_presence AS (
	SELECT DISTINCT entry_key
	FROM logical_people_grouped
	WHERE is_owner
), logical_people AS (
	SELECT u.*, p.canonical_id, p.is_author, p.is_owner,
	       (op.entry_key IS NOT NULL) AS with_owner
	FROM logical_people_grouped p
	JOIN logical_units u USING (entry_key)
	LEFT JOIN logical_owner_presence op USING (entry_key)
), logical_person_domain_candidates AS (
	SELECT u.entry_key, f.canonical_id, f.participant_domain AS domain
	FROM nonchat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL
	  AND f.is_direct

	UNION ALL

	SELECT u.entry_key, f.canonical_id, f.participant_domain AS domain
	FROM chat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL

	UNION ALL

	SELECT u.entry_key, f.canonical_id, f.participant_domain AS domain
	FROM chat_units u
	JOIN filtered_facts selected
	  ON selected.source_id = u.source_id
	 AND selected.conversation_id = u.conversation_id
	 AND selected.is_chat
	JOIN %[1]s f ON f.message_id = selected.message_id
	WHERE f.canonical_id IS NOT NULL
	  AND f.is_direct
	  AND f.message_id <> u.anchor_message_id
), logical_person_domains AS (
	SELECT DISTINCT entry_key, canonical_id, domain
	FROM logical_person_domain_candidates
), logical_domain_candidates AS (
	SELECT u.entry_key, f.participant_domain AS domain
	FROM nonchat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL

	UNION ALL

	SELECT u.entry_key, f.participant_domain AS domain
	FROM chat_units u
	JOIN %[1]s f ON f.message_id = u.anchor_message_id
	WHERE f.canonical_id IS NOT NULL

	UNION ALL

	SELECT u.entry_key, f.participant_domain AS domain
	FROM chat_units u
	JOIN filtered_facts selected
	  ON selected.source_id = u.source_id
	 AND selected.conversation_id = u.conversation_id
	 AND selected.is_chat
	JOIN %[1]s f ON f.message_id = selected.message_id
	WHERE f.canonical_id IS NOT NULL
	  AND f.is_direct
	  AND f.message_id <> u.anchor_message_id
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
		activity,
		filterSQL,
	)
}

func buildRelationshipActivitySQL(path func(string) string, occurredYear int64) string {
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
	       m.sender_id::BIGINT AS sender_id,
	       m.owner_participant_id::BIGINT AS owner_participant_id
	FROM read_parquet('%s', hive_partitioning=true, union_by_name=true) m
	JOIN read_parquet('%s') s ON s.id = m.source_id
	LEFT JOIN read_parquet('%s') c ON c.id = m.conversation_id
	WHERE year(m.sent_at) = %d
), scoped_conversations AS (
	SELECT DISTINCT conversation_id
	FROM message_facts
	WHERE conversation_id IS NOT NULL
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
), direct_edges AS (
	SELECT mr.message_id::BIGINT AS message_id,
	       mr.participant_id::BIGINT AS participant_id,
	       true AS is_direct,
	       (mr.recipient_type = 'from'
	        AND mr.participant_id = m.owner_participant_id) AS is_sender,
	       (mr.recipient_type = 'from'
	        AND mr.participant_id = m.owner_participant_id) AS is_owner_sender,
	       (mr.recipient_type = 'from') AS is_author
	FROM read_parquet('%s') mr
	JOIN message_facts m ON m.message_id = mr.message_id

	UNION ALL

	SELECT m.message_id, m.sender_id, true, true,
	       coalesce(m.sender_id = m.owner_participant_id, false), true
	FROM message_facts m
	WHERE m.sender_id IS NOT NULL
), direct_canon AS (
	SELECT e.message_id, e.is_direct, e.is_sender, e.is_owner_sender, e.is_author,
	       c.canonical_id, c.participant_domain
	FROM direct_edges e
	LEFT JOIN canon c USING (participant_id)
), direct_agg AS (
	SELECT message_id, canonical_id, participant_domain,
	       bool_or(is_direct) AS is_direct,
	       bool_or(is_sender) AS is_sender,
	       bool_or(is_owner_sender) AS is_owner_sender,
	       bool_or(is_author) AS is_author
	FROM direct_canon
	WHERE canonical_id IS NOT NULL
	GROUP BY message_id, canonical_id, participant_domain
), conv_members AS (
	SELECT cp.conversation_id::BIGINT AS conversation_id,
	       c.canonical_id, c.participant_domain
	FROM read_parquet('%s') cp
	JOIN scoped_conversations sc ON sc.conversation_id = cp.conversation_id
	LEFT JOIN canon c ON c.participant_id = cp.participant_id
), conv_members_canon AS (
	SELECT DISTINCT conversation_id, canonical_id, participant_domain
	FROM conv_members
	WHERE canonical_id IS NOT NULL
)
SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
       m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
       m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
       m.deleted_from_source,
       cm.canonical_id, cm.participant_domain,
       coalesce(d.is_direct, false) AS is_direct,
       true AS is_conversation_member,
       coalesce(d.is_sender, false) AS is_sender,
       coalesce(d.is_author, false) AS is_author,
       (o.canonical_id IS NOT NULL OR
        (m.is_from_me AND coalesce(d.is_owner_sender, false))) AS is_owner,
       m.occurred_year
FROM message_facts m
	JOIN conv_members_canon cm USING (conversation_id)
	LEFT JOIN direct_agg d
	  ON d.message_id = m.message_id
	 AND d.canonical_id = cm.canonical_id
	 AND d.participant_domain = cm.participant_domain
	LEFT JOIN owner_canon o ON o.canonical_id = cm.canonical_id

UNION ALL

SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
       m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
       m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
       m.deleted_from_source,
       d.canonical_id, d.participant_domain,
       d.is_direct,
       false AS is_conversation_member,
       d.is_sender,
       d.is_author,
       (o.canonical_id IS NOT NULL OR
        (m.is_from_me AND d.is_owner_sender)) AS is_owner,
       m.occurred_year
FROM message_facts m
	JOIN direct_agg d USING (message_id)
	LEFT JOIN conv_members_canon cm
	  ON cm.conversation_id = m.conversation_id
	 AND cm.canonical_id = d.canonical_id
	 AND cm.participant_domain = d.participant_domain
	LEFT JOIN owner_canon o ON o.canonical_id = d.canonical_id
WHERE cm.canonical_id IS NULL

UNION ALL

SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
       m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
       m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
       m.deleted_from_source,
       NULL::BIGINT AS canonical_id,
       NULL::VARCHAR AS participant_domain,
       false AS is_direct,
       false AS is_conversation_member,
       false AS is_sender,
       false AS is_author,
       false AS is_owner,
       m.occurred_year
FROM message_facts m
WHERE EXISTS (
	SELECT 1
	FROM direct_canon d
	WHERE d.message_id = m.message_id AND d.canonical_id IS NULL
)
OR EXISTS (
	SELECT 1
	FROM conv_members cm
	WHERE cm.conversation_id = m.conversation_id
	  AND cm.canonical_id IS NULL
)
OR (
	NOT EXISTS (
		SELECT 1 FROM direct_agg d WHERE d.message_id = m.message_id
	)
	AND NOT EXISTS (
		SELECT 1
		FROM conv_members_canon cm
		WHERE cm.conversation_id = m.conversation_id
	)
)`,
		EntryKindSQL("m.message_type"),
		IsChatSQL("m.message_type", "coalesce(c.conversation_type, '')"),
		quoteSQLString(path("messages")),
		quoteSQLString(path("sources")),
		quoteSQLString(path("conversations")),
		occurredYear,
		quoteSQLString(path("participants")),
		quoteSQLString(path("participant_clusters")),
		quoteSQLString(path("owner_participants")),
		quoteSQLString(path("message_recipients")),
		quoteSQLString(path("conversation_participants")),
	)
}

func buildLogicalActivityMaterializationSQL(path string) string {
	return logicalActivitySQL(path, "true") + `
SELECT 1::UTINYINT AS relation_kind,
       p.entry_key, p.anchor_message_id, p.conversation_id, p.source_id,
       p.source_type, p.occurred_at, p.entry_kind, p.is_from_me,
       p.attachment_count, p.canonical_id, p.is_author, p.is_owner,
       p.with_owner, NULL::VARCHAR AS domain
FROM logical_people p

UNION ALL

SELECT 2::UTINYINT AS relation_kind,
       u.entry_key, u.anchor_message_id, u.conversation_id, u.source_id,
       u.source_type, u.occurred_at, u.entry_kind, u.is_from_me,
       u.attachment_count, NULL::BIGINT AS canonical_id,
       NULL::BOOLEAN AS is_author, NULL::BOOLEAN AS is_owner,
       NULL::BOOLEAN AS with_owner, k.domain
FROM logical_domain_keys k
JOIN logical_units u USING (entry_key)
WHERE k.domain <> ''

UNION ALL

SELECT 3::UTINYINT AS relation_kind,
       p.entry_key, NULL::BIGINT AS anchor_message_id,
       NULL::BIGINT AS conversation_id, NULL::BIGINT AS source_id,
       NULL::VARCHAR AS source_type, NULL::TIMESTAMP AS occurred_at,
       NULL::VARCHAR AS entry_kind, NULL::BOOLEAN AS is_from_me,
       NULL::BIGINT AS attachment_count, p.canonical_id,
       NULL::BOOLEAN AS is_author, NULL::BOOLEAN AS is_owner,
       NULL::BOOLEAN AS with_owner, p.domain
FROM logical_person_domains p
WHERE p.domain <> ''`
}
