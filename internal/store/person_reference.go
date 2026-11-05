package store

import (
	"context"
	"fmt"
	"net/mail"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/textimport"
)

type PersonRefKind string

type PersonCandidate struct {
	PersonID             int64
	PersonUID            string
	ParticipantIDs       []int64
	DisplayName, Address string
}

// activityParticipantMatchSQL mirrors the projector's sender, explicit-envelope
// and recipient-less room fallback population. Expressions are trusted SQL.
func activityParticipantMatchSQL(m, participant string) string {
	return `((` + m + `.sender_id=` + participant + ` AND NOT EXISTS(
 SELECT 1 FROM message_recipients subject_from WHERE subject_from.message_id=` + m + `.id AND subject_from.recipient_type='from')) OR EXISTS(SELECT 1 FROM message_recipients subject_mr WHERE subject_mr.message_id=` + m + `.id AND subject_mr.participant_id=` + participant + `) OR (
 NOT EXISTS(SELECT 1 FROM message_recipients subject_explicit WHERE subject_explicit.message_id=` + m + `.id AND subject_explicit.recipient_type IN ('to','cc','bcc'))
 AND EXISTS(SELECT 1 FROM conversation_participants subject_cp WHERE subject_cp.conversation_id=` + m + `.conversation_id AND subject_cp.participant_id=` + participant + `)))`
}

// ResolvePersonReferenceContext uses archive SQL only. Scope and person grants
// are applied before ordering and limiting, including during cluster traversal.
func (s *Store) ResolvePersonReferenceContext(ctx context.Context, kind PersonRefKind, value string, allowedPersonIDs []int64, limit int) ([]PersonCandidate, error) {
	result := []PersonCandidate{}
	if limit < 1 || limit > 11 {
		return nil, fmt.Errorf("%w: candidate limit must be 1..11", ErrInvalidActivityRequest)
	}
	if allowedPersonIDs != nil && len(allowedPersonIDs) == 0 {
		return result, nil
	}
	value = strings.TrimSpace(value)
	var numeric int64
	switch kind {
	case PersonRefKind(IdentityMatchPerson), "participant":
		var err error
		numeric, err = strconv.ParseInt(value, 10, 64)
		if err != nil || numeric <= 0 {
			return nil, fmt.Errorf("%w: invalid %s reference", ErrInvalidActivityRequest, kind)
		}
	case PersonRefKind(ContactAddressEmail):
		value = strings.ToLower(value)
		address, err := mail.ParseAddress(value)
		if err != nil || address.Address != value || address.Name != "" {
			return nil, fmt.Errorf("%w: invalid email reference", ErrInvalidActivityRequest)
		}
	case "phone":
		var err error
		value, err = textimport.NormalizePhone(value)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid phone reference", ErrInvalidActivityRequest)
		}
	case "name":
		if value == "" {
			return nil, fmt.Errorf("%w: empty name", ErrInvalidActivityRequest)
		}
	default:
		return nil, fmt.Errorf("%w: unknown person reference kind", ErrInvalidActivityRequest)
	}
	scope, args := sourceScopeClause(ctx, "observed.source_id")
	visible := "TRUE"
	if scope != "" {
		visible = `EXISTS(SELECT 1 FROM messages observed WHERE ` + activityParticipantMatchSQL("observed", "p.id") + scope + `)`
	}
	predicate := ""
	personMatch := "FALSE"
	switch kind {
	case PersonRefKind(IdentityMatchPerson):
		predicate = "pp.person_id=?"
		args = append(args, numeric)
	case "participant":
		predicate = "p.id=?"
		args = append(args, numeric)
	case PersonRefKind(ContactAddressEmail):
		predicate = `(` + s.dialect.UnicodeLowerExpression("p.email_address") + `=? OR EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='email' AND ` + s.dialect.UnicodeLowerExpression("pi.identifier_value") + `=?))`
		args = append(args, value, value)
	case "phone":
		predicate = `(p.phone_number=? OR EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='phone' AND pi.identifier_value=?))`
		args = append(args, value, value)
	case "name":
		predicate = s.dialect.UnicodeLowerExpression("p.display_name") + `=` + s.dialect.UnicodeLowerExpression("?")
		args = append(args, value)
	}
	// matching is evaluated once; a saved name may also match its curated name.
	if kind == PersonRefKind(IdentityMatchPerson) {
		personMatch = "person.id=?"
		args = append(args, numeric)
	}
	if kind == "name" {
		personMatch = s.dialect.UnicodeLowerExpression("person.display_name") + `=` + s.dialect.UnicodeLowerExpression("?")
		args = append(args, value)
	}
	grant := ""
	if allowedPersonIDs != nil {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(allowedPersonIDs)), ",")
		grant = " AND candidate.person_id IN (" + marks + ")"
		for _, id := range allowedPersonIDs {
			args = append(args, id)
		}
	}
	args = append(args, limit)
	query := `WITH RECURSIVE visible AS (
 SELECT p.id,p.display_name,COALESCE(p.email_address,p.phone_number,'') AS address,pp.person_id
 FROM participants p LEFT JOIN person_participants pp ON pp.participant_id=p.id WHERE ` + visible + `
 ), matching AS (
 SELECT p.id,pp.person_id FROM participants p LEFT JOIN person_participants pp ON pp.participant_id=p.id
 WHERE EXISTS(SELECT 1 FROM visible v WHERE v.id=p.id) AND ` + predicate + `
 ), reach(root,id) AS (
 SELECT id,id FROM visible WHERE person_id IS NULL
 UNION
 SELECT r.root,CASE WHEN l.participant_a=r.id THEN l.participant_b ELSE l.participant_a END
 FROM reach r JOIN participant_links l ON l.participant_a=r.id OR l.participant_b=r.id
 WHERE EXISTS(SELECT 1 FROM visible v WHERE v.id=CASE WHEN l.participant_a=r.id THEN l.participant_b ELSE l.participant_a END AND v.person_id IS NULL)
 ), roots AS (SELECT id,MIN(root) AS root FROM reach GROUP BY id), candidate AS (
 SELECT person.id AS person_id,person.vcard_uid AS uid,COALESCE(person.display_name,'') AS name,
 COALESCE((SELECT MIN(v.address) FROM visible v WHERE v.person_id=person.id),'') AS address,0 AS cluster
 FROM persons person WHERE (` + personMatch + ` OR EXISTS(SELECT 1 FROM matching m WHERE m.person_id=person.id))`
	if scope != "" {
		query += ` AND EXISTS(SELECT 1 FROM visible v WHERE v.person_id=person.id)`
	}
	query += ` UNION ALL
 SELECT 0,'',COALESCE(v.display_name,''),v.address,r.root FROM roots r JOIN visible v ON v.id=r.root
 WHERE r.id=r.root AND EXISTS(SELECT 1 FROM matching m JOIN roots mr ON mr.id=m.id WHERE mr.root=r.root)
 ) SELECT candidate.person_id,candidate.uid,candidate.name,candidate.address,candidate.cluster FROM candidate WHERE TRUE` + grant + ` ORDER BY LOWER(candidate.name),candidate.person_id,candidate.cluster LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("resolve person reference: %w", err)
	}
	clusters := []int64{}
	for rows.Next() {
		var c PersonCandidate
		var root int64
		if err = rows.Scan(&c.PersonID, &c.PersonUID, &c.DisplayName, &c.Address, &root); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan person reference: %w", err)
		}
		result = append(result, c)
		clusters = append(clusters, root)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("iterate person references: %w", err)
	}
	for i := range result {
		// Fetch only members observed within the same source scope.
		memberScope, memberArgs := sourceScopeClause(ctx, "observed.source_id")
		memberVisible := "TRUE"
		if memberScope != "" {
			memberVisible = `EXISTS(SELECT 1 FROM messages observed WHERE ` + activityParticipantMatchSQL("observed", "p.id") + memberScope + `)`
		}
		where := `EXISTS(SELECT 1 FROM person_participants pp WHERE pp.participant_id=p.id AND pp.person_id=?)`
		ids := []any{result[i].PersonID}
		prefix := ""
		if result[i].PersonID == 0 {
			// Keep traversal inside the same visible unsaved population used
			// to choose the candidate. Hidden nodes cannot bridge two clusters.
			next := `CASE WHEN l.participant_a=m.id THEN l.participant_b ELSE l.participant_a END`
			prefix = `WITH RECURSIVE visible_members AS (
 SELECT p.id FROM participants p WHERE NOT EXISTS(SELECT 1 FROM person_participants pp WHERE pp.participant_id=p.id) AND ` + memberVisible + `
 ), members(id) AS (SELECT id FROM visible_members WHERE id=? UNION SELECT ` + next + ` FROM members m JOIN participant_links l ON l.participant_a=m.id OR l.participant_b=m.id WHERE EXISTS(SELECT 1 FROM visible_members v WHERE v.id=` + next + `)) `
			where = `p.id IN (SELECT id FROM members)`
			ids = append(append([]any{}, memberArgs...), clusters[i])
			memberVisible = "TRUE"
		} else {
			ids = append(ids, memberArgs...)
		}
		members, err := s.db.QueryContext(ctx, s.Rebind(prefix+`SELECT p.id FROM participants p WHERE `+where+` AND `+memberVisible+` ORDER BY p.id`), ids...)
		if err != nil {
			return nil, fmt.Errorf("resolve reference members: %w", err)
		}
		result[i].ParticipantIDs = []int64{}
		for members.Next() {
			var id int64
			if err = members.Scan(&id); err != nil {
				_ = members.Close()
				return nil, fmt.Errorf("scan reference member: %w", err)
			}
			result[i].ParticipantIDs = append(result[i].ParticipantIDs, id)
		}
		err = members.Err()
		_ = members.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate reference members: %w", err)
		}
	}
	return result, nil
}
