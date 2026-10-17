package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

// PersonTierCandidate is a participant approximation, never an automatically
// created profile. A pending identity match should be reviewed before promotion.
type PersonTierCandidate struct {
	ParticipantID            int64  `json:"participant_id"`
	CanonicalID              string `json:"canonical_id,omitempty"`
	Basis                    string `json:"basis"`
	SuggestedCommand         string `json:"suggested_command"`
	OutboundShort            int    `json:"outbound_short"`
	InboundShort             int    `json:"inbound_short"`
	ActiveDaysShort          int    `json:"active_days_short"`
	IdentityMatchCandidateID *int64 `json:"identity_match_candidate_id,omitempty"`
}

type PersonTierCandidatePage struct {
	Candidates []PersonTierCandidate `json:"candidates"`
	// NextCursor is the decimal minimum participant ID of the last scanned
	// canonical group. A page can be empty after filtering and still have a cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

type tierCandidateGroup struct {
	id        int64
	canonical string
	members   []int64
	signals   persontiers.Signals
	days      map[string]struct{}
}

// PersonTierCandidatesContext reads at most limit participant groups, then
// evaluates only messages involving their members. It shares the real activity
// loader and classifier's ownership, envelope and membership rules. No activity
// projection, person promotion, linking or queue mutation occurs here.
func (s *Store) PersonTierCandidatesContext(ctx context.Context, afterID int64, limit int, asOf time.Time, cfg persontiers.Config, timezone string, maxDirect int) (PersonTierCandidatePage, error) {
	page := PersonTierCandidatePage{Candidates: []PersonTierCandidate{}}
	if afterID < 0 || limit < 1 || limit > personTierSignalPageSize || asOf.IsZero() {
		return page, fmt.Errorf("%w: candidates require a nonnegative cursor, limit 1..500, and nonzero as-of date", ErrInvalidActivityRequest)
	}
	if err := cfg.Validate(); err != nil {
		return page, fmt.Errorf("%w: %w", ErrInvalidActivityRequest, err)
	}
	if timezone == "" {
		timezone = "UTC"
	}
	if timezone == "Local" {
		return page, fmt.Errorf("%w: candidates require an explicit timezone", ErrInvalidActivityRequest)
	}
	zone, err := time.LoadLocation(timezone)
	if err != nil {
		return page, fmt.Errorf("%w: candidate timezone: %w", ErrInvalidActivityRequest, err)
	}
	if maxDirect <= 0 {
		maxDirect = DefaultMaxDirectActivityCounterparts
	}
	local := asOf.In(zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	from := day.AddDate(0, 0, -(cfg.ShortWindowDays - 1))
	through := day.AddDate(0, 0, 1)
	groups, more, err := s.personTierCandidateGroupsContext(ctx, afterID, limit)
	if err != nil {
		return page, err
	}
	if len(groups) == 0 {
		return page, nil
	}
	if more {
		page.NextCursor = strconv.FormatInt(groups[len(groups)-1].id, 10)
	}
	byParticipant, err := s.personTierCandidateMembersContext(ctx, groups)
	if err != nil {
		return page, err
	}
	// Reuse the evaluator's hard exclusions, independent of the signal floor or
	// scheduling switches. Empty authority inputs keep this entirely observational.
	allowed := map[int64]*tierCandidateGroup{}
	memberIDs := []int64{}
	for _, group := range groups {
		decision := persontiers.Evaluate(group.signals, cfg, persontiers.Prior{}, persontiers.ManualState{}, local, true)
		if decision.Rule == "owner" || decision.Rule == "calendar_room" || decision.Rule == "automated_address" {
			continue
		}
		allowed[group.id] = group
		memberIDs = append(memberIDs, group.members...)
	}
	if len(memberIDs) == 0 {
		return page, nil
	}
	messageIDs, err := s.personTierCandidateMessageIDsContext(ctx, memberIDs, from.UTC(), through.UTC(), cfg.ExcludeListMessages)
	if err != nil {
		return page, err
	}
	for start := 0; start < len(messageIDs); start += personTierSignalPageSize {
		candidates, err := s.LoadActivityCandidatesByIDContext(ctx, messageIDs[start:min(start+personTierSignalPageSize, len(messageIDs))])
		if err != nil {
			return page, fmt.Errorf("load tier candidate metadata: %w", err)
		}
		for _, candidate := range candidates {
			if !candidate.Eligible || IsMeetingMessageType(candidate.MessageType) {
				continue
			}
			at, ok := tierCandidateInstant(candidate)
			if !ok || at.Before(from) || !at.Before(through) {
				continue
			}
			ownerParticipants := map[int64]bool{}
			ownerPersons := map[int64]bool{}
			for _, counterpart := range candidate.Counterparts {
				if counterpart.IsOwner {
					ownerParticipants[counterpart.ParticipantID] = true
					if counterpart.PersonID != nil {
						ownerPersons[*counterpart.PersonID] = true
					}
				}
			}
			audience := map[int64]struct{}{}
			for i := range candidate.Counterparts {
				counterpart := &candidate.Counterparts[i]
				counterpart.IsOwner = counterpart.IsOwner || ownerParticipants[counterpart.ParticipantID] || (counterpart.PersonID != nil && ownerPersons[*counterpart.PersonID])
				if !counterpart.IsOwner {
					audience[counterpart.ParticipantID] = struct{}{}
				}
				// Positive IDs identify this page's canonical groups. All other
				// participants stay separate under negative ephemeral IDs. Ownership
				// from existing bindings was preserved above before replacing PersonID.
				id := -counterpart.ParticipantID
				if group := byParticipant[counterpart.ParticipantID]; group != nil {
					id = group.id
				}
				counterpart.PersonID = new(id)
			}
			// Inbound senders are direct in the spine even on broadcasts. At
			// participant grain both directions require the bounded audience.
			if len(audience) > maxDirect {
				continue
			}
			classified := ClassifyActivityCandidate(candidate, maxDirect)
			date := at.In(zone).Format(time.DateOnly)
			for _, link := range classified.Persons {
				group := allowed[link.PersonID]
				if group == nil || link.Evidence != EvidenceDirect {
					continue
				}
				counted := false
				if classified.Direction == DirectionInbound && link.Role == RoleSender {
					group.signals.InboundShort++
					counted = true
				}
				if classified.Direction == DirectionOutbound && (link.Role == RoleAddressed || link.Role == RoleMember) {
					group.signals.OutboundShort++
					counted = true
				}
				if counted {
					group.days[date] = struct{}{}
				}
			}
		}
	}
	for _, group := range groups {
		if allowed[group.id] == nil {
			continue
		}
		signal := group.signals
		if signal.OutboundShort < cfg.Tier2.MinOutboundLong || signal.InboundShort < cfg.Tier2.MinInboundLong || len(group.days) < cfg.Tier2.MinActiveDaysLong {
			continue
		}
		pending, err := s.personTierCandidateIdentityMatchContext(ctx, group.members)
		if err != nil {
			return page, err
		}
		page.Candidates = append(page.Candidates, PersonTierCandidate{
			ParticipantID: group.id, CanonicalID: group.canonical, Basis: "participant_approximation",
			SuggestedCommand: "msgvault person promote " + strconv.FormatInt(group.id, 10),
			OutboundShort:    signal.OutboundShort, InboundShort: signal.InboundShort, ActiveDaysShort: len(group.days), IdentityMatchCandidateID: pending,
		})
	}
	return page, nil
}

func (s *Store) personTierCandidateGroupsContext(ctx context.Context, afterID int64, limit int) ([]*tierCandidateGroup, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,COALESCE(p.canonical_id,'') FROM participants p
 WHERE p.id>? AND NOT EXISTS (SELECT 1 FROM person_participants pp WHERE pp.participant_id=p.id)
 AND (p.canonical_id IS NULL OR p.canonical_id='' OR (
 NOT EXISTS (SELECT 1 FROM participants earlier WHERE earlier.canonical_id=p.canonical_id AND earlier.id<p.id)
 AND NOT EXISTS (SELECT 1 FROM participants bound JOIN person_participants pp ON pp.participant_id=bound.id WHERE bound.canonical_id=p.canonical_id)))
 ORDER BY p.id LIMIT ?`, afterID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list tier candidate groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	groups := []*tierCandidateGroup{}
	for rows.Next() {
		group := &tierCandidateGroup{days: map[string]struct{}{}}
		if err = rows.Scan(&group.id, &group.canonical); err != nil {
			return nil, false, fmt.Errorf("scan tier candidate group: %w", err)
		}
		groups = append(groups, group)
	}
	if err = rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate tier candidate groups: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, false, fmt.Errorf("close tier candidate groups: %w", err)
	}
	more := len(groups) > limit
	if more {
		groups = groups[:limit]
	}
	return groups, more, nil
}

func (s *Store) personTierCandidateMembersContext(ctx context.Context, groups []*tierCandidateGroup) (map[int64]*tierCandidateGroup, error) {
	args := make([]any, 0, len(groups)*2)
	canonical := map[string]*tierCandidateGroup{}
	representatives := map[int64]*tierCandidateGroup{}
	for _, group := range groups {
		args = append(args, group.id)
		representatives[group.id] = group
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(groups)), ",")
	condition := "p.id IN (" + marks + ")"
	for _, group := range groups {
		if group.canonical != "" {
			canonical[group.canonical] = group
			args = append(args, group.canonical)
		}
	}
	if len(args) > len(groups) {
		condition += " OR p.canonical_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)-len(groups)), ",") + ")"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,COALESCE(p.canonical_id,''),COALESCE(p.email_address,''),COALESCE(p.phone_number,''),
 COALESCE(pi.identifier_type,''),COALESCE(pi.identifier_value,''),`+personTierOwnerMatch("p.id", "")+`
 FROM participants p LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE `+condition+`
 ORDER BY p.id,pi.identifier_type,pi.identifier_value`, args...)
	if err != nil {
		return nil, fmt.Errorf("read tier candidate members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byParticipant := map[int64]*tierCandidateGroup{}
	for rows.Next() {
		var id int64
		var key, email, phone, kind, value string
		var owner bool
		if err = rows.Scan(&id, &key, &email, &phone, &kind, &value, &owner); err != nil {
			return nil, fmt.Errorf("scan tier candidate member: %w", err)
		}
		group := representatives[id]
		if group == nil {
			group = canonical[key]
		}
		if group == nil {
			continue
		}
		if byParticipant[id] == nil {
			group.members = append(group.members, id)
			byParticipant[id] = group
		}
		signal := &group.signals
		signal.Owner = signal.Owner || owner
		signal.HasPhoneOrChat = signal.HasPhoneOrChat || strings.TrimSpace(phone) != "" || (kind != "" && kind != "email" && strings.TrimSpace(value) != "")
		emails := []string{email}
		if kind == "email" {
			emails = append(emails, value)
		}
		for _, address := range emails {
			address = strings.ToLower(strings.TrimSpace(address))
			if address != "" && !slices.Contains(signal.Emails, address) {
				signal.Emails = append(signal.Emails, address)
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tier candidate members: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("close tier candidate members: %w", err)
	}
	return byParticipant, nil
}

func (s *Store) personTierCandidateMessageIDsContext(ctx context.Context, memberIDs []int64, from, through time.Time, excludeList bool) ([]int64, error) {
	seen := map[int64]struct{}{}
	for start := 0; start < len(memberIDs); start += personTierSignalPageSize {
		members := memberIDs[start:min(start+personTierSignalPageSize, len(memberIDs))]
		args := make([]any, 0, len(members)+3)
		for _, id := range members {
			args = append(args, id)
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(members)), ",")
		timestamp := "COALESCE(m.sent_at,m.received_at,m.internal_date)"
		bounds := timestamp + ">=? AND " + timestamp + "<?"
		if !s.IsPostgreSQL() {
			bounds = "julianday(" + timestamp + ")>=julianday(?) AND julianday(" + timestamp + ")<julianday(?)"
		}
		exclude := 0
		if excludeList {
			exclude = 1
		}
		args = append(args, from, through, exclude)
		rows, err := s.db.QueryContext(ctx, `WITH page_participants AS (SELECT id FROM participants WHERE id IN (`+marks+`)), related_messages AS (
   SELECT mr.message_id FROM page_participants p JOIN message_recipients mr ON mr.participant_id=p.id WHERE mr.recipient_type IN ('from','to','cc','bcc')
   UNION SELECT m.id FROM page_participants p JOIN messages m ON m.sender_id=p.id
   WHERE NOT EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from')
   UNION SELECT m.id FROM page_participants p JOIN conversation_participants cp ON cp.participant_id=p.id
   JOIN messages m ON m.conversation_id=cp.conversation_id
   WHERE NOT EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type IN ('to','cc','bcc'))
  ) SELECT m.id FROM related_messages r JOIN messages m ON m.id=r.message_id
  WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND `+bounds+`
  AND (m.list_id IS NULL OR ?=0) ORDER BY m.id`, args...)
		if err != nil {
			return nil, fmt.Errorf("read tier candidate message ids: %w", err)
		}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan tier candidate message id: %w", err)
			}
			seen[id] = struct{}{}
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate tier candidate message ids: %w", err)
		}
		if err = rows.Close(); err != nil {
			return nil, fmt.Errorf("close tier candidate message ids: %w", err)
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func tierCandidateInstant(candidate ActivityCandidate) (time.Time, bool) {
	for _, at := range []*time.Time{candidate.SentAt, candidate.ReceivedAt, candidate.InternalDate} {
		if at != nil && !at.IsZero() {
			return *at, true
		}
	}
	return time.Time{}, false
}

func (s *Store) personTierCandidateIdentityMatchContext(ctx context.Context, members []int64) (*int64, error) {
	var first *int64
	for start := 0; start < len(members); start += personTierSignalPageSize {
		chunk := members[start:min(start+personTierSignalPageSize, len(members))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)*2)
		for range 2 {
			for _, id := range chunk {
				args = append(args, id)
			}
		}
		var pending sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT MIN(id) FROM identity_match_candidates WHERE state IN ('candidate','conflict')
  AND ((left_kind='participant' AND left_id IN (`+marks+`)) OR (right_kind='participant' AND right_id IN (`+marks+`)))`, args...).Scan(&pending)
		if err != nil {
			return nil, fmt.Errorf("read tier candidate pending identity match: %w", err)
		}
		if pending.Valid && (first == nil || pending.Int64 < *first) {
			first = new(pending.Int64)
		}
	}
	return first, nil
}
