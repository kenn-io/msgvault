package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

type activityReadWindow struct {
	FromDate, ToDate string
	After, Before    *time.Time
	ConversationID   int64
	Subject          ActivitySubject
}

func (s *Store) normalizedActivityFilters(ctx context.Context, f ActivityFilters) (ActivityFilters, error) {
	if f.MaxDirect < 0 {
		return f, fmt.Errorf("%w: negative direct counterpart limit", ErrInvalidActivityRequest)
	}
	if f.Exclusions.ShortWindowDays == 0 && f.Exclusions.LongWindowDays == 0 && f.Exclusions.ExcludeAddressPatterns == nil && f.Exclusions.ExcludeDomainPatterns == nil {
		f.Exclusions = persontiers.DefaultConfig()
	}
	if err := f.Exclusions.Validate(); err != nil {
		return f, fmt.Errorf("%w: %w", ErrInvalidActivityRequest, err)
	}
	if f.MaxDirect == 0 {
		policy, err := s.ActivityDirectLimitTransitionContext(ctx)
		if err != nil {
			return f, err
		}
		f.maxDirect = policy.Target
	}
	return f, nil
}

func (f ActivityFilters) directLimit() int {
	if f.MaxDirect > 0 {
		return f.MaxDirect
	}
	if f.maxDirect > 0 {
		return f.maxDirect
	}
	return DefaultMaxDirectActivityCounterparts
}

func activitySubjectClause(subject ActivitySubject, message string) (string, []any, error) {
	if subject.PersonID < 0 || (subject.PersonID != 0 && len(subject.ParticipantIDs) > 0) {
		return "", nil, fmt.Errorf("%w: select one activity subject", ErrInvalidActivityRequest)
	}
	if subject.PersonID > 0 {
		return ` AND EXISTS(SELECT 1 FROM person_participants subject_pp WHERE subject_pp.person_id=? AND ` + activityParticipantMatchSQL(message, "subject_pp.participant_id") + `)`, []any{subject.PersonID}, nil
	}
	if len(subject.ParticipantIDs) > 0 {
		args := make([]any, len(subject.ParticipantIDs))
		for i, id := range subject.ParticipantIDs {
			if id <= 0 {
				return "", nil, fmt.Errorf("%w: invalid participant id", ErrInvalidActivityRequest)
			}
			args[i] = id
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		return ` AND EXISTS(SELECT 1 FROM participants subject_p WHERE subject_p.id IN (` + marks + `) AND ` + activityParticipantMatchSQL(message, "subject_p.id") + `)`, args, nil
	}
	return "", nil, nil
}

// exclusionPatternsSQL implements Config's exact, prefix and subdomain patterns
// without treating SQL wildcard characters in a configured pattern as rules.
func exclusionPatternsSQL(expr string, patterns []string) (string, []any) {
	terms := []string{}
	args := []any{}
	for _, pattern := range patterns {
		if suffix, subdomains := strings.CutPrefix(pattern, "*"); subdomains && strings.HasPrefix(suffix, ".") {
			terms = append(terms, `SUBSTR(`+expr+`,LENGTH(`+expr+`)-?+1)=?`)
			args = append(args, len([]rune(suffix)), suffix)
		} else if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
			terms = append(terms, `SUBSTR(`+expr+`,1,?)=?`)
			args = append(args, len([]rune(prefix)), prefix)
		} else {
			terms = append(terms, expr+`=?`)
			args = append(args, pattern)
		}
	}
	if len(terms) == 0 {
		return "FALSE", args
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}

// activityAddressExclusionsCTE applies the tier evaluator's address rules to
// visible identities bound to a saved person or linked to an unsaved contact.
// subjects must return participant_id and nullable person_id once per participant.
func (s *Store) activityAddressExclusionsCTE(ctx context.Context, subjects string, f ActivityFilters) (string, []any) {
	at := `INSTR(address,'@')`
	if s.IsPostgreSQL() {
		at = `STRPOS(address,'@')`
	}
	valid := at + `>1 AND ` + at + `<LENGTH(address) AND address NOT LIKE '% %' AND address NOT LIKE '%'||CHAR(9)||'%' AND address NOT LIKE '%'||CHAR(10)||'%' AND address NOT LIKE '%'||CHAR(13)||'%'`
	if s.IsPostgreSQL() {
		valid = strings.ReplaceAll(valid, "CHAR(", "CHR(")
	}
	visible, args := activityIdentityPopulationSQL(ctx)
	domain, domainArgs := exclusionPatternsSQL(`SUBSTR(address,`+at+`+1)`, f.Exclusions.ExcludeDomainPatterns)
	local, localArgs := exclusionPatternsSQL(`SUBSTR(address,1,CASE WHEN `+at+`>0 THEN `+at+`-1 ELSE 0 END)`, f.Exclusions.ExcludeAddressPatterns)
	args = append(args, domainArgs...)
	args = append(args, localArgs...)
	return `exclusion_subjects AS MATERIALIZED (` + subjects + `), exclusion_visible AS MATERIALIZED (
 ` + visible + `
 ), exclusion_links(participant_id,member_id) AS (
 SELECT subject.participant_id,p.id FROM exclusion_subjects subject
 LEFT JOIN person_participants bound ON bound.person_id=subject.person_id
 JOIN exclusion_visible p ON p.id=COALESCE(bound.participant_id,subject.participant_id)
 UNION
 SELECT member.participant_id,p.id FROM exclusion_links member
 JOIN exclusion_subjects subject ON subject.participant_id=member.participant_id AND subject.person_id IS NULL
 JOIN participant_links l ON l.participant_a=member.member_id OR l.participant_b=member.member_id
 JOIN exclusion_visible p ON p.id=CASE WHEN l.participant_a=member.member_id THEN l.participant_b ELSE l.participant_a END AND p.person_id IS NULL
 ), exclusion_members AS MATERIALIZED (
 SELECT member.participant_id,p.email_address,p.phone_number,p.is_account_owner,pi.identifier_type,pi.identifier_value
 FROM exclusion_links member JOIN exclusion_visible p ON p.id=member.member_id
 LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id
 ), exclusion_addresses AS (
 SELECT participant_id,is_account_owner,LOWER(TRIM(COALESCE(email_address,''))) AS address,
 CASE WHEN TRIM(COALESCE(phone_number,''))<>'' OR (identifier_type<>'email' AND TRIM(identifier_value)<>'') THEN 1 ELSE 0 END AS has_phone
 FROM exclusion_members
 UNION ALL SELECT participant_id,is_account_owner,LOWER(TRIM(identifier_value)),0 FROM exclusion_members WHERE identifier_type='email'
 ), address_exclusions AS (
 SELECT participant_id,CASE WHEN MAX(is_account_owner)=1 THEN 'owner'
 WHEN MAX(CASE WHEN address<>'' THEN 1 ELSE 0 END)=0 THEN ''
 WHEN MIN(CASE WHEN address='' OR (` + valid + ` AND ` + domain + `) THEN 1 ELSE 0 END)=1 THEN 'calendar_room'
 WHEN MAX(has_phone)=0 AND MIN(CASE WHEN address='' OR (` + valid + ` AND ` + local + `) THEN 1 ELSE 0 END)=1 THEN 'automated_address'
 ELSE '' END AS excluded FROM exclusion_addresses GROUP BY participant_id
 )`, args
}

// activityReadCTE applies authorization and live eligibility before identity,
// copy selection or aggregation. It exposes complete representatives and their
// per-message counted flag; callers add logical grouping before page limits.
func (s *Store) activityReadCTE(ctx context.Context, w activityReadWindow, f ActivityFilters) (string, []any, error) {
	if w.ConversationID < 0 || (w.After != nil && w.Before != nil && !w.After.Before(*w.Before)) {
		return "", nil, fmt.Errorf("%w: invalid activity window", ErrInvalidActivityRequest)
	}
	where := s.activityLiveEligibleSQL() + ` AND ` + FullActivityEventWhere("ae")
	scope, args := sourceScopeClause(ctx, "m.source_id")
	where += scope
	windowWhere := []string{}
	windowArgs := []any{}
	if w.ConversationID > 0 {
		windowWhere = append(windowWhere, `ae.message_id IN (SELECT id FROM messages WHERE conversation_id=?)`)
		windowArgs = append(windowArgs, w.ConversationID)
	}
	if w.After != nil {
		windowWhere = append(windowWhere, `ae.occurred_at>=?`)
		windowArgs = append(windowArgs, w.After.UTC())
	}
	if w.Before != nil {
		windowWhere = append(windowWhere, `ae.occurred_at<?`)
		windowArgs = append(windowArgs, w.Before.UTC())
	}
	if w.FromDate != "" || w.ToDate != "" {
		if !validActivityDate(w.FromDate) || !validActivityDate(w.ToDate) || w.FromDate > w.ToDate {
			return "", nil, fmt.Errorf("%w: invalid local dates", ErrInvalidActivityRequest)
		}
		windowWhere = append(windowWhere, `ae.local_date BETWEEN ? AND ?`)
		windowArgs = append(windowArgs, w.FromDate, w.ToDate)
	}
	prefix, events := "WITH RECURSIVE ", "activity_events"
	if len(windowArgs) > 0 {
		// Fence the indexed activity window or conversation IDs before live policy.
		// Otherwise SQLite can start from sources and scan every live message
		// before checking the much smaller date or timestamp range.
		materialization := "MATERIALIZED"
		if s.IsPostgreSQL() {
			materialization = "NOT MATERIALIZED"
		}
		prefix += `activity_window AS ` + materialization + ` (SELECT * FROM activity_events ae WHERE ` + strings.Join(windowWhere, " AND ") + `), `
		events = "activity_window"
		args = append(windowArgs, args...)
	}
	subject, subjectArgs, err := activitySubjectClause(w.Subject, "m")
	if err != nil {
		return "", nil, err
	}
	where += subject
	args = append(args, subjectArgs...)
	maxDirect := f.directLimit()
	maxSQL := strconv.Itoa(maxDirect)
	addressCTE, addressArgs := s.activityAddressExclusionsCTE(ctx, `SELECT participant_id,person_id FROM population GROUP BY participant_id,person_id`, f)
	args = append(args, addressArgs...)
	exclusion := `CASE WHEN cp.link_owner=1 THEN 'owner' ELSE ax.excluded END`
	role := `CASE WHEN cp.recipient_type='from' THEN CASE WHEN e.ref_kind='meeting' THEN 'organizer' ELSE 'sender' END WHEN cp.recipient_type='member' THEN 'member' WHEN e.ref_kind='meeting' THEN 'attendee' ELSE 'addressed' END`
	// Persisted person links carry the projector's strongest saved-person role.
	// Explicit limits recompute evidence before the same strongest-link arbitration.
	if f.MaxDirect == 0 {
		role = `COALESCE(aep.role,` + role + `)`
	}
	evidence := `CASE WHEN e.direction='outbound' AND cp.audience_n<=` + maxSQL + ` THEN 'direct' WHEN e.direction='inbound' AND cp.recipient_type='from' THEN 'direct' ELSE 'co_presence' END`
	if f.MaxDirect == 0 {
		evidence = `COALESCE(aep.evidence,` + evidence + `)`
	}
	listExcluded := "0"
	if f.Exclusions.ExcludeListMessages {
		listExcluded = `CASE WHEN f.list_id IS NOT NULL THEN 1 ELSE 0 END`
	}
	large := `CASE WHEN e.direction='inbound' AND COALESCE(cp.audience_n,0)>` + maxSQL + ` THEN 1 ELSE 0 END`
	counted := CountedEventWhere(CountedEventInputs{Evidence: "f.evidence", Role: "f.role", Direction: "f.direction", RefKind: "f.ref_kind", ListExcluded: listExcluded, LargeInbound: "f.large_inbound"})
	countedSubject := ""
	if w.Subject.PersonID != 0 || len(w.Subject.ParticipantIDs) > 0 {
		predicate, subjectFactArgs, subjectErr := activityFactSubjectClause(w.Subject)
		if subjectErr != nil {
			return "", nil, subjectErr
		}
		countedSubject = " AND " + predicate
		args = append(args, subjectFactArgs...)
	}
	// These reused sets must be evaluated once. SQLite can otherwise inline
	// them into each correlated fact lookup and rebuild the scoped population
	// repeatedly while scoring a burst. The fences retain all eligible rows
	// before copy selection, grouping and pagination. Prefer counted contact
	// for the requested subject when copies differ, then the fullest thread.
	// Thread sizes include eligible messages outside this read's window so
	// widening the window keeps the same account's copy.
	query := prefix + `eligible AS MATERIALIZED (
 SELECT ae.message_id,m.source_id,m.conversation_id,ae.channel,ae.direction,ae.occurred_at,ae.local_date,ae.ref_kind,
 m.message_type,m.rfc822_message_id,m.list_id,src.source_type,
 CASE WHEN ` + matrixPendingEncryptedSQL("m") + ` THEN 1 ELSE 0 END AS encrypted,
 CASE WHEN ae.ref_kind='meeting' OR ` + countedMessageTypeSQL("m", "c") + ` THEN 1 ELSE 0 END AS contact_eligible
 FROM ` + events + ` ae JOIN messages m ON m.id=ae.message_id JOIN sources src ON src.id=m.source_id LEFT JOIN conversations c ON c.id=m.conversation_id WHERE ` + where + `
 ), population AS MATERIALIZED (` + s.activityCounterpartsSQL(`SELECT message_id FROM eligible`) + `), owner_population AS (
 SELECT *,MAX(is_owner) OVER person_link AS link_owner,ROW_NUMBER() OVER person_link AS link_rank
 FROM population WINDOW person_link AS (PARTITION BY message_id,person_id,CASE WHEN person_id IS NULL THEN participant_id ELSE 0 END)
 ), ` + addressCTE + `, exclusions AS (
 SELECT cp.*,` + exclusion + ` AS excluded FROM owner_population cp JOIN address_exclusions ax ON ax.participant_id=cp.participant_id
 ), audience_facts AS (
 SELECT *,SUM(CASE WHEN excluded<>'owner' AND link_rank=1 THEN 1 ELSE 0 END) OVER (PARTITION BY message_id) AS audience_n FROM exclusions
 ), raw_facts AS MATERIALIZED (
 SELECT e.*,cp.participant_id,cp.person_id,cp.recipient_type,cp.excluded,
 ` + large + ` AS large_inbound,` + role + ` AS raw_role,` + evidence + ` AS raw_evidence
 FROM eligible e LEFT JOIN audience_facts cp ON cp.message_id=e.message_id
 LEFT JOIN activity_event_persons aep ON aep.message_id=e.message_id AND aep.person_id=cp.person_id
 ), facts AS MATERIALIZED (
 SELECT cp.*,FIRST_VALUE(raw_role) OVER strongest_link AS role,FIRST_VALUE(raw_evidence) OVER strongest_link AS evidence
 FROM raw_facts cp WINDOW strongest_link AS (
 PARTITION BY message_id,person_id,CASE WHEN person_id IS NULL THEN participant_id ELSE 0 END
 ORDER BY CASE WHEN raw_evidence='direct' THEN 0 ELSE 1 END,
 CASE raw_role WHEN 'sender' THEN 0 WHEN 'organizer' THEN 1 WHEN 'addressed' THEN 2 WHEN 'attendee' THEN 3 WHEN 'member' THEN 4 ELSE 5 END)
 ), scored AS (
 SELECT f.message_id,f.source_id,f.conversation_id,f.channel,f.direction,f.occurred_at,f.local_date,f.ref_kind,
 f.message_type,f.rfc822_message_id,f.list_id,f.source_type,f.encrypted,f.contact_eligible,
 MAX(f.large_inbound) AS large_inbound,
 CASE WHEN f.contact_eligible=1 AND f.encrypted=0
 THEN MAX(CASE WHEN f.excluded=''` + countedSubject + ` AND ` + counted + ` THEN 1 ELSE 0 END) ELSE 0 END AS counted,
 CASE WHEN ` + listExcluded + `=1 OR (MAX(CASE WHEN f.recipient_type='from' AND f.excluded NOT IN ('','owner') THEN 1 ELSE 0 END)=1
 AND MAX(CASE WHEN f.recipient_type='from' AND f.excluded='' THEN 1 ELSE 0 END)=0) THEN 0 ELSE 1 END AS non_noise
 FROM facts f GROUP BY f.message_id,f.source_id,f.conversation_id,f.channel,f.direction,f.occurred_at,f.local_date,f.ref_kind,
 f.message_type,f.rfc822_message_id,f.list_id,f.source_type,f.encrypted,f.contact_eligible
 ), thread_sizes AS MATERIALIZED (
 SELECT e.source_id,e.conversation_id,(SELECT COUNT(*) FROM messages m JOIN activity_events ae ON ae.message_id=m.id JOIN sources src ON src.id=m.source_id
 WHERE m.source_id=e.source_id AND m.conversation_id=e.conversation_id AND ` + s.activityLiveEligibleSQL() + ` AND ` + FullActivityEventWhere("ae") + `) AS thread_messages
 FROM scored e GROUP BY e.source_id,e.conversation_id
 ), copies AS (
 SELECT e.*,ROW_NUMBER() OVER (PARTITION BY CASE WHEN channel='email' AND COALESCE(rfc822_message_id,'')<>'' THEN 'email:'||` + s.dialect.RFC822CanonicalIDExpr("rfc822_message_id") + ` ELSE 'id:'||CAST(message_id AS TEXT) END ORDER BY counted DESC,COALESCE(ts.thread_messages,1) DESC,e.source_id,message_id) AS copy_rank,
 COUNT(*) OVER (PARTITION BY CASE WHEN channel='email' AND COALESCE(rfc822_message_id,'')<>'' THEN 'email:'||` + s.dialect.RFC822CanonicalIDExpr("rfc822_message_id") + ` ELSE 'id:'||CAST(message_id AS TEXT) END) AS copy_count
 FROM scored e LEFT JOIN thread_sizes ts ON ts.source_id=e.source_id AND ts.conversation_id=e.conversation_id
 ), representatives AS (SELECT * FROM copies WHERE copy_rank=1)`
	return query, args, nil
}

const activityBurstGroupsCTE = `, burst_groups AS (
 SELECT source_id,COALESCE(conversation_id,0) AS conversation_id,local_date,MIN(channel) AS channel,
 'burst:'||CAST(source_id AS TEXT)||':'||CAST(COALESCE(conversation_id,0) AS TEXT)||':'||local_date AS key,
 MIN(occurred_at) AS first_at,MAX(occurred_at) AS last_at,
 SUM(CASE WHEN direction='inbound' THEN counted ELSE 0 END) AS inbound,
 SUM(CASE WHEN direction='outbound' THEN counted ELSE 0 END) AS outbound,
 COUNT(*) AS message_count,CASE WHEN SUM(counted)=0 OR SUM(non_noise)=0 THEN 1 ELSE 0 END AS noise
 FROM representatives GROUP BY source_id,conversation_id,local_date
)`
