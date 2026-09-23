package identityindex

import "fmt"

// buildSparseRelationshipActivitySQL writes message facts once and keeps only
// direct canonical edges beside them. Conversation membership stays in the
// conversation_participants base dataset, where it is stored once per thread.
func buildSparseRelationshipActivitySQL(path func(string) string, occurredYear int64) string {
	return fmt.Sprintf(`
WITH message_facts AS (
	SELECT m.id::BIGINT AS message_id, m.conversation_id::BIGINT AS conversation_id,
	       m.source_id::BIGINT AS source_id, s.source_type::VARCHAR AS source_type,
	       m.sent_at::TIMESTAMP AS occurred_at, m.message_type::VARCHAR AS message_type,
	       coalesce(c.conversation_type, '')::VARCHAR AS conversation_type,
	       %s AS entry_kind, (%s) AS is_chat,
	       m.is_from_me::BOOLEAN AS is_from_me, m.attachment_count::INTEGER AS attachment_count,
	       coalesce(m.has_attachments::BOOLEAN, false) AS has_attachments,
	       (m.deleted_from_source_at IS NOT NULL) AS deleted_from_source,
	       year(m.sent_at)::SMALLINT AS occurred_year,
	       m.sender_id::BIGINT AS sender_id,
	       m.owner_participant_id::BIGINT AS owner_participant_id
	FROM read_parquet('%s', hive_partitioning=true, union_by_name=true) m
	JOIN read_parquet('%s') s ON s.id = m.source_id
	LEFT JOIN read_parquet('%s') c ON c.id = m.conversation_id
	WHERE year(m.sent_at) = %d
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
	SELECT mr.message_id::BIGINT AS message_id, mr.participant_id::BIGINT AS participant_id,
	       (mr.recipient_type = 'from' AND mr.participant_id = m.owner_participant_id) AS is_sender,
	       (mr.recipient_type = 'from' AND mr.participant_id = m.owner_participant_id) AS is_owner_sender,
	       (mr.recipient_type = 'from') AS is_author
	FROM read_parquet('%s') mr JOIN message_facts m ON m.message_id = mr.message_id
	UNION ALL
	SELECT m.message_id, m.sender_id, true,
	       coalesce(m.sender_id = m.owner_participant_id, false), true
	FROM message_facts m WHERE m.sender_id IS NOT NULL
), direct_canon AS (
	SELECT e.*, c.canonical_id, c.participant_domain
	FROM direct_edges e LEFT JOIN canon c USING (participant_id)
), direct_agg AS (
	SELECT message_id, canonical_id, participant_domain,
	       bool_or(is_sender) AS is_sender,
	       bool_or(is_owner_sender) AS is_owner_sender,
	       bool_or(is_author) AS is_author
	FROM direct_canon WHERE canonical_id IS NOT NULL
	GROUP BY message_id, canonical_id, participant_domain
)
SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
       m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
       m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
       m.deleted_from_source, d.canonical_id, d.participant_domain,
       true AS is_direct, false AS is_conversation_member,
       d.is_sender, d.is_author,
       (o.canonical_id IS NOT NULL OR (m.is_from_me AND d.is_owner_sender)) AS is_owner,
       m.occurred_year
FROM message_facts m JOIN direct_agg d USING (message_id)
LEFT JOIN owner_canon o ON o.canonical_id = d.canonical_id
UNION ALL
SELECT m.message_id, m.conversation_id, m.source_id, m.source_type,
       m.occurred_at, m.message_type, m.conversation_type, m.entry_kind,
       m.is_chat, m.is_from_me, m.attachment_count, m.has_attachments,
       m.deleted_from_source, NULL::BIGINT AS canonical_id,
       NULL::VARCHAR AS participant_domain,
       EXISTS (SELECT 1 FROM direct_canon d
               WHERE d.message_id = m.message_id AND d.canonical_id IS NULL) AS is_direct,
       false AS is_conversation_member, false AS is_sender,
       false AS is_author, false AS is_owner, m.occurred_year
FROM message_facts m`, EntryKindSQL("m.message_type"),
		IsChatSQL("m.message_type", "coalesce(c.conversation_type, '')"),
		quoteSQLString(path("messages")), quoteSQLString(path("sources")),
		quoteSQLString(path("conversations")), occurredYear,
		quoteSQLString(path("participants")), quoteSQLString(path("participant_clusters")),
		quoteSQLString(path("owner_participants")), quoteSQLString(path("message_recipients")))
}

// ExpandedActivityRelation restores the logical edge relation on demand from
// sparse message facts and the normalized roster. Its arguments are trusted
// SQL relations produced by the cache builder or query engine.
func ExpandedActivityRelation(sparse, roster, participants, clusters, owners string) string {
	return fmt.Sprintf(`(
WITH raw_activity AS (SELECT * FROM %[1]s),
facts AS (SELECT * FROM raw_activity WHERE canonical_id IS NULL),
direct AS (SELECT * FROM raw_activity WHERE canonical_id IS NOT NULL),
canon AS (
	SELECT p.id::BIGINT AS participant_id,
	       coalesce(c.canonical_id, p.id)::BIGINT AS canonical_id,
	       lower(coalesce(p.domain, ''))::VARCHAR AS participant_domain
	FROM %[3]s p LEFT JOIN %[4]s c ON c.participant_id = p.id
), owner_canon AS (
	SELECT DISTINCT c.canonical_id FROM %[5]s o
	JOIN canon c ON c.participant_id = o.participant_id
), members AS (
	SELECT DISTINCT cp.conversation_id::BIGINT AS conversation_id,
	       c.canonical_id, c.participant_domain,
	       (o.canonical_id IS NOT NULL) AS is_owner
	FROM %[2]s cp
	LEFT JOIN canon c ON c.participant_id = cp.participant_id
	LEFT JOIN owner_canon o ON o.canonical_id = c.canonical_id
)
SELECT f.message_id, f.conversation_id, f.source_id, f.source_type,
       f.occurred_at, f.message_type, f.conversation_type, f.entry_kind,
       f.is_chat, f.is_from_me, f.attachment_count, f.has_attachments,
       f.deleted_from_source, cm.canonical_id, cm.participant_domain,
       coalesce(d.is_direct, false) AS is_direct,
       true AS is_conversation_member, coalesce(d.is_sender, false) AS is_sender,
       coalesce(d.is_author, false) AS is_author,
       (cm.is_owner OR coalesce(d.is_owner, false)) AS is_owner,
       f.occurred_year
FROM facts f JOIN members cm ON cm.conversation_id = f.conversation_id
LEFT JOIN direct d ON d.message_id = f.message_id
                  AND d.canonical_id = cm.canonical_id
                  AND d.participant_domain = cm.participant_domain
WHERE cm.canonical_id IS NOT NULL
UNION ALL
SELECT d.message_id, d.conversation_id, d.source_id, d.source_type,
       d.occurred_at, d.message_type, d.conversation_type, d.entry_kind,
       d.is_chat, d.is_from_me, d.attachment_count, d.has_attachments,
       d.deleted_from_source, d.canonical_id, d.participant_domain,
       d.is_direct, false AS is_conversation_member,
       d.is_sender, d.is_author, d.is_owner, d.occurred_year
FROM direct d
WHERE NOT EXISTS (
	SELECT 1 FROM members cm WHERE cm.conversation_id = d.conversation_id
	AND cm.canonical_id = d.canonical_id
	AND cm.participant_domain = d.participant_domain
)
UNION ALL
SELECT f.message_id, f.conversation_id, f.source_id, f.source_type,
       f.occurred_at, f.message_type, f.conversation_type, f.entry_kind,
       f.is_chat, f.is_from_me, f.attachment_count, f.has_attachments,
       f.deleted_from_source, NULL::BIGINT AS canonical_id,
       NULL::VARCHAR AS participant_domain,
       false AS is_direct, false AS is_conversation_member,
       false AS is_sender, false AS is_author, false AS is_owner,
       f.occurred_year
FROM facts f
WHERE f.is_direct
   OR EXISTS (SELECT 1 FROM members cm
              WHERE cm.conversation_id = f.conversation_id AND cm.canonical_id IS NULL)
   OR (NOT EXISTS (SELECT 1 FROM direct d WHERE d.message_id = f.message_id)
       AND NOT EXISTS (SELECT 1 FROM members cm
                       WHERE cm.conversation_id = f.conversation_id
                         AND cm.canonical_id IS NOT NULL))
)`, sparse, roster, participants, clusters, owners)
}
