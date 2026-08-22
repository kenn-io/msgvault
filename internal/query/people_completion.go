package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/identityindex"
)

const (
	DefaultPeopleCompletionLimit  = 8
	MaxPeopleCompletionLimit      = 20
	MaxPeopleCompletionQueryRunes = 256
)

type PeopleCompletionKind string

const (
	PeopleCompletionName         PeopleCompletionKind = "name"
	PeopleCompletionPhone        PeopleCompletionKind = "phone"
	PeopleCompletionEmail        PeopleCompletionKind = "email"
	PeopleCompletionUsername     PeopleCompletionKind = "username"
	PeopleCompletionIMPP         PeopleCompletionKind = "impp"
	PeopleCompletionOrganization PeopleCompletionKind = "organization"
	PeopleCompletionTitle        PeopleCompletionKind = "title"
	PeopleCompletionRole         PeopleCompletionKind = "role"
)

func (k PeopleCompletionKind) Valid() bool {
	switch k {
	case PeopleCompletionName, PeopleCompletionPhone, PeopleCompletionEmail,
		PeopleCompletionUsername, PeopleCompletionIMPP,
		PeopleCompletionOrganization, PeopleCompletionTitle, PeopleCompletionRole:
		return true
	default:
		return false
	}
}

type PeopleCompletionRequest struct {
	Query string
	Limit int
}

type PeopleCompletion struct {
	ParticipantID int64
	DisplayLabel  string
	Kind          PeopleCompletionKind
	Value         string
	MatchValue    string
	Source        string
}

type PeopleCompletionResponse struct {
	Rows          []PeopleCompletion
	CacheRevision string
}

type PeopleCompleter interface {
	CompletePeople(context.Context, PeopleCompletionRequest) (*PeopleCompletionResponse, error)
}

var _ PeopleCompleter = (*DuckDBEngine)(nil)

func (e *DuckDBEngine) CompletePeople(
	ctx context.Context, request PeopleCompletionRequest,
) (*PeopleCompletionResponse, error) {
	queryText, limit, err := validatePeopleCompletionRequest(request)
	if err != nil {
		return nil, err
	}
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	release, err := e.acquireCacheRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}

	normalized := NormalizePeopleCompletionText(queryText)
	digits := peopleCompletionDigits(queryText)
	directory := quoteIdentitySQLPath(e.parquetPath(identityindex.DatasetPeople))
	rows, err := e.db.QueryContext(ctx, `
		WITH params AS (
			SELECT ?::VARCHAR AS query_text, ?::VARCHAR AS query_digits
		), candidates AS (
			SELECT d.canonical_id AS participant_id,
			       d.display_label,
			       primitive.kind,
			       primitive.display_value,
			       primitive.match_value,
			       primitive."source" AS source,
			       CASE
			           WHEN primitive.kind = 'phone' AND params.query_digits != '' THEN
			               CASE
			                   WHEN regexp_replace(primitive.match_value, '[^0-9]', '', 'g') = params.query_digits THEN 0
			                   WHEN starts_with(regexp_replace(primitive.match_value, '[^0-9]', '', 'g'), params.query_digits) THEN 1
			                   ELSE 2
			               END
			           WHEN primitive.match_value = params.query_text THEN 0
			           WHEN starts_with(primitive.match_value, params.query_text) THEN 1
			           ELSE 2
			       END AS match_rank,
			       CASE primitive.kind
			           WHEN 'name' THEN 0
			           WHEN 'phone' THEN 1 WHEN 'email' THEN 1
			           WHEN 'username' THEN 1 WHEN 'impp' THEN 1
			           WHEN 'organization' THEN 2 WHEN 'title' THEN 2 WHEN 'role' THEN 2
			           ELSE 3
			       END AS kind_rank,
			       CASE WHEN primitive."source" = 'observed' THEN 0 ELSE 1 END AS source_rank
			FROM read_parquet('`+directory+`') d,
			     unnest(d.search_primitives) AS completed(primitive), params
			WHERE contains(primitive.match_value, params.query_text)
			   OR (primitive.kind = 'phone' AND params.query_digits != ''
			       AND contains(regexp_replace(primitive.match_value, '[^0-9]', '', 'g'), params.query_digits))
		), deduplicated AS (
			SELECT *, row_number() OVER (
				PARTITION BY participant_id, kind, match_value
				ORDER BY source_rank, lower(display_value), source
			) AS duplicate_rank
			FROM candidates
		)
		SELECT participant_id, display_label, kind, display_value, match_value, source
		FROM deduplicated
		WHERE duplicate_rank = 1
		ORDER BY match_rank, kind_rank, lower(display_label), participant_id,
		         kind, lower(display_value), source
		LIMIT ?
	`, normalized, digits, limit)
	if err != nil {
		return nil, fmt.Errorf("complete analytical people: %w", err)
	}
	defer func() { _ = rows.Close() }()

	response := &PeopleCompletionResponse{
		Rows: make([]PeopleCompletion, 0, limit), CacheRevision: state.Revision(),
	}
	for rows.Next() {
		var row PeopleCompletion
		if err := rows.Scan(
			&row.ParticipantID, &row.DisplayLabel, &row.Kind,
			&row.Value, &row.MatchValue, &row.Source,
		); err != nil {
			return nil, fmt.Errorf("scan analytical people completion: %w", err)
		}
		if !row.Kind.Valid() {
			return nil, fmt.Errorf("scan analytical people completion: unknown kind %q", row.Kind)
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical people completions: %w", err)
	}
	return response, nil
}

func validatePeopleCompletionRequest(request PeopleCompletionRequest) (string, int, error) {
	queryText := strings.TrimSpace(request.Query)
	if queryText == "" {
		return "", 0, fmt.Errorf("%w: completion query is required", ErrInvalidExploreRequest)
	}
	if utf8.RuneCountInString(queryText) > MaxPeopleCompletionQueryRunes {
		return "", 0, fmt.Errorf("%w: completion query exceeds %d characters",
			ErrInvalidExploreRequest, MaxPeopleCompletionQueryRunes)
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultPeopleCompletionLimit
	}
	if limit < 1 || limit > MaxPeopleCompletionLimit {
		return "", 0, fmt.Errorf("%w: completion limit must be between 1 and %d",
			ErrInvalidExploreRequest, MaxPeopleCompletionLimit)
	}
	return queryText, limit, nil
}

func NormalizePeopleCompletionText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func PeopleCompletionMatchRank(
	kind PeopleCompletionKind, queryText, value string,
) (int, bool) {
	queryText = NormalizePeopleCompletionText(queryText)
	value = NormalizePeopleCompletionText(value)
	if queryText == "" || value == "" {
		return 0, false
	}
	if kind == PeopleCompletionPhone {
		queryDigits := peopleCompletionDigits(queryText)
		valueDigits := peopleCompletionDigits(value)
		if queryDigits != "" && valueDigits != "" {
			switch {
			case valueDigits == queryDigits:
				return 0, true
			case strings.HasPrefix(valueDigits, queryDigits):
				return 1, true
			case strings.Contains(valueDigits, queryDigits):
				return 2, true
			}
		}
	}
	switch {
	case value == queryText:
		return 0, true
	case strings.HasPrefix(value, queryText):
		return 1, true
	case strings.Contains(value, queryText):
		return 2, true
	default:
		return 0, false
	}
}

func PeopleCompletionKindRank(kind PeopleCompletionKind) int {
	switch kind {
	case PeopleCompletionName:
		return 0
	case PeopleCompletionPhone, PeopleCompletionEmail,
		PeopleCompletionUsername, PeopleCompletionIMPP:
		return 1
	case PeopleCompletionOrganization, PeopleCompletionTitle, PeopleCompletionRole:
		return 2
	default:
		return 3
	}
}

func SortPeopleCompletions(rows []PeopleCompletion, queryText string) {
	slices.SortStableFunc(rows, func(a, b PeopleCompletion) int {
		aMatch, _ := PeopleCompletionMatchRank(a.Kind, queryText, a.MatchValue)
		bMatch, _ := PeopleCompletionMatchRank(b.Kind, queryText, b.MatchValue)
		if order := cmp.Compare(aMatch, bMatch); order != 0 {
			return order
		}
		if order := cmp.Compare(PeopleCompletionKindRank(a.Kind), PeopleCompletionKindRank(b.Kind)); order != 0 {
			return order
		}
		if order := cmp.Compare(NormalizePeopleCompletionText(a.DisplayLabel), NormalizePeopleCompletionText(b.DisplayLabel)); order != 0 {
			return order
		}
		if order := cmp.Compare(a.ParticipantID, b.ParticipantID); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(NormalizePeopleCompletionText(a.Value), NormalizePeopleCompletionText(b.Value)); order != 0 {
			return order
		}
		return cmp.Compare(a.Source, b.Source)
	})
}

func peopleCompletionDigits(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	return digits.String()
}
