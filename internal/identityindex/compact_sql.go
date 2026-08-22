package identityindex

import (
	"fmt"
	"strings"
)

const (
	logicalBuildRelation   = "relationship_build_logical_activity"
	directoryBuildRelation = "relationship_build_directory"
)

func buildRelationshipPeopleSQL() string {
	return `
WITH logical_people AS (
	SELECT entry_key, source_id, source_type, occurred_at, message_type, attachment_count,
	       canonical_id
	FROM ` + logicalBuildRelation + `
	WHERE relation_kind = 1
), people_totals AS (
	SELECT canonical_id,
	       count(*)::BIGINT AS activity_count,
	       count(*) FILTER (WHERE message_type = 'meeting_transcript')::BIGINT AS meeting_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_people
	GROUP BY canonical_id
), source_rows AS (
	SELECT canonical_id, source_id, source_type,
	       count(*)::BIGINT AS activity_count,
	       count(*) FILTER (WHERE message_type = 'meeting_transcript')::BIGINT AS meeting_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_people
	GROUP BY canonical_id, source_id, source_type
), source_type_rows AS (
	SELECT canonical_id, source_type,
	       sum(activity_count)::BIGINT AS source_count
	FROM source_rows
	GROUP BY canonical_id, source_type
), source_counts AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM source_type_rows
	GROUP BY canonical_id
), source_rollups AS (
	SELECT canonical_id,
	       list(struct_pack(
		       source_id := source_id,
		       source_type := source_type,
		       activity_count := activity_count,
		       meeting_count := meeting_count,
		       file_count := file_count,
		       first_at := first_at,
		       last_at := last_at
	       ) ORDER BY source_id, source_type) AS source_rollups
	FROM source_rows
	GROUP BY canonical_id
)
SELECT d.canonical_id, d.display_label, d.partial_label, d.member_ids,
	   d.search_values, d.search_primitives, d.is_owner,
	   t.activity_count, t.meeting_count, t.file_count,
       t.first_at, t.last_at, c.source_counts, r.source_rollups
FROM ` + directoryBuildRelation + ` d
JOIN people_totals t USING (canonical_id)
JOIN source_counts c USING (canonical_id)
JOIN source_rollups r USING (canonical_id)
ORDER BY d.canonical_id`
}

func buildRelationshipDomainsSQL() string {
	return `
WITH domain_entries AS (
	SELECT entry_key, domain, source_type, attachment_count, occurred_at
	FROM ` + logicalBuildRelation + `
	WHERE relation_kind = 2
), logical_person_domains AS (
	SELECT entry_key, canonical_id, domain
	FROM ` + logicalBuildRelation + `
	WHERE relation_kind = 3
), domain_totals AS (
	SELECT domain,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM domain_entries
	GROUP BY domain
), domain_source_rows AS (
	SELECT domain, source_type, count(*)::BIGINT AS source_count
	FROM domain_entries
	GROUP BY domain, source_type
), domain_sources AS (
	SELECT domain,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM domain_source_rows
	GROUP BY domain
), domain_people AS (
	SELECT domain, count(DISTINCT canonical_id)::BIGINT AS person_count
	FROM logical_person_domains
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

func buildRelationshipDailySQL() string {
	return fmt.Sprintf(`
WITH logical_people AS (
	SELECT entry_key, occurred_at, entry_kind, is_from_me, canonical_id,
	       is_author, is_owner, with_owner
	FROM `+logicalBuildRelation+`
	WHERE relation_kind = 1
), interactions AS (
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
       bit_or(modality_mask)::UTINYINT AS modality_mask,
       max(occurred_at)::TIMESTAMP AS last_at
FROM interactions
GROUP BY canonical_id, event_date
ORDER BY canonical_id, event_date`,
		ModalityMeeting,
		ModalityChat,
		ModalityEmail,
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
	-- The identifier fallback matches the legacy label policy: only the
	-- canonical participant's own phone, email, and identifiers are
	-- considered, while the best display name above spans the whole cluster.
	SELECT canonical_id, participant_id, 1 AS priority,
	       ''::VARCHAR AS identifier_type, phone_number AS value
	FROM canon WHERE participant_id = canonical_id AND phone_number != ''
	UNION ALL
	SELECT canonical_id, participant_id, 2,
	       'email', email_address
	FROM canon WHERE participant_id = canonical_id AND email_address != ''
	UNION ALL
	SELECT c.canonical_id, c.participant_id,
	       CASE WHEN pi.is_primary THEN 3 ELSE 4 END,
	       lower(trim(pi.identifier_type)),
	       coalesce(nullif(trim(pi.display_value), ''),
	                trim(pi.identifier_value))
	FROM canon c
	JOIN read_parquet('%s') pi ON pi.participant_id = c.participant_id
	WHERE c.participant_id = c.canonical_id
	  AND coalesce(nullif(trim(pi.display_value), ''),
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
), raw_primitives AS (
	SELECT canonical_id, participant_id, 'name'::VARCHAR AS kind,
	       lower(display_name)::VARCHAR AS match_value,
	       display_name::VARCHAR AS display_value,
	       'observed'::VARCHAR AS source
	FROM canon WHERE display_name != ''
	UNION ALL
	SELECT canonical_id, participant_id, 'email', lower(email_address),
	       email_address, 'observed'
	FROM canon WHERE email_address != ''
	UNION ALL
	SELECT canonical_id, participant_id, 'phone', lower(phone_number),
	       phone_number, 'observed'
	FROM canon WHERE phone_number != ''
	UNION ALL
	SELECT c.canonical_id, c.participant_id,
	       CASE lower(trim(pi.identifier_type))
	           WHEN 'email' THEN 'email'
	           WHEN 'phone' THEN 'phone'
	           WHEN 'impp' THEN 'impp'
	           ELSE 'username'
	       END::VARCHAR AS kind,
	       lower(trim(pi.identifier_value))::VARCHAR AS match_value,
	       coalesce(nullif(trim(pi.display_value), ''),
	                trim(pi.identifier_value))::VARCHAR AS display_value,
	       coalesce(nullif(lower(trim(pi.identifier_type)), ''),
	                'observed')::VARCHAR AS source
	FROM canon c
	JOIN read_parquet('%s') pi ON pi.participant_id = c.participant_id
	WHERE trim(coalesce(pi.identifier_value, '')) != ''
), primitives AS (
	SELECT canonical_id,
	       list(struct_pack(
	           kind := kind,
	           match_value := match_value,
	           display_value := display_value,
	           source := source,
	           participant_id := participant_id
	       ) ORDER BY participant_id,
	                  CASE kind WHEN 'name' THEN 0 WHEN 'email' THEN 1
	                            WHEN 'phone' THEN 2 WHEN 'impp' THEN 3 ELSE 4 END,
	                  CASE WHEN source = 'observed' THEN 0 ELSE 1 END,
	                  match_value, source, display_value) AS search_primitives
	FROM (SELECT DISTINCT canonical_id, participant_id, kind, match_value,
	             display_value, source FROM raw_primitives)
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
       coalesce(p.search_primitives, []::STRUCT(
	       kind VARCHAR, match_value VARCHAR, display_value VARCHAR,
	       source VARCHAR, participant_id BIGINT)[]) AS search_primitives,
       (o.canonical_id IS NOT NULL) AS is_owner
FROM members m
LEFT JOIN named n USING (canonical_id)
LEFT JOIN fallback f USING (canonical_id)
LEFT JOIN searches s USING (canonical_id)
LEFT JOIN primitives p USING (canonical_id)
LEFT JOIN owners o USING (canonical_id)
ORDER BY m.canonical_id`,
		quoteSQLString(path("participants")),
		quoteSQLString(path("participant_clusters")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("participant_identifiers")),
		quoteSQLString(path("owner_participants")),
	)
}
