package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	DefaultPersonCompletionLimit  = 8
	MaxPersonCompletionLimit      = 20
	MaxPersonCompletionQueryRunes = 256
)

var ErrInvalidPersonCompletionQuery = errors.New("invalid person completion query")

type PersonCompletionQuery struct {
	Query string
	Limit int
}

type PersonCompletion struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayLabel  string `json:"display_label"`
	Kind          string `json:"kind"`
	Value         string `json:"value"`
	MatchValue    string `json:"match_value"`
	Source        string `json:"source"`
}

func (s *Store) CompletePersonProfilesContext(
	ctx context.Context, request PersonCompletionQuery,
) ([]PersonCompletion, error) {
	queryText, queryUsername, queryDigits, limit, err := validatePersonCompletionQuery(request)
	if err != nil {
		return nil, err
	}
	currentEmployment := s.dialect.BoolTrueExpr("e.is_current")
	rows, err := s.db.QueryContext(ctx, `
		WITH bindings AS (
			SELECT person_id, MIN(participant_id) AS participant_id
			FROM person_participants GROUP BY person_id
		), raw_primitives AS (
			SELECT b.participant_id, p.display_name AS profile_label,
			       'name' AS kind,
			       COALESCE(
			           NULLIF(TRIM(n.formatted), ''),
			           NULLIF(TRIM(COALESCE(n.given_name, '') || ' ' || COALESCE(n.family_name, '')), ''),
			           NULLIF(TRIM(n.original_value), '')
			       ) AS value,
			       LOWER(COALESCE(
			           NULLIF(TRIM(n.formatted), ''),
			           NULLIF(TRIM(COALESCE(n.given_name, '') || ' ' || COALESCE(n.family_name, '')), ''),
			           NULLIF(TRIM(n.original_value), '')
			       )) AS match_value,
			       CAST(n.name_kind AS TEXT) AS source
			FROM person_names n
			JOIN persons p ON p.id = n.person_id
			JOIN bindings b ON b.person_id = p.id
			WHERE n.active_until IS NULL AND n.superseded_at IS NULL
			  AND n.name_kind IN ('formatted', 'structured', 'nickname', 'phonetic')
			UNION ALL
			SELECT b.participant_id, p.display_name,
			       CAST(cp.address_kind AS TEXT), cp.original_value,
			       LOWER(cp.normalized_value), COALESCE(cs.slug, 'profile')
			FROM person_contact_points cp
			JOIN persons p ON p.id = cp.person_id
			JOIN bindings b ON b.person_id = p.id
			LEFT JOIN communication_services cs ON cs.id = cp.service_id
			WHERE cp.active_until IS NULL AND cp.superseded_at IS NULL
			  AND cp.address_kind IN ('email', 'phone', 'username', 'impp')
			UNION ALL
			SELECT b.participant_id, p.display_name, 'organization', o.name,
			       LOWER(o.name_normalized), 'profile'
			FROM employments e
			JOIN persons p ON p.id = e.person_id
			JOIN bindings b ON b.person_id = p.id
			JOIN organizations o ON o.id = e.organization_id
			WHERE `+currentEmployment+`
			  AND o.merged_into_id IS NULL AND o.retired_at IS NULL
			UNION ALL
			SELECT b.participant_id, p.display_name, 'title', e.title,
			       LOWER(e.title_normalized), 'profile'
			FROM employments e
			JOIN persons p ON p.id = e.person_id
			JOIN bindings b ON b.person_id = p.id
			JOIN organizations o ON o.id = e.organization_id
			WHERE `+currentEmployment+` AND e.title IS NOT NULL
			  AND TRIM(e.title) != ''
			  AND o.merged_into_id IS NULL AND o.retired_at IS NULL
			UNION ALL
			SELECT b.participant_id, p.display_name, 'role', e.role,
			       LOWER(TRIM(e.role)), 'profile'
			FROM employments e
			JOIN persons p ON p.id = e.person_id
			JOIN bindings b ON b.person_id = p.id
			JOIN organizations o ON o.id = e.organization_id
			WHERE `+currentEmployment+` AND e.role IS NOT NULL
			  AND TRIM(e.role) != ''
			  AND o.merged_into_id IS NULL AND o.retired_at IS NULL
		), params AS (
			SELECT ? AS query_text, ? AS query_username, ? AS query_digits
		), candidates AS (
			SELECT participant_id,
			       COALESCE(NULLIF(TRIM(profile_label), ''), value,
			                'Person #' || CAST(participant_id AS TEXT)) AS display_label,
			       kind, value, match_value, source,
			       CASE
			           WHEN kind = 'phone' THEN
			               CASE WHEN REPLACE(match_value, '+', '') = query_digits THEN 0
			                    WHEN REPLACE(match_value, '+', '') LIKE query_digits || '%' THEN 1
			                    ELSE 2 END
			           WHEN kind = 'username' THEN
			               CASE WHEN match_value = query_username THEN 0
			                    WHEN match_value LIKE query_username || '%' THEN 1
			                    ELSE 2 END
			           ELSE CASE WHEN match_value = query_text THEN 0
			                     WHEN match_value LIKE query_text || '%' THEN 1
			                     ELSE 2 END
			       END AS match_rank,
			       CASE kind WHEN 'name' THEN 0
			                 WHEN 'phone' THEN 1 WHEN 'email' THEN 1
			                 WHEN 'username' THEN 1 WHEN 'impp' THEN 1
			                 ELSE 2 END AS kind_rank
			FROM raw_primitives, params
			WHERE value IS NOT NULL AND TRIM(value) != '' AND (
				(kind = 'phone' AND query_digits != ''
				 AND REPLACE(match_value, '+', '') LIKE '%' || query_digits || '%')
				OR (kind = 'username' AND query_username != ''
				 AND match_value LIKE '%' || query_username || '%')
				OR (kind NOT IN ('phone', 'username')
				 AND match_value LIKE '%' || query_text || '%')
			)
		), deduplicated AS (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY participant_id, kind, match_value
				ORDER BY source, LOWER(value)
			) AS duplicate_rank
			FROM candidates
		)
		SELECT participant_id, display_label, kind, value, match_value, source
		FROM deduplicated
		WHERE duplicate_rank = 1
		ORDER BY match_rank, kind_rank, LOWER(display_label), participant_id,
		         kind, LOWER(value), source
		LIMIT ?
	`, queryText, queryUsername, queryDigits, limit)
	if err != nil {
		return nil, fmt.Errorf("complete person profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]PersonCompletion, 0, limit)
	for rows.Next() {
		var row PersonCompletion
		if err := rows.Scan(
			&row.ParticipantID, &row.DisplayLabel, &row.Kind,
			&row.Value, &row.MatchValue, &row.Source,
		); err != nil {
			return nil, fmt.Errorf("scan person profile completion: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person profile completions: %w", err)
	}
	return result, nil
}

func validatePersonCompletionQuery(
	request PersonCompletionQuery,
) (queryText, queryUsername, queryDigits string, limit int, err error) {
	queryText = strings.ToLower(strings.Join(strings.Fields(request.Query), " "))
	if queryText == "" || utf8.RuneCountInString(strings.TrimSpace(request.Query)) > MaxPersonCompletionQueryRunes {
		return "", "", "", 0, ErrInvalidPersonCompletionQuery
	}
	limit = request.Limit
	if limit == 0 {
		limit = DefaultPersonCompletionLimit
	}
	if limit < 1 || limit > MaxPersonCompletionLimit {
		return "", "", "", 0, ErrInvalidPersonCompletionQuery
	}
	queryUsername = strings.TrimLeft(queryText, "@")
	var digits strings.Builder
	for _, r := range request.Query {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	return queryText, queryUsername, digits.String(), limit, nil
}
