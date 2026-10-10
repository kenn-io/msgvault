package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrInvalidContactLookup = errors.New("invalid contact lookup")

type ContactCandidateQuery struct {
	Query   string
	Limit   int
	AfterID int64
}

type ContactCandidate struct {
	PersonID    int64    `json:"person_id"`
	PersonUID   string   `json:"person_uid"`
	DisplayName string   `json:"display_name"`
	Revision    int64    `json:"revision"`
	MatchKinds  []string `json:"match_kinds"`
}

type ContactCandidatePage struct {
	Candidates       []ContactCandidate `json:"candidates"`
	HasMore          bool               `json:"has_more"`
	NextAfterID      int64              `json:"next_after_id"`
	Ambiguous        bool               `json:"ambiguous"`
	IdentityRevision int64              `json:"identity_revision"`
}

// ValidateContactCandidateQuery is shared by the Store, HTTP and MCP readers.
// SQLite LOWER folds ASCII only; Unicode spelling is otherwise literal.
func ValidateContactCandidateQuery(q ContactCandidateQuery) error {
	name := strings.TrimSpace(q.Query)
	validQuery := utf8.ValidString(q.Query) && len(name) > 0 && len(name) <= 256 && len(strings.Fields(name)) <= 16
	if !validQuery || q.Limit < 0 || q.Limit > 100 || q.AfterID < 0 {
		return fmt.Errorf("%w: query must contain 1..256 UTF-8 bytes and at most 16 tokens; "+
			"limit 1..100; after_id nonnegative", ErrInvalidContactLookup)
	}
	return nil
}

// candidateNameLanes deliberately checks only names. It never promotes a
// participant, follows a suggestion, or matches a telephone identifier.
var candidateNameLanes = []string{
	`LOWER(COALESCE(p.display_name,'')) LIKE LOWER(?) ESCAPE '\'`,
	`EXISTS (SELECT 1 FROM person_names n WHERE n.person_id=p.id
 AND n.active_until IS NULL AND n.superseded_at IS NULL
 AND LOWER(COALESCE(n.formatted,'') || ' ' || COALESCE(n.honorific_prefixes,'')
  || ' ' || COALESCE(n.given_name,'') || ' ' || COALESCE(n.additional_names,'')
  || ' ' || COALESCE(n.family_name,'') || ' ' || COALESCE(n.secondary_surname,'')
  || ' ' || COALESCE(n.honorific_suffixes,'') || ' ' || COALESCE(n.generation,'')
  || ' ' || COALESCE(n.sort_as,'') || ' ' || n.original_value) LIKE LOWER(?) ESCAPE '\')`,
	`EXISTS (SELECT 1 FROM person_participants pp JOIN participants member ON member.id=pp.participant_id
 WHERE pp.person_id=p.id AND LOWER(COALESCE(member.display_name,'')) LIKE LOWER(?) ESCAPE '\')`,
	`EXISTS (SELECT 1 FROM person_participants pp JOIN message_recipients mr ON mr.participant_id=pp.participant_id
 WHERE pp.person_id=p.id AND LOWER(COALESCE(mr.display_name,'')) LIKE LOWER(?) ESCAPE '\')`,
}

func contactCandidatePredicate(query string) (string, []any) {
	clauses := []string{}
	args := []any{}
	escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for token := range strings.FieldsSeq(strings.TrimSpace(query)) {
		clauses = append(clauses, "("+strings.Join(candidateNameLanes, " OR ")+")")
		for range candidateNameLanes {
			args = append(args, "%"+escape.Replace(token)+"%")
		}
	}
	return strings.Join(clauses, " AND "), args
}

var candidateMatchKinds = []string{"saved_name", "curated_name", "bound_observed_name", "archived_alias"}

func (s *Store) FindContactCandidatesContext(
	ctx context.Context, q ContactCandidateQuery,
) (*ContactCandidatePage, error) {
	if err := ValidateContactCandidateQuery(q); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}
	page := &ContactCandidatePage{Candidates: []ContactCandidate{}}
	predicate, args := contactCandidatePredicate(q.Query)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		page.IdentityRevision, err = s.currentIdentityRevisionTxContext(ctx, tx)
		if err != nil {
			return err
		}
		// Ambiguity describes the whole query, including when the current page is
		// the last one or limit=1. A first page reads limit+1 >= 2 rows and
		// answers it directly. Archived aliases make each pass read every
		// recipient row of the bound participants, so only later pages pay for
		// a separate pass, which stops after two roots.
		if q.AfterID > 0 {
			var matched int
			countQuery := `SELECT COUNT(*) FROM (SELECT p.id FROM persons p WHERE ` + predicate + ` LIMIT 2) matched`
			if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&matched); err != nil {
				return err
			}
			page.Ambiguous = matched > 1
		}
		matchColumns := []string{}
		matchArgs := []any{}
		escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		// Report every lane used by any token, including queries whose tokens
		// match across different saved, curated and observed names.
		for _, lane := range candidateNameLanes {
			clauses := []string{}
			for token := range strings.FieldsSeq(strings.TrimSpace(q.Query)) {
				clauses = append(clauses, lane)
				matchArgs = append(matchArgs, "%"+escape.Replace(token)+"%")
			}
			matchColumns = append(matchColumns, "("+strings.Join(clauses, " OR ")+")")
		}
		allArgs := make([]any, 0, len(matchArgs)+len(args)+2)
		allArgs = append(allArgs, matchArgs...)
		allArgs = append(allArgs, args...)
		allArgs = append(allArgs, q.AfterID, limit+1)
		rows, err := tx.QueryContext(ctx, `SELECT p.id,p.vcard_uid,COALESCE(p.display_name,''),p.revision,`+
			strings.Join(matchColumns, ",")+`
   FROM persons p WHERE `+predicate+` AND p.id>? ORDER BY p.id LIMIT ?`, allArgs...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c ContactCandidate
			matches := make([]bool, len(candidateMatchKinds))
			dest := []any{&c.PersonID, &c.PersonUID, &c.DisplayName, &c.Revision}
			for i := range matches {
				dest = append(dest, &matches[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			c.MatchKinds = []string{}
			for i, matched := range matches {
				if matched {
					c.MatchKinds = append(c.MatchKinds, candidateMatchKinds[i])
				}
			}
			page.Candidates = append(page.Candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find contact candidates: %w", err)
	}
	if q.AfterID == 0 {
		page.Ambiguous = len(page.Candidates) > 1
	}
	if len(page.Candidates) > limit {
		page.HasMore = true
		page.Candidates = page.Candidates[:limit]
	}
	if len(page.Candidates) > 0 {
		page.NextAfterID = page.Candidates[len(page.Candidates)-1].PersonID
	}
	return page, nil
}
