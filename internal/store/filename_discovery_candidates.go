package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/filenamediscovery"
)

const filenamePostingBudget = 2048
const filenameCandidateBudget = 500
const filenamePostingPage = 64

// Scope filters reject postings before admission, so scoped requests read
// deeper into each stream to fill the same candidate pool.
const filenameScopedPostingBudget = 8192
const filenameScopedPostingPage = 256

// Hex token keys use only [0-9a-f]; "g" sorts after every key and starts a
// descending tuple seek.
const filenameKeyCeiling = "g"

var ErrFilenameIndexUnavailable = errors.New("filename discovery index is not ready")

// FilenameCandidateSet is private retrieval evidence. IDs still require
// authority, scope and current-name verification before becoming suggestions.
type FilenameCandidateSet struct {
	IDs                   []int64
	PostingRows           int
	CandidateLimitReached bool
	IndexRevision         string
}

// filenamePostingStream seeks table alias p in index order: descending token
// key when needed, then descending attachment ID. where binds args, then the
// cursor bounds. Ranking happens after the bounded retrieval.
type filenamePostingStream struct {
	table string
	key   string
	where string
	args  []any
	tuple bool
}

type filenamePosting struct {
	key     string
	id      int64
	inScope bool
	name    string
}

// FilenameCandidatesContext honors an existing authority snapshot, or owns a
// read snapshot for this call. It never initializes or repairs maintenance.
// Only live, in-scope postings whose projected name matches the whole query
// compete for the candidate budget. Every physical posting row, admitted or
// not, is charged to the work budget.
func (s *Store) FilenameCandidatesContext(ctx context.Context, query filenamediscovery.Query, scope FilenameSuggestionScope) (FilenameCandidateSet, error) {
	if err := ctx.Err(); err != nil {
		return FilenameCandidateSet{}, err
	}
	validated, err := filenamediscovery.ParseQuery(query.Normalized)
	if err != nil || !slices.Equal(validated.Tokens, query.Tokens) || validated.Normalized != query.Normalized {
		return FilenameCandidateSet{}, filenamediscovery.ErrInvalidQuery
	}
	if err := validateFilenameSuggestionScope(nil, scope); err != nil {
		return FilenameCandidateSet{}, err
	}
	scope = normalizeFilenameSuggestionScope(scope)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return FilenameCandidateSet{}, err
	}
	defer release()
	db := ReadDBContext(ctx, s.db.DB)
	var generation, revision int64
	err = db.QueryRowContext(ctx, s.Rebind(`SELECT generation, revision FROM filename_discovery_state AS st
		WHERE singleton=1 AND generation>0 AND phase='ready' AND EXISTS (
			SELECT 1 FROM attachment_change_consumers AS c WHERE consumer_key=?
			AND reconciliation_complete=TRUE AND baseline_sequence=st.baseline AND last_sequence=st.last_sequence
		)`), filenameDiscoveryConsumer).Scan(&generation, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return FilenameCandidateSet{}, ErrFilenameIndexUnavailable
	}
	if err != nil {
		return FilenameCandidateSet{}, fmt.Errorf("read filename index readiness: %w", err)
	}
	set := FilenameCandidateSet{IDs: make([]int64, 0), IndexRevision: fmt.Sprintf("g%d:r%d", generation, revision)}
	if scope.Person != nil && len(scope.Person.ParticipantIDs) == 0 {
		return set, nil
	}
	budget, pageSize := filenamePostingBudget, filenamePostingPage
	if filenameScopeFilters(scope) {
		budget, pageSize = filenameScopedPostingBudget, filenameScopedPostingPage
	}
	conditions, scopeArgs := s.filenameScopeConditions(scope)
	inScope := s.filenameAttachmentScopeExists("p.attachment_id", conditions)
	type candidate struct {
		id   int64
		rank filenamediscovery.Rank
	}
	var matches []candidate
	seen := make(map[int64]struct{}, filenameCandidateBudget)
streams:
	for _, stream := range filenameCandidateStreams(query, generation) {
		lastKey, lastID := filenameKeyCeiling, int64(math.MaxInt64)
		for {
			limit := min(pageSize, budget-set.PostingRows)
			if limit == 0 {
				set.CandidateLimitReached = true
				break streams
			}
			page, err := s.filenamePostingPage(ctx, db, stream, inScope, scopeArgs, lastKey, lastID, limit)
			if err != nil {
				return FilenameCandidateSet{}, err
			}
			set.PostingRows += len(page) // Charge the entire physical page before admission.
			for _, row := range page {
				lastKey, lastID = row.key, row.id
				if _, exists := seen[row.id]; exists || !row.inScope {
					continue
				}
				seen[row.id] = struct{}{}
				name, ok := filenamediscovery.ProjectName(row.name)
				if !ok {
					continue
				}
				if rank, matched := filenamediscovery.Match(query, name); matched {
					matches = append(matches, candidate{id: row.id, rank: rank})
				}
			}
			if len(page) < limit {
				break
			}
		}
	}
	// The posting budget also bounds this list. Select across all observed
	// token groups before the 500-ID authority limit, so partial matches and
	// lexical token order cannot crowd out better matches already read.
	slices.SortFunc(matches, func(a, b candidate) int {
		if order := cmp.Compare(a.rank, b.rank); order != 0 {
			return order
		}
		return cmp.Compare(b.id, a.id)
	})
	if len(matches) > filenameCandidateBudget {
		set.CandidateLimitReached = true
		matches = matches[:filenameCandidateBudget]
	}
	for _, match := range matches {
		set.IDs = append(set.IDs, match.id)
	}
	return set, nil
}

func (s *Store) filenamePostingPage(ctx context.Context, db SQLReader, stream filenamePostingStream,
	inScope string, scopeArgs []any, lastKey string, lastID int64, limit int,
) ([]filenamePosting, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A scalar primary-key probe preserves the posting seek's bounded plan.
	name := `(SELECT e.normalized_name FROM filename_discovery_entries e
		WHERE e.generation=p.generation AND e.attachment_id=p.attachment_id)`
	if stream.table == "filename_discovery_entries" {
		name = "p.normalized_name"
	}
	query := `SELECT ` + stream.key + `, p.attachment_id, ` + inScope + `, ` + name + ` FROM ` + stream.table + ` p
		WHERE p.generation=? AND ` + stream.where
	args := slices.Concat(scopeArgs, stream.args)
	if stream.tuple {
		query += ` AND (p.token_key,p.attachment_id)<(?,?) ORDER BY p.token_key DESC, p.attachment_id DESC LIMIT ?`
		args = append(args, lastKey, lastID, limit)
	} else {
		query += ` AND p.attachment_id<? ORDER BY p.attachment_id DESC LIMIT ?`
		args = append(args, lastID, limit)
	}
	rows, err := db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("seek filename postings: %w", err)
	}
	// Explicit close below reports errors; defer covers early failures.
	defer func() { _ = rows.Close() }()
	page := make([]filenamePosting, 0, limit)
	for rows.Next() {
		var row filenamePosting
		if err := rows.Scan(&row.key, &row.id, &row.inScope, &row.name); err != nil {
			return nil, fmt.Errorf("scan filename posting: %w", err)
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate filename postings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close filename postings: %w", err)
	}
	return page, nil
}

func filenameCandidateStreams(query filenamediscovery.Query, generation int64) []filenamePostingStream {
	seed := query.Tokens[0]
	for _, token := range query.Tokens[1:] {
		if utf8.RuneCountInString(token) > utf8.RuneCountInString(seed) {
			seed = token
		}
	}
	key := hex.EncodeToString([]byte(seed))
	hash := sha256.Sum256([]byte(query.Normalized))
	streams := []filenamePostingStream{{
		table: "filename_discovery_entries", key: "''",
		where: "p.name_hash=?", args: []any{generation, hex.EncodeToString(hash[:])},
	}}
	exact := func(key string) filenamePostingStream {
		return filenamePostingStream{
			table: "filename_discovery_tokens", key: "p.token_key",
			where: "p.token_key=?", args: []any{generation, key},
		}
	}
	streams = append(streams, exact(key))
	numeric := true
	for _, r := range seed {
		if !unicode.IsNumber(r) {
			numeric = false
			break
		}
	}
	if numeric {
		return streams
	}
	// Hex digits have an ASCII successor; incrementing the last digit gives
	// the strict upper bound for every byte key with this nonempty prefix.
	end := []byte(key)
	end[len(end)-1]++
	streams = append(streams, filenamePostingStream{
		table: "filename_discovery_tokens", key: "p.token_key",
		where: "p.token_key>? AND p.token_key<?", args: []any{generation, key, string(end)}, tuple: true,
	})
	if utf8.RuneCountInString(seed) < 5 {
		return streams
	}
	deletions := filenamediscovery.DeleteKeys(seed)
	for _, deletion := range deletions {
		streams = append(streams, exact(hex.EncodeToString([]byte(deletion))))
	}
	for _, deletion := range append([]string{seed}, deletions...) {
		streams = append(streams, filenamePostingStream{
			table: "filename_discovery_deletes", key: "p.token_key",
			where: "p.delete_key=?", args: []any{generation, hex.EncodeToString([]byte(deletion))}, tuple: true,
		})
	}
	return streams
}
