package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultDirectoryPeopleLimit        = 50
	MaxDirectoryPeopleLimit            = 100
	maxDirectoryQueryRunes             = 256
	maxDirectoryCursorBytes            = 1024
	maxDirectoryRawCandidateChunkSize  = 64
	directoryCursorVersion             = 6
	directoryLastContactKeyLayout      = "2006-01-02T15:04:05.000000000Z"
	DirectoryPeopleSortName            = "name"
	DirectoryPeopleSortLastContactAsc  = "last_contact_asc"
	DirectoryPeopleSortLastContactDesc = "last_contact_desc"
)

// DirectoryPeopleQuery selects durable people for the Directory read surface.
// All text fields are case- and whitespace-normalized before matching.
type DirectoryPeopleQuery struct {
	Query             string     `json:"query,omitempty"`
	Cursor            string     `json:"cursor,omitempty"`
	Limit             int        `json:"limit,omitempty"`
	ContactState      string     `json:"contact_state,omitempty"`
	Category          string     `json:"category,omitempty"`
	Organization      string     `json:"organization,omitempty"`
	PrimaryChannel    string     `json:"primary_channel,omitempty"`
	LastContactAfter  *time.Time `json:"last_contact_after,omitempty"`
	LastContactBefore *time.Time `json:"last_contact_before,omitempty"`
	Sort              string     `json:"sort,omitempty"`
}

// DirectoryPersonSummary is the non-sensitive, directory-sized projection of
// a durable person root. ContactState is "active" when a contact projection
// has a last-contact timestamp and "inactive" otherwise.
type DirectoryPersonSummary struct {
	ID             int64      `json:"id"`
	DisplayName    *string    `json:"display_name,omitempty"`
	Revision       int64      `json:"revision"`
	PrimaryChannel string     `json:"primary_channel,omitempty"`
	ContactState   string     `json:"contact_state"`
	LastContactAt  *time.Time `json:"last_contact_at,omitempty"`
	Categories     []string   `json:"categories" nullable:"false"`
	Organizations  []string   `json:"organizations" nullable:"false"`
}

// DirectoryPeoplePage is one stable keyset page of directory people.
type DirectoryPeoplePage struct {
	People     []DirectoryPersonSummary `json:"people"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

type normalizedDirectoryPeopleQuery struct {
	query             string
	terms             []string
	cursor            string
	limit             int
	contactState      string
	category          string
	organization      string
	primaryChannel    string
	lastContactAfter  string
	lastContactBefore string
	sort              string
	fingerprint       string
}

type directoryPeopleCursor struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	Quality     int    `json:"quality"`
	AnchorHash  string `json:"anchor_hash"`
	OrderKey    string `json:"-"`
	SortKey     string `json:"-"`
	PersonID    int64  `json:"person_id"`
}

type directoryPersonCandidate struct {
	summary   DirectoryPersonSummary
	quality   int
	sortKey   string
	orderName string
}

type directoryRawCandidateCursor struct {
	quality   int
	sortKey   string
	orderName string
	personID  int64
}

// DirectoryPeoplePageContext returns a deterministic, cursor-paginated page
// over promoted people. Its SQL uses only portable normalization and matching
// so SQLite and PostgreSQL share lexical, filtering, and cursor semantics.
func (s *Store) DirectoryPeoplePageContext(
	ctx context.Context,
	query DirectoryPeopleQuery,
) (*DirectoryPeoplePage, error) {
	normalized, err := normalizeDirectoryPeopleQuery(query)
	if err != nil {
		return nil, err
	}
	return s.directoryPeoplePageContext(ctx, normalized)
}

func normalizeDirectoryPeopleQuery(query DirectoryPeopleQuery) (normalizedDirectoryPeopleQuery, error) {
	normalized := normalizedDirectoryPeopleQuery{
		query:          normalizeDirectoryText(query.Query),
		cursor:         strings.TrimSpace(query.Cursor),
		contactState:   normalizeDirectoryText(query.ContactState),
		category:       normalizeDirectoryText(query.Category),
		organization:   normalizeDirectoryText(query.Organization),
		primaryChannel: normalizeDirectoryText(query.PrimaryChannel),
		sort:           strings.TrimSpace(query.Sort),
	}
	if normalized.sort == "" {
		normalized.sort = DirectoryPeopleSortName
	}
	if normalized.sort != DirectoryPeopleSortName && normalized.sort != DirectoryPeopleSortLastContactAsc && normalized.sort != DirectoryPeopleSortLastContactDesc {
		return normalized, fmt.Errorf("%w: unknown sort", ErrInvalidDirectoryQuery)
	}
	if query.LastContactAfter != nil {
		normalized.lastContactAfter = directoryLastContactKey(*query.LastContactAfter)
	}
	if query.LastContactBefore != nil {
		normalized.lastContactBefore = directoryLastContactKey(*query.LastContactBefore)
	}
	if normalized.lastContactAfter != "" && normalized.lastContactBefore != "" && query.LastContactAfter.After(*query.LastContactBefore) {
		return normalized, fmt.Errorf("%w: last contact range is empty", ErrInvalidDirectoryQuery)
	}
	if utf8.RuneCountInString(normalized.query) > maxDirectoryQueryRunes {
		return normalized, fmt.Errorf("%w: query is too long", ErrInvalidDirectoryQuery)
	}
	if normalized.contactState != "" && normalized.contactState != "active" && normalized.contactState != "inactive" {
		return normalized, fmt.Errorf("%w: unknown contact state", ErrInvalidDirectoryQuery)
	}
	normalized.terms = directoryTokens(query.Query)
	normalized.query = strings.Join(normalized.terms, " ")
	normalized.limit = query.Limit
	if normalized.limit <= 0 {
		normalized.limit = DefaultDirectoryPeopleLimit
	}
	if normalized.limit > MaxDirectoryPeopleLimit {
		normalized.limit = MaxDirectoryPeopleLimit
	}
	encoded, err := json.Marshal(struct {
		Query             string `json:"query"`
		ContactState      string `json:"contact_state"`
		Category          string `json:"category"`
		Organization      string `json:"organization"`
		PrimaryChannel    string `json:"primary_channel"`
		LastContactAfter  string `json:"last_contact_after"`
		LastContactBefore string `json:"last_contact_before"`
		Sort              string `json:"sort"`
	}{
		Query: normalized.query, ContactState: normalized.contactState,
		Category: normalized.category, Organization: normalized.organization,
		PrimaryChannel:   normalized.primaryChannel,
		LastContactAfter: normalized.lastContactAfter, LastContactBefore: normalized.lastContactBefore,
		Sort: normalized.sort,
	})
	if err != nil {
		return normalized, fmt.Errorf("encode directory filters: %w", err)
	}
	digest := sha256.Sum256(encoded)
	normalized.fingerprint = hex.EncodeToString(digest[:])
	return normalized, nil
}

func (s *Store) directoryPeoplePageContext(
	ctx context.Context,
	query normalizedDirectoryPeopleQuery,
) (*DirectoryPeoplePage, error) {
	var after *directoryPeopleCursor
	if query.cursor != "" {
		cursor, err := decodeDirectoryPeopleCursor(query.cursor)
		if err != nil {
			return nil, err
		}
		if cursor.Fingerprint != query.fingerprint {
			return nil, ErrInvalidDirectoryCursor
		}
		after = &cursor
	}

	var page *DirectoryPeoplePage
	err := s.withFreshDirectorySnapshotContext(ctx, func(tx *loggedTx) error {
		if after != nil {
			anchor, err := s.directoryCursorAnchorTx(ctx, tx, query, after.PersonID)
			if err != nil {
				return err
			}
			if after.Quality != anchor.quality ||
				after.AnchorHash != directoryAnchorHash(anchor.sortKey, anchor.orderName) {
				return ErrInvalidDirectoryCursor
			}
			after.OrderKey = anchor.orderName
			after.SortKey = anchor.sortKey
		}
		candidates, err := s.selectDirectoryPeopleTx(ctx, tx, query, after)
		if err != nil {
			return err
		}
		page = &DirectoryPeoplePage{People: make([]DirectoryPersonSummary, 0, min(query.limit, len(candidates)))}
		if len(candidates) > query.limit {
			last := candidates[query.limit-1]
			page.NextCursor, err = encodeDirectoryPeopleCursor(directoryPeopleCursor{
				Version: directoryCursorVersion, Fingerprint: query.fingerprint,
				Quality: last.quality, AnchorHash: directoryAnchorHash(last.sortKey, last.orderName), PersonID: last.summary.ID,
			})
			if err != nil {
				return err
			}
			candidates = candidates[:query.limit]
		}
		if err := s.hydrateDirectoryPeopleTx(ctx, tx, candidates); err != nil {
			return err
		}
		for _, candidate := range candidates {
			page.People = append(page.People, candidate.summary)
		}
		return nil
	})
	return page, err
}

// selectDirectoryPeopleTx scans indexed candidate chunks until it has the
// requested verified page plus its cursor row. Hydration stays separate so
// current categories and employments are never read outside that window.
func (s *Store) selectDirectoryPeopleTx(ctx context.Context, tx *loggedTx, query normalizedDirectoryPeopleQuery, after *directoryPeopleCursor) ([]directoryPersonCandidate, error) {
	querySQL, args := directoryCandidateProjectionSQL(query)
	var rawAfter *directoryRawCandidateCursor
	if after != nil {
		rawAfter = &directoryRawCandidateCursor{quality: after.Quality, sortKey: after.SortKey, orderName: after.OrderKey, personID: after.PersonID}
	}
	target, chunkSize := query.limit+1, directoryRawCandidateChunkSize(query.limit)
	verified := make([]directoryPersonCandidate, 0, target)
	for len(verified) < target {
		raw, err := s.selectDirectoryRawCandidateChunkTx(ctx, tx, querySQL, args, rawAfter, chunkSize, query.sort)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		rawCount := len(raw)
		if rawAfter != nil && !directoryRawCandidateFollows(raw[0], *rawAfter, query.sort) {
			return nil, errors.New("directory raw candidate cursor repeated")
		}
		lastRaw := raw[len(raw)-1]
		if len(query.terms) > 0 {
			raw, err = s.verifyDirectoryCandidateTokensTx(ctx, tx, raw, query.terms)
			if err != nil {
				return nil, err
			}
		}
		verified = append(verified, raw...)
		if len(verified) >= target || rawCount < chunkSize {
			break
		}
		rawAfter = &directoryRawCandidateCursor{quality: lastRaw.quality, sortKey: lastRaw.sortKey, orderName: lastRaw.orderName, personID: lastRaw.summary.ID}
	}
	if len(verified) > target {
		verified = verified[:target]
	}
	return verified, nil
}

func directoryCandidateProjectionSQL(query normalizedDirectoryPeopleQuery) (string, []any) {
	where, filterArgs := []string{}, []any{}
	if query.contactState != "" {
		where, filterArgs = append(where, "dp.contact_state = ?"), append(filterArgs, query.contactState)
	}
	if query.primaryChannel != "" {
		where, filterArgs = append(where, "dp.primary_channel = ?"), append(filterArgs, query.primaryChannel)
	}
	if query.lastContactAfter != "" {
		where, filterArgs = append(where, "dp.last_contact_key >= ?"), append(filterArgs, query.lastContactAfter)
	}
	if query.lastContactBefore != "" {
		where, filterArgs = append(where, "dp.last_contact_key <= ? AND dp.last_contact_key != ''"), append(filterArgs, query.lastContactBefore)
	}
	if query.category != "" {
		where, filterArgs = append(where, `EXISTS (SELECT 1 FROM directory_person_filters filter WHERE filter.person_id = dp.person_id AND filter.filter_kind = 'category' AND filter.value_key = ?)`), append(filterArgs, directoryKey(query.category))
	}
	if query.organization != "" {
		where, filterArgs = append(where, `EXISTS (SELECT 1 FROM directory_person_filters filter WHERE filter.person_id = dp.person_id AND filter.filter_kind = 'organization' AND filter.value_key = ?)`), append(filterArgs, directoryKey(query.organization))
	}
	filterSQL := "1 = 1"
	if len(where) > 0 {
		filterSQL = strings.Join(where, " AND ")
	}
	var args []any
	var querySQL string
	sortExpression := "dp.order_key"
	if query.sort != DirectoryPeopleSortName {
		sortExpression = "dp.last_contact_key"
	}
	if len(query.terms) == 0 {
		querySQL, args = `SELECT dp.person_id, 0 AS match_quality, `+sortExpression+` AS sort_key, dp.order_key FROM directory_people dp WHERE `+filterSQL, append(args, filterArgs...)
	} else {
		raw := make([]string, 0, len(query.terms)*4)
		for index, term := range query.terms {
			fragment, fragmentArgs := directoryTermProjectionSQL(term, index)
			raw, args = append(raw, fragment), append(args, fragmentArgs...)
		}
		args = append(args, filterArgs...)
		querySQL = `WITH raw_matches AS (` + strings.Join(raw, ` UNION ALL `) + `), term_matches AS (SELECT person_id, term_index, MIN(match_quality) AS match_quality FROM raw_matches GROUP BY person_id, term_index), ranked AS (SELECT dp.person_id, MAX(match.match_quality) AS match_quality, ` + sortExpression + ` AS sort_key, dp.order_key FROM term_matches match JOIN directory_people dp ON dp.person_id = match.person_id WHERE ` + filterSQL + ` GROUP BY dp.person_id, dp.order_key, dp.last_contact_key HAVING COUNT(*) = ` + strconv.Itoa(len(query.terms)) + `) SELECT person_id, match_quality, sort_key, order_key FROM ranked`
	}
	return querySQL, args
}

func (s *Store) directoryCursorAnchorTx(
	ctx context.Context,
	tx *loggedTx,
	query normalizedDirectoryPeopleQuery,
	personID int64,
) (directoryPersonCandidate, error) {
	querySQL, args := directoryCandidateProjectionSQL(query)
	querySQL = `SELECT person_id, match_quality, sort_key, order_key FROM (` + querySQL + `) candidate WHERE person_id = ?`
	args = append(args, personID)
	candidates, err := s.selectDirectoryRawCandidateChunkTx(ctx, tx, querySQL, args, nil, 1, query.sort)
	if err != nil {
		return directoryPersonCandidate{}, err
	}
	if len(query.terms) > 0 {
		candidates, err = s.verifyDirectoryCandidateTokensTx(ctx, tx, candidates, query.terms)
		if err != nil {
			return directoryPersonCandidate{}, err
		}
	}
	if len(candidates) != 1 {
		return directoryPersonCandidate{}, ErrInvalidDirectoryCursor
	}
	return candidates[0], nil
}

func directoryRawCandidateChunkSize(limit int) int {
	return min(max(limit+1, 16), maxDirectoryRawCandidateChunkSize)
}

func (s *Store) selectDirectoryRawCandidateChunkTx(ctx context.Context, tx *loggedTx, candidateSQL string, args []any, after *directoryRawCandidateCursor, limit int, order string) ([]directoryPersonCandidate, error) {
	queryArgs := append([]any(nil), args...)
	sortComparator, sortDirection := ">", "ASC"
	if order == DirectoryPeopleSortLastContactDesc {
		sortComparator, sortDirection = "<", "DESC"
	}
	if after != nil {
		candidateSQL = `SELECT person_id, match_quality, sort_key, order_key FROM (` + candidateSQL + `) candidate WHERE (match_quality > ? OR (match_quality = ? AND (sort_key ` + sortComparator + ` ? OR (sort_key = ? AND (order_key > ? OR (order_key = ? AND person_id > ?))))))`
		queryArgs = append(queryArgs, after.quality, after.quality, after.sortKey, after.sortKey, after.orderName, after.orderName, after.personID)
	}
	queryArgs = append(queryArgs, limit)
	rows, err := tx.QueryContext(ctx, candidateSQL+` ORDER BY match_quality, sort_key `+sortDirection+`, order_key, person_id LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("select directory people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]directoryPersonCandidate, 0, limit)
	for rows.Next() {
		var candidate directoryPersonCandidate
		if err := rows.Scan(&candidate.summary.ID, &candidate.quality, &candidate.sortKey, &candidate.orderName); err != nil {
			return nil, fmt.Errorf("scan directory person: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate directory people: %w", err)
	}
	return candidates, nil
}

func directoryRawCandidateFollows(candidate directoryPersonCandidate, after directoryRawCandidateCursor, order string) bool {
	if candidate.quality != after.quality {
		return candidate.quality > after.quality
	}
	if candidate.sortKey != after.sortKey {
		if order == DirectoryPeopleSortLastContactDesc {
			return candidate.sortKey < after.sortKey
		}
		return candidate.sortKey > after.sortKey
	}
	if candidate.orderName != after.orderName {
		return candidate.orderName > after.orderName
	}
	return candidate.summary.ID > after.personID
}

// verifyDirectoryCandidateTokensTx treats delete-key hits as an indexed
// prefilter only. The bounded selected IDs are verified against their actual
// canonical tokens so shared deletion keys cannot promote edit-distance-two
// values into the fuzzy tier.
func (s *Store) verifyDirectoryCandidateTokensTx(ctx context.Context, tx *loggedTx, candidates []directoryPersonCandidate, terms []string) ([]directoryPersonCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	ids, tokens := make([]any, 0, len(candidates)), make(map[int64][]string, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.summary.ID)
	}
	rows, err := tx.QueryContext(ctx, `SELECT person_id, token_key FROM directory_person_tokens WHERE person_id IN (`+directoryPlaceholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("load directory candidate tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("scan directory candidate token: %w", err)
		}
		value, err := hex.DecodeString(key)
		if err != nil {
			return nil, fmt.Errorf("decode directory candidate token: %w", err)
		}
		tokens[id] = append(tokens[id], string(value))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate directory candidate tokens: %w", err)
	}
	verified := make([]directoryPersonCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		quality, matches := directoryCandidateQuality(tokens[candidate.summary.ID], terms)
		if matches {
			candidate.quality = quality
			verified = append(verified, candidate)
		}
	}
	return verified, nil
}

func directoryCandidateQuality(tokens, terms []string) (int, bool) {
	quality := 0
	for _, term := range terms {
		best, found := 3, false
		for _, token := range tokens {
			switch {
			case token == term:
				best, found = 0, true
			case strings.HasPrefix(token, term) && best > 1:
				best, found = 1, true
			case utf8.RuneCountInString(term) >= 4 && directoryEditDistanceAtMostOne(token, term) && best > 2:
				best, found = 2, true
			}
		}
		if !found {
			return 0, false
		}
		if best > quality {
			quality = best
		}
	}
	return quality, true
}

func directoryEditDistanceAtMostOne(left, right string) bool {
	a, b := []rune(left), []rune(right)
	if len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	if len(a) == len(b) {
		first, second := -1, -1
		for index := range a {
			if a[index] == b[index] {
				continue
			}
			if first == -1 {
				first = index
			} else if second == -1 {
				second = index
			} else {
				return false
			}
		}
		if second == first+1 && a[first] == b[second] && a[second] == b[first] {
			return true
		}
	}
	previous, current := make([]int, len(b)+1), make([]int, len(b)+1)
	for index := range previous {
		previous[index] = index
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		minimum := current[0]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
			if current[j] < minimum {
				minimum = current[j]
			}
		}
		if minimum > 1 {
			return false
		}
		previous, current = current, previous
	}
	return previous[len(b)] <= 1
}

func directoryTermProjectionSQL(term string, index int) (string, []any) {
	key, end := directoryKey(term), directoryPrefixEnd(directoryKey(term))
	parts := []string{`SELECT person_id, 0 AS match_quality, ` + strconv.Itoa(index) + ` AS term_index FROM directory_person_tokens WHERE token_key = ?`, `SELECT person_id, 1 AS match_quality, ` + strconv.Itoa(index) + ` AS term_index FROM directory_person_tokens WHERE token_key >= ? AND token_key < ?`}
	args := []any{key, key, end}
	if utf8.RuneCountInString(term) >= 4 {
		deletes := append([]string{term}, directoryDeleteKeys(term)...)
		parts = append(parts, `SELECT person_id, 2 AS match_quality, `+strconv.Itoa(index)+` AS term_index FROM directory_person_token_deletes WHERE delete_key IN (`+directoryPlaceholders(len(deletes))+`)`)
		for _, value := range deletes {
			args = append(args, directoryKey(value))
		}
		fuzzy := directoryFuzzyTokenKeys(term)
		parts = append(parts, `SELECT person_id, 2 AS match_quality, `+strconv.Itoa(index)+` AS term_index FROM directory_person_tokens WHERE token_key IN (`+directoryPlaceholders(len(fuzzy))+`)`)
		for _, value := range fuzzy {
			args = append(args, directoryKey(value))
		}
	}
	return strings.Join(parts, ` UNION ALL `), args
}

func directoryPrefixEnd(value string) string {
	bytes := []byte(value)
	for index := len(bytes) - 1; index >= 0; index-- {
		if bytes[index] != 0xff {
			bytes[index]++
			return string(bytes[:index+1])
		}
	}
	return "\xff"
}
func directoryPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (s *Store) hydrateDirectoryPeopleTx(ctx context.Context, tx *loggedTx, candidates []directoryPersonCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]any, 0, len(candidates))
	byID := make(map[int64]int, len(candidates))
	for index := range candidates {
		ids = append(ids, candidates[index].summary.ID)
		byID[candidates[index].summary.ID] = index
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT person.id, person.display_name, person.revision,
		projection.primary_channel, projection.contact_state, projection.last_contact_key
		FROM persons person JOIN directory_people projection ON projection.person_id = person.id
		WHERE person.id IN (`+placeholders+`)`, ids...)
	if err != nil {
		return fmt.Errorf("hydrate directory people: %w", err)
	}
	for rows.Next() {
		var id int64
		var displayName sql.NullString
		var primaryChannel, contactState, lastContactKey string
		if err := rows.Scan(&id, &displayName, &candidates[byID[id]].summary.Revision, &primaryChannel, &contactState, &lastContactKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan hydrated directory person: %w", err)
		}
		candidate := &candidates[byID[id]]
		candidate.summary.PrimaryChannel = primaryChannel
		candidate.summary.ContactState = contactState
		if lastContactKey != "" {
			lastContactAt, err := time.Parse(time.RFC3339Nano, lastContactKey)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("parse hydrated directory last contact: %w", err)
			}
			candidate.summary.LastContactAt = &lastContactAt
		}
		candidate.summary.Categories = []string{}
		candidate.summary.Organizations = []string{}
		if displayName.Valid {
			value := displayName.String
			candidate.summary.DisplayName = &value
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate hydrated directory people: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close hydrated directory people: %w", err)
	}
	if err := hydrateDirectoryValuesTx(ctx, tx, `SELECT person_id, original_value
		FROM person_categories WHERE active_until IS NULL AND superseded_at IS NULL
		AND person_id IN (`+placeholders+`)`, ids, byID, candidates, true); err != nil {
		return err
	}
	return hydrateDirectoryValuesTx(ctx, tx, `SELECT employment.person_id, organization.name
		FROM employments employment JOIN organizations organization ON organization.id = employment.organization_id
		WHERE `+s.dialect.BoolTrueExpr("employment.is_current")+`
		  AND organization.merged_into_id IS NULL AND organization.retired_at IS NULL
		  AND employment.person_id IN (`+placeholders+`)`, ids, byID, candidates, false)
}

func hydrateDirectoryValuesTx(ctx context.Context, tx *loggedTx, query string, args []any, byID map[int64]int, candidates []directoryPersonCandidate, categories bool) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("hydrate directory values: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var personID int64
		var value string
		if err := rows.Scan(&personID, &value); err != nil {
			return fmt.Errorf("scan directory value: %w", err)
		}
		if categories {
			candidates[byID[personID]].summary.Categories = append(candidates[byID[personID]].summary.Categories, value)
		} else {
			candidates[byID[personID]].summary.Organizations = append(candidates[byID[personID]].summary.Organizations, value)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate directory values: %w", err)
	}
	for index := range candidates {
		candidates[index].summary.Categories = uniqueDirectoryStrings(candidates[index].summary.Categories)
		candidates[index].summary.Organizations = uniqueDirectoryStrings(candidates[index].summary.Organizations)
	}
	return nil
}

func encodeDirectoryPeopleCursor(cursor directoryPeopleCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode directory cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeDirectoryPeopleCursor(value string) (directoryPeopleCursor, error) {
	if len(value) > base64.RawURLEncoding.EncodedLen(maxDirectoryCursorBytes) {
		return directoryPeopleCursor{}, ErrInvalidDirectoryCursor
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maxDirectoryCursorBytes {
		return directoryPeopleCursor{}, ErrInvalidDirectoryCursor
	}
	var cursor directoryPeopleCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil ||
		cursor.Version != directoryCursorVersion ||
		!validLowerSHA256(cursor.Fingerprint) ||
		!validLowerSHA256(cursor.AnchorHash) ||
		cursor.Quality < 0 || cursor.Quality > 2 ||
		cursor.PersonID <= 0 {
		return directoryPeopleCursor{}, ErrInvalidDirectoryCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return directoryPeopleCursor{}, ErrInvalidDirectoryCursor
	}
	return cursor, nil
}

func directoryAnchorHash(sortKey, orderKey string) string {
	digest := sha256.Sum256([]byte(sortKey + "\x00" + orderKey))
	return hex.EncodeToString(digest[:])
}

func directoryLastContactKey(value time.Time) string {
	return value.UTC().Format(directoryLastContactKeyLayout)
}

func uniqueDirectoryStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := normalizeDirectoryText(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		leftKey, rightKey := normalizeDirectoryText(result[left]), normalizeDirectoryText(result[right])
		if leftKey == rightKey {
			return result[left] < result[right]
		}
		return leftKey < rightKey
	})
	return result
}
