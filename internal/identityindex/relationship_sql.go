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
	       f.message_type::VARCHAR AS message_type,
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
	       arg_max(f.message_type,
	           struct_pack(occurred_at := f.occurred_at, message_id := f.message_id))::VARCHAR
	           AS message_type,
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

func buildLogicalActivityMaterializationSQL(path string) string {
	return logicalActivitySQL(path, "true") + `
SELECT 1::UTINYINT AS relation_kind,
       p.entry_key, p.anchor_message_id, p.conversation_id, p.source_id,
       p.source_type, p.occurred_at, p.message_type, p.entry_kind, p.is_from_me,
       p.attachment_count, p.canonical_id, p.is_author, p.is_owner,
       p.with_owner, NULL::VARCHAR AS domain
FROM logical_people p

UNION ALL

SELECT 2::UTINYINT AS relation_kind,
       u.entry_key, u.anchor_message_id, u.conversation_id, u.source_id,
       u.source_type, u.occurred_at, u.message_type, u.entry_kind, u.is_from_me,
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
       NULL::VARCHAR AS message_type, NULL::VARCHAR AS entry_kind,
       NULL::BOOLEAN AS is_from_me,
       NULL::BIGINT AS attachment_count, p.canonical_id,
       NULL::BOOLEAN AS is_author, NULL::BOOLEAN AS is_owner,
       NULL::BOOLEAN AS with_owner, p.domain
FROM logical_person_domains p
WHERE p.domain <> ''

UNION ALL

-- Keep the unit even when a conversation has no resolved participants yet.
-- A later append can then inherit its previous attachment total and anchor.
SELECT 4::UTINYINT AS relation_kind,
       u.entry_key, u.anchor_message_id, u.conversation_id, u.source_id,
       u.source_type, u.occurred_at, u.message_type, u.entry_kind, u.is_from_me,
       u.attachment_count, NULL::BIGINT AS canonical_id,
       NULL::BOOLEAN AS is_author, NULL::BOOLEAN AS is_owner,
       NULL::BOOLEAN AS with_owner, NULL::VARCHAR AS domain
FROM logical_units u
WHERE u.entry_kind = 'conversation'`
}
