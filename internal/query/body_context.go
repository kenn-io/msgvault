package query

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/sqldialect"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

const (
	// MessageBodyContextSnippetBytes is the maximum UTF-8 byte length of one
	// body-search context snippet.
	MessageBodyContextSnippetBytes = 300
	// MessageBodyContextMaxSnippets caps contexts returned for one message.
	MessageBodyContextMaxSnippets = 5

	// Context extraction deliberately has a request-wide budget. Search bodies
	// are imported data and may be attacker-controlled; multiplying a per-body
	// allowance by the HTTP result limit would otherwise make one request do
	// hundreds of megabytes of tokenization work.
	messageBodyContextRequestScanBytes = 1 * 1024 * 1024
	messageBodyContextChunkCoreBytes   = 4 * 1024
	messageBodyContextChunkGuardBytes  = 1 * 1024
	// PostgreSQL's indexed body input is byte-capped at 700 kB on the
	// incremental path. Keeping the canonical probe at or below that bound
	// avoids creating a tsvector larger than the one that produced the hit.
	messageBodyContextPostgresScanBytes = 700_000

	messageBodyContextMaxQueryTerms     = 32
	messageBodyContextMaxQueryBytes     = 32 * 1024
	messageBodyContextMaxQueryLexemes   = 256
	messageBodyContextMaxMarkers        = 2_048
	messageBodyContextMaxCandidates     = 2_048
	messageBodyContextMaxCandidateBytes = 8 * 1024 * 1024
)

type bodyContextSpan struct {
	start int
	end   int
}

type bodyContextMarkers struct {
	start    string
	end      string
	ellipsis string
}

type bodyContextSourceState struct {
	scanTruncated bool
	chunked       bool
}

type bodyContextChunk struct {
	id        int
	messageID int64
	body      string
	start     int
	end       int
	coreStart int
	coreEnd   int
}

type rawBodyContext struct {
	messageID int64
	group     int
	chunkID   int
	marked    string
}

type parsedBodyContext struct {
	messageID     int64
	group         int
	chunkID       int
	fragmentStart int
	plain         string
	spans         []bodyContextSpan
	truncated     bool
}

type postgresBodyContextCandidate struct {
	parsedIndex int
	query       string
	fragment    string
	spans       []bodyContextSpan
}

type bodyContextGroupKey struct {
	messageID int64
	group     int
}

type messageBodyContextAccumulator struct {
	snippets  []string
	seen      map[string]struct{}
	groups    map[int]struct{}
	truncated bool
}

func validateMessageBodyContextQuery(terms []string) error {
	if len(terms) == 0 {
		return fmt.Errorf("%w: requires at least one search term", ErrMessageBodySearchInvalidQuery)
	}
	if len(terms) > messageBodyContextMaxQueryTerms {
		return fmt.Errorf("%w: supports at most %d free-text terms",
			ErrMessageBodySearchInvalidQuery, messageBodyContextMaxQueryTerms)
	}
	totalBytes := 0
	for _, term := range terms {
		totalBytes += len(term)
		if len(sqldialect.EscapeTSQueryTerm(term)) > messageBodyContextMaxQueryLexemes {
			return fmt.Errorf("%w: one term expands beyond the %d-lexeme limit",
				ErrMessageBodySearchInvalidQuery, messageBodyContextMaxQueryLexemes)
		}
	}
	if totalBytes > messageBodyContextMaxQueryBytes {
		return fmt.Errorf("%w: free text exceeds the %d-byte limit",
			ErrMessageBodySearchInvalidQuery, messageBodyContextMaxQueryBytes)
	}
	return nil
}

func (e *SQLiteEngine) searchableBodyContextTerms(
	ctx context.Context,
	terms []string,
) ([]string, error) {
	switch e.dialect.messageBodyContextBackend() {
	case messageBodyContextPostgreSQL:
		searchable := make([]string, 0, len(terms))
		for _, term := range terms {
			if _, arg := e.dialect.BuildFTSBodyTerm([]string{term}); arg != "" {
				searchable = append(searchable, term)
			}
		}
		return searchable, nil
	case messageBodyContextSQLite:
		return sqliteSearchableBodyContextTerms(ctx, terms)
	default:
		return nil, errors.New("message body context is unavailable for this query dialect")
	}
}

// sqliteSearchableBodyContextTerms asks unicode61 itself which term groups
// contain tokens. A Go Unicode-category prefilter is not equivalent: SQLite's
// frozen table intentionally treats some code points assigned after Unicode
// 6.1 as tokens, so host-language classification could silently broaden an
// AND query by dropping a real group.
func sqliteSearchableBodyContextTerms(ctx context.Context, terms []string) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	scratch, err := sql.Open(sqliteutil.DriverName(), ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open SQLite body-context term probe: %w", err)
	}
	scratch.SetMaxOpenConns(1)
	defer func() { _ = scratch.Close() }()
	if _, err := scratch.ExecContext(ctx, `CREATE VIRTUAL TABLE term_probe USING fts5(
		term_id UNINDEXED,
		body,
		tokenize='unicode61 remove_diacritics 1'
	)`); err != nil {
		return nil, fmt.Errorf("create SQLite body-context term probe: %w", err)
	}

	values := make([]string, len(terms))
	args := make([]any, 0, 3*len(terms))
	for i, term := range terms {
		values[i] = "(?, ?, ?)"
		// SQLiteQueryDialect removes embedded '*' before quoting the MATCH
		// phrase, so probe the same literal text rather than tokenizing '*' as
		// a separator here.
		args = append(args, i+1, i, strings.ReplaceAll(term, "*", ""))
	}
	if _, err := scratch.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO term_probe(rowid, term_id, body) VALUES %s
	`, strings.Join(values, ", ")), args...); err != nil {
		return nil, fmt.Errorf("populate SQLite body-context term probe: %w", err)
	}

	dialect := SQLiteQueryDialect{}
	parts := make([]string, len(terms))
	args = args[:0]
	for i, term := range terms {
		_, queryArg := dialect.BuildFTSBodyTerm([]string{term})
		parts[i] = fmt.Sprintf(`
			SELECT %d AS term_index
			FROM term_probe
			WHERE rowid = ? AND term_probe MATCH ?
		`, i)
		args = append(args, i+1, queryArg)
	}
	rows, err := scratch.QueryContext(ctx, fmt.Sprintf(`
		WITH searchable AS (%s)
		SELECT term_index FROM searchable ORDER BY term_index
	`, strings.Join(parts, " UNION ALL ")), args...)
	if err != nil {
		return nil, fmt.Errorf("probe SQLite body-context terms: %w", err)
	}
	defer func() { _ = rows.Close() }()
	searchable := make([]string, 0, len(terms))
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, fmt.Errorf("scan SQLite body-context term probe: %w", err)
		}
		searchable = append(searchable, terms[index])
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite body-context term probe: %w", err)
	}
	return searchable, nil
}

// attachMessageBodySearchContexts extracts contexts in a bounded number of
// set-based operations. Each backend searches bounded, overlapping chunks
// with its native FTS implementation, so context matching cannot drift from
// SQLite unicode61 or PostgreSQL's simple text-search configuration.
func (e *SQLiteEngine) attachMessageBodySearchContexts(
	ctx context.Context,
	results []MessageSummary,
	terms []string,
) error {
	if len(results) == 0 {
		return nil
	}
	if err := validateMessageBodyContextQuery(terms); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	markers, err := newBodyContextMarkers()
	if err != nil {
		return err
	}
	ids := make([]int64, len(results))
	for i := range results {
		ids[i] = results[i].ID
	}

	searchableTerms, err := e.searchableBodyContextTerms(ctx, terms)
	if err != nil {
		return err
	}
	groupCount := min(len(searchableTerms), MessageBodyContextMaxSnippets)
	contextTerms := searchableTerms[:groupCount]
	perBodyScanBytes := max(1, messageBodyContextRequestScanBytes/len(results))
	if e.dialect.messageBodyContextBackend() == messageBodyContextPostgreSQL {
		perBodyScanBytes = min(perBodyScanBytes, messageBodyContextPostgresScanBytes)
	}
	states, chunks, bodies, err := e.loadBodyContextChunks(ctx, ids, perBodyScanBytes)
	if err != nil {
		return err
	}
	canonicalMatches, err := e.canonicalBodyContextMatches(ctx, ids, bodies, searchableTerms)
	if err != nil {
		return err
	}
	// Context output is capped at five groups, but stale-index validation is
	// not. A legacy index can only be trusted when every searchable AND group
	// matches a fully scanned canonical body; otherwise a sixth (non-rendered)
	// term could hide a false index hit.
	for _, result := range results {
		state := states[result.ID]
		if state.scanTruncated {
			continue
		}
		for group := range searchableTerms {
			key := bodyContextGroupKey{messageID: result.ID, group: group}
			if _, matchesCanonical := canonicalMatches[key]; !matchesCanonical {
				return fmt.Errorf(
					"%w: indexed body for message %d does not match its stored body",
					ErrMessageBodySearchIndexStale, result.ID,
				)
			}
		}
	}
	chunksByID := make(map[int]bodyContextChunk, len(chunks))
	for _, chunk := range chunks {
		chunksByID[chunk.id] = chunk
	}

	var raw []rawBodyContext
	switch e.dialect.messageBodyContextBackend() {
	case messageBodyContextSQLite:
		raw, err = e.sqliteBodyContexts(ctx, chunks, contextTerms, markers)
	case messageBodyContextPostgreSQL:
		raw, err = e.postgresBodyContexts(ctx, chunks, contextTerms, markers)
	default:
		return errors.New("message body context is unavailable for this query dialect")
	}
	if err != nil {
		return err
	}

	parsed := make([]parsedBodyContext, 0, len(raw))
	for _, item := range raw {
		plain, spans, truncated, parseErr := parseMarkedBodyContext(
			ctx, item.marked, markers,
		)
		if parseErr != nil {
			return fmt.Errorf("parse message %d body context: %w", item.messageID, parseErr)
		}
		chunk, ok := chunksByID[item.chunkID]
		if !ok {
			return fmt.Errorf("message %d body context references an unknown chunk", item.messageID)
		}
		fragmentStart := bodyContextFragmentStart(
			chunk, plain, spans, states[item.messageID],
		)
		if fragmentStart < 0 {
			return fmt.Errorf("message %d backend-native body context does not align with its source chunk", item.messageID)
		}
		parsed = append(parsed, parsedBodyContext{
			messageID:     item.messageID,
			group:         item.group,
			chunkID:       item.chunkID,
			fragmentStart: fragmentStart,
			plain:         plain,
			spans:         spans,
			truncated:     truncated || len(plain) < len(chunk.body),
		})
	}

	accepted := make(map[int][]bodyContextSpan, len(parsed))
	uncertainGroups := make(map[bodyContextGroupKey]struct{})
	for _, result := range results {
		state := states[result.ID]
		for group := range contextTerms {
			key := bodyContextGroupKey{messageID: result.ID, group: group}
			if state.scanTruncated {
				uncertainGroups[bodyContextGroupKey{messageID: result.ID, group: group}] = struct{}{}
			} else if _, matchesCanonical := canonicalMatches[key]; matchesCanonical {
				uncertainGroups[key] = struct{}{}
			}
		}
	}
	if e.dialect.messageBodyContextBackend() == messageBodyContextPostgreSQL {
		var postgresUncertain map[bodyContextGroupKey]struct{}
		accepted, postgresUncertain, err = e.validatePostgresBodyContextCandidates(
			ctx, parsed, contextTerms, chunksByID, states,
		)
		if err != nil {
			return err
		}
		for key := range postgresUncertain {
			uncertainGroups[key] = struct{}{}
		}
	} else {
		acceptedGroups := make(map[bodyContextGroupKey]struct{})
		for i := range parsed {
			chunk := chunksByID[parsed[i].chunkID]
			state := states[parsed[i].messageID]
			var safeSpan *bodyContextSpan
			for j := range parsed[i].spans {
				if bodyContextSpanIsSafe(
					chunk, parsed[i].spanInChunk(parsed[i].spans[j]), state,
				) {
					safeSpan = &parsed[i].spans[j]
					break
				}
			}
			key := bodyContextGroupKey{messageID: parsed[i].messageID, group: parsed[i].group}
			if safeSpan == nil {
				uncertainGroups[key] = struct{}{}
				continue
			}
			if _, exists := acceptedGroups[key]; exists {
				continue
			}
			// FTS5 snippet() marks complete phrase instances. One marked span is
			// sufficient to represent this query-term group and reserves the
			// remaining response slots for other groups.
			accepted[i] = []bodyContextSpan{*safeSpan}
			acceptedGroups[key] = struct{}{}
		}
	}

	accumulators := make(map[int64]*messageBodyContextAccumulator, len(results))
	for _, result := range results {
		state, ok := states[result.ID]
		if !ok {
			return fmt.Errorf("message %d body context source is unavailable", result.ID)
		}
		accumulators[result.ID] = &messageBodyContextAccumulator{
			seen:      make(map[string]struct{}),
			groups:    make(map[int]struct{}),
			truncated: state.scanTruncated || state.chunked || len(searchableTerms) > groupCount,
		}
	}
	for i, item := range parsed {
		spans := accepted[i]
		if len(spans) == 0 {
			continue
		}
		acc := accumulators[item.messageID]
		if _, alreadyRepresented := acc.groups[item.group]; alreadyRepresented {
			continue
		}
		acc.groups[item.group] = struct{}{}
		acc.truncated = acc.truncated || item.truncated
		snippets, snippetsTruncated := bodyContextSnippets(item.plain, spans)
		acc.truncated = acc.truncated || snippetsTruncated
		for _, snippet := range snippets {
			if _, duplicate := acc.seen[snippet]; duplicate {
				continue
			}
			if len(acc.snippets) >= MessageBodyContextMaxSnippets {
				acc.truncated = true
				continue
			}
			acc.seen[snippet] = struct{}{}
			acc.snippets = append(acc.snippets, snippet)
		}
	}

	for i := range results {
		acc := accumulators[results[i].ID]
		for group := range groupCount {
			if _, represented := acc.groups[group]; represented {
				continue
			}
			acc.truncated = true
			key := bodyContextGroupKey{messageID: results[i].ID, group: group}
			if _, uncertain := uncertainGroups[key]; !uncertain {
				return fmt.Errorf(
					"%w: indexed body for message %d does not match its stored body",
					ErrMessageBodySearchIndexStale, results[i].ID,
				)
			}
		}
		results[i].BodyContextSnippets = acc.snippets
		results[i].BodyContextSnippetsTruncated = acc.truncated
	}
	return nil
}

func newBodyContextMarkers() (bodyContextMarkers, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return bodyContextMarkers{}, fmt.Errorf("generate body context markers: %w", err)
	}
	base := "mv" + hex.EncodeToString(random[:])
	return bodyContextMarkers{
		start:    base + "s",
		end:      base + "e",
		ellipsis: base + "x",
	}, nil
}

func bodyContextIDPlaceholders(ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ", "), args
}

func (e *SQLiteEngine) loadBodyContextChunks(
	ctx context.Context,
	ids []int64,
	scanBytes int,
) (map[int64]bodyContextSourceState, []bodyContextChunk, map[int64]string, error) {
	idPlaceholders, idArgs := bodyContextIDPlaceholders(ids)
	var args []any
	var querySQL string
	switch e.dialect.messageBodyContextBackend() {
	case messageBodyContextSQLite:
		// SQLite materializes an entire TEXT cell before evaluating substr(),
		// even when only a tiny prefix is requested. octet_length() is served
		// from record metadata, so CASE can reject an oversized cell before it
		// is loaded. Eligible values sum to the request-wide scan budget (plus
		// one UTF-8 guard per result); oversized values become explicitly
		// truncated with no misleading fallback context.
		limit := scanBytes + utf8.UTFMax
		args = append([]any{limit, limit}, idArgs...)
		querySQL = fmt.Sprintf(`
			SELECT mb.message_id,
				CASE
					WHEN COALESCE(octet_length(mb.body_text), 0) <= ?
					THEN CAST(COALESCE(mb.body_text, '') AS BLOB)
					ELSE X''
				END,
				COALESCE(octet_length(mb.body_text), 0) > ?
			FROM message_bodies mb
			WHERE mb.message_id IN (%s)
			ORDER BY mb.message_id
		`, idPlaceholders)
	case messageBodyContextPostgreSQL:
		args = append([]any{scanBytes + utf8.UTFMax}, idArgs...)
		querySQL = fmt.Sprintf(`
			SELECT mb.message_id,
				COALESCE(convert_to(
					SUBSTRING(COALESCE(mb.body_text, '') FROM 1 FOR ?), 'UTF8'
				), ''::bytea),
				FALSE
			FROM message_bodies mb
			WHERE mb.message_id IN (%s)
			ORDER BY mb.message_id
		`, idPlaceholders)
	default:
		return nil, nil, nil, errors.New("message body context is unavailable for this query dialect")
	}
	rows, err := e.queryContext(ctx, querySQL, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load bounded message body prefixes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	states := make(map[int64]bodyContextSourceState, len(ids))
	bodies := make(map[int64]string, len(ids))
	chunks := make([]bodyContextChunk, 0)
	for rows.Next() {
		var messageID int64
		var raw []byte
		var sourceTruncated bool
		if err := rows.Scan(&messageID, &raw, &sourceTruncated); err != nil {
			return nil, nil, nil, fmt.Errorf("scan bounded message body prefix: %w", err)
		}
		prefix, prefixTruncated, err := validBodyContextPrefix(raw, scanBytes)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("message %d body context: %w", messageID, err)
		}
		bodies[messageID] = prefix
		messageChunks := makeBodyContextChunks(messageID, prefix, len(chunks)+1)
		states[messageID] = bodyContextSourceState{
			scanTruncated: sourceTruncated || prefixTruncated,
			chunked:       len(messageChunks) > 1,
		}
		chunks = append(chunks, messageChunks...)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("iterate bounded message body prefixes: %w", err)
	}
	return states, chunks, bodies, nil
}

func validBodyContextPrefix(raw []byte, scanBytes int) (string, bool, error) {
	cut := min(len(raw), scanBytes)
	for cut > 0 && cut < len(raw) && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	prefix := raw[:cut]
	if !utf8.Valid(prefix) {
		return "", false, errors.New("stored body prefix is not valid UTF-8")
	}
	return string(prefix), len(raw) > cut, nil
}

func (e *SQLiteEngine) canonicalBodyContextMatches(
	ctx context.Context,
	ids []int64,
	bodies map[int64]string,
	terms []string,
) (map[bodyContextGroupKey]struct{}, error) {
	matches := make(map[bodyContextGroupKey]struct{})
	if len(ids) == 0 || len(terms) == 0 {
		return matches, nil
	}

	if e.dialect.messageBodyContextBackend() == messageBodyContextSQLite {
		scratch, err := sql.Open(sqliteutil.DriverName(), ":memory:")
		if err != nil {
			return nil, fmt.Errorf("open SQLite canonical body-context probe: %w", err)
		}
		scratch.SetMaxOpenConns(1)
		defer func() { _ = scratch.Close() }()
		if _, err := scratch.ExecContext(ctx, `CREATE VIRTUAL TABLE canonical_bodies USING fts5(
			message_id UNINDEXED,
			body,
			tokenize='unicode61 remove_diacritics 1'
		)`); err != nil {
			return nil, fmt.Errorf("create SQLite canonical body-context probe: %w", err)
		}
		values := make([]string, len(ids))
		args := make([]any, 0, 3*len(ids))
		for i, messageID := range ids {
			values[i] = "(?, ?, ?)"
			args = append(args, i+1, messageID, bodies[messageID])
		}
		if _, err := scratch.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO canonical_bodies(rowid, message_id, body) VALUES %s
		`, strings.Join(values, ", ")), args...); err != nil {
			return nil, fmt.Errorf("populate SQLite canonical body-context probe: %w", err)
		}
		parts := make([]string, len(terms))
		args = args[:0]
		for group, term := range terms {
			_, queryArg := e.dialect.BuildFTSBodyTerm([]string{term})
			parts[group] = fmt.Sprintf(`
				SELECT message_id, %d AS group_id
				FROM canonical_bodies
				WHERE canonical_bodies MATCH ?
			`, group)
			args = append(args, queryArg)
		}
		rows, err := scratch.QueryContext(ctx, strings.Join(parts, " UNION ALL "), args...)
		if err != nil {
			return nil, fmt.Errorf("query SQLite canonical body-context probe: %w", err)
		}
		defer func() { _ = rows.Close() }()
		return scanCanonicalBodyContextMatches(rows)
	}

	if e.dialect.messageBodyContextBackend() != messageBodyContextPostgreSQL {
		return nil, errors.New("message body context is unavailable for this query dialect")
	}
	sourceValues := make([]string, len(ids))
	args := make([]any, 0, 2*len(ids)+len(terms))
	for i, messageID := range ids {
		sourceValues[i] = "(?::bigint, ?::text)"
		args = append(args, messageID, bodies[messageID])
	}
	queryValues := make([]string, len(terms))
	for group, term := range terms {
		_, weightedArg := e.dialect.BuildFTSBodyTerm([]string{term})
		queryValues[group] = fmt.Sprintf("(%d, to_tsquery('simple', ?))", group)
		args = append(args, postgresContextTSQuery(weightedArg))
	}
	rows, err := e.queryContext(ctx, fmt.Sprintf(`
		WITH
		canonical_bodies(message_id, body) AS (VALUES %s),
		query_groups(group_id, query) AS (VALUES %s)
		SELECT b.message_id, q.group_id
		FROM canonical_bodies b
		CROSS JOIN query_groups q
		WHERE to_tsvector('simple', b.body) @@ q.query
	`, strings.Join(sourceValues, ", "), strings.Join(queryValues, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("query PostgreSQL canonical body-context probe: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanCanonicalBodyContextMatches(rows)
}

func scanCanonicalBodyContextMatches(rows *sql.Rows) (map[bodyContextGroupKey]struct{}, error) {
	matches := make(map[bodyContextGroupKey]struct{})
	for rows.Next() {
		var key bodyContextGroupKey
		if err := rows.Scan(&key.messageID, &key.group); err != nil {
			return nil, fmt.Errorf("scan canonical body-context match: %w", err)
		}
		matches[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate canonical body-context matches: %w", err)
	}
	return matches, nil
}

func makeBodyContextChunks(messageID int64, body string, firstID int) []bodyContextChunk {
	chunks := make([]bodyContextChunk, 0, 1+len(body)/messageBodyContextChunkCoreBytes)
	coreStart := 0
	for coreStart < len(body) {
		coreEnd := min(len(body), coreStart+messageBodyContextChunkCoreBytes)
		for coreEnd > coreStart && coreEnd < len(body) && !utf8.RuneStart(body[coreEnd]) {
			coreEnd--
		}
		if coreEnd == coreStart {
			_, size := utf8.DecodeRuneInString(body[coreStart:])
			coreEnd = coreStart + size
		}

		chunkStart := max(0, coreStart-messageBodyContextChunkGuardBytes)
		for chunkStart > 0 && !utf8.RuneStart(body[chunkStart]) {
			chunkStart--
		}
		chunkEnd := min(len(body), coreEnd+messageBodyContextChunkGuardBytes)
		for chunkEnd < len(body) && !utf8.RuneStart(body[chunkEnd]) {
			chunkEnd++
		}
		chunks = append(chunks, bodyContextChunk{
			id:        firstID + len(chunks),
			messageID: messageID,
			body:      body[chunkStart:chunkEnd],
			start:     chunkStart,
			end:       chunkEnd,
			coreStart: coreStart,
			coreEnd:   coreEnd,
		})
		coreStart = coreEnd
	}
	return chunks
}

// bodyContextSpanIsSafe rejects matches manufactured by cutting through a
// token at a chunk or request-prefix boundary. Each chunk owns match starts in
// its non-overlapping core; guards provide enough original text on both sides
// for ordinary snippets and phrases. A match touching an artificial edge is
// omitted and advertised through the truncation flag instead of being shown as
// if it came from the indexed body.
func bodyContextSpanIsSafe(
	chunk bodyContextChunk,
	span bodyContextSpan,
	state bodyContextSourceState,
) bool {
	if span.start < 0 || span.end <= span.start || span.end > len(chunk.body) {
		return false
	}
	absoluteStart := chunk.start + span.start
	if absoluteStart < chunk.coreStart || absoluteStart >= chunk.coreEnd {
		return false
	}
	if span.start == 0 && chunk.start > 0 {
		return false
	}
	if span.end == len(chunk.body) && (chunk.end > chunk.coreEnd || state.scanTruncated) {
		return false
	}
	return true
}

func (item parsedBodyContext) spanInChunk(span bodyContextSpan) bodyContextSpan {
	return bodyContextSpan{
		start: item.fragmentStart + span.start,
		end:   item.fragmentStart + span.end,
	}
}

// bodyContextFragmentStart maps a backend-produced bounded fragment back to
// its source chunk. Prefer an occurrence whose marked span is owned by the
// chunk core. Identical repeated fragments have identical tokenizer behavior,
// so any safe occurrence is equivalent for context purposes.
func bodyContextFragmentStart(
	chunk bodyContextChunk,
	plain string,
	spans []bodyContextSpan,
	state bodyContextSourceState,
) int {
	if plain == "" || len(spans) == 0 || len(plain) > len(chunk.body) {
		return -1
	}
	first := -1
	searchFrom := 0
	outer := bodyContextSpan{start: spans[0].start, end: spans[len(spans)-1].end}
	for searchFrom <= len(chunk.body)-len(plain) {
		relative := strings.Index(chunk.body[searchFrom:], plain)
		if relative < 0 {
			break
		}
		offset := searchFrom + relative
		if first < 0 {
			first = offset
		}
		if bodyContextSpanIsSafe(chunk, bodyContextSpan{
			start: offset + outer.start,
			end:   offset + outer.end,
		}, state) {
			return offset
		}
		searchFrom = offset + 1
	}
	return first
}

func (e *SQLiteEngine) sqliteBodyContexts(
	ctx context.Context,
	chunks []bodyContextChunk,
	terms []string,
	markers bodyContextMarkers,
) ([]rawBodyContext, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	scratch, err := sql.Open(sqliteutil.DriverName(), ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open SQLite body-context scratch index: %w", err)
	}
	scratch.SetMaxOpenConns(1)
	defer func() { _ = scratch.Close() }()
	if _, err := scratch.ExecContext(ctx, `CREATE VIRTUAL TABLE body_chunks USING fts5(
		message_id UNINDEXED,
		chunk_id UNINDEXED,
		body,
		tokenize='unicode61 remove_diacritics 1'
	)`); err != nil {
		return nil, fmt.Errorf("create SQLite body-context scratch index: %w", err)
	}

	values := make([]string, len(chunks))
	args := make([]any, 0, 4*len(chunks))
	for i, chunk := range chunks {
		values[i] = "(?, ?, ?, ?)"
		args = append(args, chunk.id, chunk.messageID, chunk.id, chunk.body)
	}
	if _, err := scratch.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO body_chunks(rowid, message_id, chunk_id, body) VALUES %s
	`, strings.Join(values, ", ")), args...); err != nil {
		return nil, fmt.Errorf("populate SQLite body-context scratch index: %w", err)
	}

	rawParts := make([]string, 0, len(terms))
	args = args[:0]
	for group, term := range terms {
		_, arg := e.dialect.BuildFTSBodyTerm([]string{term})
		if arg == "" {
			continue
		}
		rawParts = append(rawParts, fmt.Sprintf(`
			SELECT message_id, %d AS group_id, chunk_id,
				snippet(body_chunks, 2, ?, ?, ?, 64) AS marked
			FROM body_chunks
			WHERE body_chunks MATCH ?
		`, group))
		args = append(args, markers.start, markers.end, markers.ellipsis, arg)
	}
	if len(rawParts) == 0 {
		return nil, errors.New("message body context query has no searchable terms")
	}
	rows, err := scratch.QueryContext(ctx, fmt.Sprintf(`
		WITH raw_matches AS (%s)
		SELECT message_id, group_id, chunk_id, marked
		FROM raw_matches
		ORDER BY message_id, group_id, chunk_id
	`, strings.Join(rawParts, " UNION ALL ")), args...)
	if err != nil {
		return nil, fmt.Errorf("query bounded SQLite body contexts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRawBodyContexts(rows)
}

func (e *SQLiteEngine) postgresBodyContexts(
	ctx context.Context,
	chunks []bodyContextChunk,
	terms []string,
	markers bodyContextMarkers,
) ([]rawBodyContext, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	chunkValues := make([]string, len(chunks))
	args := make([]any, 0, 3*len(chunks)+len(terms)+1)
	for i, chunk := range chunks {
		chunkValues[i] = "(?::integer, ?::bigint, ?::text)"
		args = append(args, chunk.id, chunk.messageID, chunk.body)
	}
	queryValues := make([]string, 0, len(terms))
	for group, term := range terms {
		_, weightedArg := e.dialect.BuildFTSBodyTerm([]string{term})
		arg := postgresContextTSQuery(weightedArg)
		if arg == "" {
			continue
		}
		queryValues = append(queryValues, fmt.Sprintf("(%d, to_tsquery('simple', ?))", group))
		args = append(args, arg)
	}
	if len(queryValues) == 0 {
		return nil, errors.New("message body context query has no searchable terms")
	}
	options := fmt.Sprintf(
		"StartSel=%s, StopSel=%s, MaxWords=64, MinWords=1, MaxFragments=1, FragmentDelimiter=%s",
		markers.start, markers.end, markers.ellipsis,
	)
	args = append(args, options)
	rows, err := e.queryContext(ctx, fmt.Sprintf(`
		WITH
		chunks(chunk_id, message_id, body) AS (VALUES %s),
		query_groups(group_id, query) AS (VALUES %s)
		SELECT c.message_id, q.group_id, c.chunk_id,
			ts_headline('simple', c.body, q.query, ?) AS marked
		FROM chunks c
		CROSS JOIN query_groups q
		WHERE to_tsvector('simple', c.body) @@ q.query
		ORDER BY c.message_id, q.group_id, c.chunk_id
	`, strings.Join(chunkValues, ", "), strings.Join(queryValues, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("query bounded PostgreSQL body contexts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanRawBodyContexts(rows)
}

func scanRawBodyContexts(rows *sql.Rows) ([]rawBodyContext, error) {
	var raw []rawBodyContext
	for rows.Next() {
		var item rawBodyContext
		if err := rows.Scan(&item.messageID, &item.group, &item.chunkID, &item.marked); err != nil {
			return nil, fmt.Errorf("scan body context: %w", err)
		}
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate body contexts: %w", err)
	}
	return raw, nil
}

func (e *SQLiteEngine) validatePostgresBodyContextCandidates(
	ctx context.Context,
	parsed []parsedBodyContext,
	terms []string,
	chunks map[int]bodyContextChunk,
	states map[int64]bodyContextSourceState,
) (map[int][]bodyContextSpan, map[bodyContextGroupKey]struct{}, error) {
	accepted := make(map[int][]bodyContextSpan, len(parsed))
	uncertain := make(map[bodyContextGroupKey]struct{})
	candidates := make([]postgresBodyContextCandidate, 0)
	totalCandidateBytes := 0

	for parsedIndex, item := range parsed {
		chunk := chunks[item.chunkID]
		state := states[item.messageID]
		key := bodyContextGroupKey{messageID: item.messageID, group: item.group}
		if item.truncated {
			uncertain[key] = struct{}{}
		}
		lexemeCount := len(sqldialect.EscapeTSQueryTerm(terms[item.group]))
		if lexemeCount <= 1 {
			for _, span := range item.spans {
				if bodyContextSpanIsSafe(chunk, item.spanInChunk(span), state) {
					accepted[parsedIndex] = []bodyContextSpan{span}
					break
				}
			}
			if len(accepted[parsedIndex]) == 0 {
				uncertain[key] = struct{}{}
			}
			continue
		}
		_, weightedArg := e.dialect.BuildFTSBodyTerm([]string{terms[item.group]})
		queryArg := postgresContextTSQuery(weightedArg)
		foundBudget := false
		for start := range item.spans {
			for width := 1; width <= lexemeCount && start+width <= len(item.spans); width++ {
				if len(candidates) >= messageBodyContextMaxCandidates {
					uncertain[key] = struct{}{}
					foundBudget = true
					break
				}
				end := start + width
				fragmentStart := item.spans[start].start
				fragmentEnd := item.spans[end-1].end
				if !bodyContextSpanIsSafe(chunk, item.spanInChunk(bodyContextSpan{
					start: fragmentStart,
					end:   fragmentEnd,
				}), state) {
					uncertain[key] = struct{}{}
					continue
				}
				fragment := item.plain[fragmentStart:fragmentEnd]
				if totalCandidateBytes+len(fragment) > messageBodyContextMaxCandidateBytes {
					uncertain[key] = struct{}{}
					foundBudget = true
					break
				}
				totalCandidateBytes += len(fragment)
				componentSpans := append([]bodyContextSpan(nil), item.spans[start:end]...)
				candidates = append(candidates, postgresBodyContextCandidate{
					parsedIndex: parsedIndex,
					query:       queryArg,
					fragment:    fragment,
					spans:       componentSpans,
				})
			}
			if foundBudget {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return accepted, uncertain, nil
	}

	values := make([]string, len(candidates))
	args := make([]any, 0, 3*len(candidates))
	for i, candidate := range candidates {
		values[i] = "(?::integer, ?::text, ?::text)"
		args = append(args, i, candidate.fragment, candidate.query)
	}
	rows, err := e.queryContext(ctx, fmt.Sprintf(`
		WITH candidates(candidate_id, fragment, query_text) AS (VALUES %s)
		SELECT candidate_id
		FROM candidates
		WHERE to_tsvector('simple', fragment) @@ to_tsquery('simple', query_text)
		ORDER BY candidate_id
	`, strings.Join(values, ", ")), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("validate PostgreSQL body-context candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var candidateID int
		if err := rows.Scan(&candidateID); err != nil {
			return nil, nil, fmt.Errorf("scan PostgreSQL body-context validation: %w", err)
		}
		candidate := candidates[candidateID]
		if _, alreadyAccepted := accepted[candidate.parsedIndex]; alreadyAccepted {
			continue
		}
		accepted[candidate.parsedIndex] = candidate.spans
		if len(candidate.fragment) > MessageBodyContextSnippetBytes {
			parsed[candidate.parsedIndex].truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate PostgreSQL body-context validations: %w", err)
	}
	for parsedIndex, item := range parsed {
		if len(accepted[parsedIndex]) == 0 {
			uncertain[bodyContextGroupKey{messageID: item.messageID, group: item.group}] = struct{}{}
		}
	}
	return accepted, uncertain, nil
}

func postgresContextTSQuery(weightedArg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(weightedArg, ":*D", ":*"), ":D", "")
}

func parseMarkedBodyContext(
	ctx context.Context,
	marked string,
	markers bodyContextMarkers,
) (string, []bodyContextSpan, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, false, err
	}
	truncated := strings.Contains(marked, markers.ellipsis)
	marked = strings.ReplaceAll(marked, markers.ellipsis, "")
	var plain strings.Builder
	plain.Grow(len(marked))
	spans := make([]bodyContextSpan, 0)
	cursor := 0
	for cursor < len(marked) {
		if len(spans)%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", nil, false, err
			}
		}
		relStart := strings.Index(marked[cursor:], markers.start)
		if relStart < 0 {
			plain.WriteString(marked[cursor:])
			break
		}
		markerStart := cursor + relStart
		plain.WriteString(marked[cursor:markerStart])
		matchStart := plain.Len()
		matchTextStart := markerStart + len(markers.start)
		relEnd := strings.Index(marked[matchTextStart:], markers.end)
		if relEnd < 0 {
			return "", nil, false, errors.New("body context has an unterminated match marker")
		}
		markerEnd := matchTextStart + relEnd
		plain.WriteString(marked[matchTextStart:markerEnd])
		spans = append(spans, bodyContextSpan{start: matchStart, end: plain.Len()})
		cursor = markerEnd + len(markers.end)
		if len(spans) >= messageBodyContextMaxMarkers {
			remainder := strings.ReplaceAll(marked[cursor:], markers.start, "")
			remainder = strings.ReplaceAll(remainder, markers.end, "")
			plain.WriteString(remainder)
			truncated = true
			break
		}
	}
	if len(spans) == 0 {
		return "", nil, false, errors.New("backend-native body context contains no match markers")
	}
	return plain.String(), spans, truncated, nil
}

// markedFragmentContext is retained as a focused parser seam for unit tests.
func markedFragmentContext(
	ctx context.Context,
	marked string,
	startMarker string,
	endMarker string,
	ellipsisMarker string,
) ([]string, bool, error) {
	plain, spans, truncated, err := parseMarkedBodyContext(ctx, marked, bodyContextMarkers{
		start: startMarker, end: endMarker, ellipsis: ellipsisMarker,
	})
	if err != nil {
		return nil, false, err
	}
	snippets, capped := bodyContextSnippets(plain, spans)
	return snippets, truncated || capped, nil
}

func bodyContextSnippets(body string, spans []bodyContextSpan) ([]string, bool) {
	if len(spans) == 0 {
		return nil, false
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end < spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	merged := make([]bodyContextSpan, 0, MessageBodyContextMaxSnippets)
	truncated := false
	for _, span := range spans {
		matchLen := span.end - span.start
		if matchLen > MessageBodyContextSnippetBytes {
			matchLen = MessageBodyContextSnippetBytes
			truncated = true
		}
		start, end := bodyContextWindow(
			len(body), span.start, matchLen, MessageBodyContextSnippetBytes,
		)
		candidate := bodyContextSpan{start: start, end: end}
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			unionStart := min(last.start, candidate.start)
			unionEnd := max(last.end, candidate.end)
			if candidate.start <= last.end && unionEnd-unionStart <= MessageBodyContextSnippetBytes {
				last.start = unionStart
				last.end = unionEnd
				continue
			}
		}
		if len(merged) >= MessageBodyContextMaxSnippets {
			truncated = true
			continue
		}
		merged = append(merged, candidate)
	}
	snippets := make([]string, 0, len(merged))
	for _, span := range merged {
		snippet, adjusted := bodyContextByteSlice(body, span.start, span.end)
		truncated = truncated || adjusted
		snippets = append(snippets, snippet)
	}
	return snippets, truncated
}

func bodyContextWindow(bodyLen, pos, matchLen, contextBytes int) (start, end int) {
	start = pos - (contextBytes-matchLen)/2
	end = start + contextBytes
	if start < 0 {
		start = 0
		end = min(bodyLen, contextBytes)
	} else if end > bodyLen {
		end = bodyLen
		start = max(0, end-contextBytes)
	}
	return start, end
}

func bodyContextByteSlice(body string, start, end int) (string, bool) {
	start = max(0, min(start, len(body)))
	end = max(start, min(end, len(body)))
	originalStart, originalEnd := start, end
	for start < end && !utf8.RuneStart(body[start]) {
		start++
	}
	for end > start && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	return body[start:end], start != originalStart || end != originalEnd
}
