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

const personTierCandidateIdentityMatchChunkSize = 499

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
	// participant group. A page can be empty after filtering and still have a cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

type tierCandidateGroup struct {
	id        int64
	canonical string
	members   []int64
	signals   persontiers.Signals
	days      map[string]struct{}
}

type tierCandidateChatAudienceKey struct {
	conversationID int64
	sourceID       int64
}

type tierCandidateAudienceIdentityKind uint8

const (
	tierCandidateAudienceParticipant tierCandidateAudienceIdentityKind = iota + 1
	tierCandidateAudiencePerson
	tierCandidateAudienceGroup
)

type tierCandidateAudienceIdentity struct {
	kind tierCandidateAudienceIdentityKind
	id   int64
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
	edges, err := s.loadLinkEdgesContext(ctx)
	if err != nil {
		return page, err
	}
	clusters := clustersFromEdges(edges)
	groups, more, err := s.personTierCandidateGroupsContext(ctx, afterID, limit, clusters)
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
		chatAudiences, err := s.personTierCandidateChatAudiencesContext(ctx, candidates)
		if err != nil {
			return page, err
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
			ownerClusters := map[int64]bool{}
			ownerPersons := map[int64]bool{}
			for _, counterpart := range candidate.Counterparts {
				if counterpart.IsOwner {
					ownerParticipants[counterpart.ParticipantID] = true
					ownerClusters[participantIdentityClusterID(counterpart.ParticipantID, clusters)] = true
					if counterpart.PersonID != nil {
						ownerPersons[*counterpart.PersonID] = true
					}
				}
			}
			audience := map[int64]struct{}{}
			classificationIDs := map[tierCandidateAudienceIdentity]int64{}
			nextClassificationID := int64(-1)
			for i := range candidate.Counterparts {
				counterpart := &candidate.Counterparts[i]
				counterpart.IsOwner = counterpart.IsOwner || ownerParticipants[counterpart.ParticipantID] ||
					ownerClusters[participantIdentityClusterID(counterpart.ParticipantID, clusters)] ||
					(counterpart.PersonID != nil && ownerPersons[*counterpart.PersonID])
				group := byParticipant[counterpart.ParticipantID]
				identity := tierCandidateAudienceIdentity{
					kind: tierCandidateAudienceParticipant,
					id:   participantIdentityClusterID(counterpart.ParticipantID, clusters),
				}
				if counterpart.PersonID != nil {
					identity = tierCandidateAudienceIdentity{kind: tierCandidateAudiencePerson, id: *counterpart.PersonID}
				} else if group != nil {
					identity = tierCandidateAudienceIdentity{kind: tierCandidateAudienceGroup, id: group.id}
				}
				if !counterpart.IsOwner {
					// Audience limits count participants, even when classification groups aliases.
					audience[counterpart.ParticipantID] = struct{}{}
				}
				// Classifier IDs stay unique across existing people and unbound
				// groups; only candidate groups use positive IDs consumed by allowed.
				if group != nil {
					counterpart.PersonID = new(group.id)
					continue
				}
				id, ok := classificationIDs[identity]
				if !ok {
					id = nextClassificationID
					nextClassificationID--
					classificationIDs[identity] = id
				}
				counterpart.PersonID = new(id)
			}
			// Inbound senders are direct in the spine even on broadcasts. At
			// participant grain both directions require the bounded audience.
			// Chat envelopes can list only addressed recipients, so also enforce
			// the full source-scoped conversation roster limit.
			chatAudience := 0
			if candidate.ConversationID != nil {
				chatAudience = chatAudiences[tierCandidateChatAudienceKey{
					conversationID: *candidate.ConversationID,
					sourceID:       candidate.SourceID,
				}]
			}
			if len(audience) > maxDirect || chatAudience > maxDirect {
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

func participantIdentityClusterID(participantID int64, clusters map[int64]int64) int64 {
	if root, ok := clusters[participantID]; ok {
		return root
	}
	return participantID
}

func personTierResolvedNativeOwnerMatch(participantExpr, messageExpr, senderExpr string) string {
	return `( (` + senderExpr + ` IS NOT NULL AND ` +
		personTierResolvedParticipantMatch(participantExpr, senderExpr) + `) OR (` + senderExpr + ` IS NULL AND EXISTS (
	 SELECT 1 FROM message_recipients owner_from
	 WHERE owner_from.message_id=` + messageExpr + ` AND owner_from.recipient_type='from'
	  AND ` + personTierResolvedParticipantMatch(participantExpr, "owner_from.participant_id") + `
	  AND NOT EXISTS (
	   SELECT 1 FROM message_recipients other_from
	   WHERE other_from.message_id=` + messageExpr + ` AND other_from.recipient_type='from'
	    AND other_from.participant_id<>owner_from.participant_id
	  )
	)))`
}

func (s *Store) personTierCandidateChatAudiencesContext(
	ctx context.Context,
	candidates []ActivityCandidate,
) (map[tierCandidateChatAudienceKey]int, error) {
	audiences := map[tierCandidateChatAudienceKey]int{}
	messageIDs := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Eligible || candidate.ConversationID == nil || IsMeetingMessageType(candidate.MessageType) ||
			activityChannelFor(candidate.ConversationType, false) != ChannelChat {
			continue
		}
		messageIDs = append(messageIDs, candidate.MessageID)
	}
	for start := 0; start < len(messageIDs); start += personTierSignalPageSize {
		end := min(start+personTierSignalPageSize, len(messageIDs))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		query := `WITH candidate_messages AS (
 SELECT m.id,m.conversation_id,m.source_id,m.sender_id,COALESCE(m.source_is_from_me,FALSE) AS source_is_from_me
 FROM messages m WHERE m.id IN (` + marks + `)
 ), chat_conversations AS (
 SELECT m.conversation_id,m.source_id FROM candidate_messages m
 JOIN conversations c ON c.id=m.conversation_id
 WHERE c.conversation_type IN ('chat','group_chat','direct_chat','channel')
 GROUP BY m.conversation_id,m.source_id
		) SELECT c.conversation_id,c.source_id,COUNT(DISTINCT cp.participant_id)
 FROM chat_conversations c JOIN conversation_participants cp ON cp.conversation_id=c.conversation_id
 WHERE NOT ` + personTierResolvedArchivedOwnerMatch("cp.participant_id", "c.conversation_id", "c.source_id") + `
 -- Mirror activity candidate ownership: resolved sender, or the sole From participant when sender_id is absent.
 AND NOT EXISTS (
  SELECT 1 FROM candidate_messages native
  WHERE native.conversation_id=c.conversation_id AND native.source_id=c.source_id
   AND native.source_is_from_me=TRUE
   AND ` + personTierResolvedNativeOwnerMatch("cp.participant_id", "native.id", "native.sender_id") + `
 )
		 GROUP BY c.conversation_id,c.source_id`
		rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), messageIDs[start:end]...)
		if err != nil {
			return nil, fmt.Errorf("read tier candidate chat audiences: %w", err)
		}
		for rows.Next() {
			var conversationID int64
			var sourceID int64
			var audience int64
			if err = rows.Scan(&conversationID, &sourceID, &audience); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan tier candidate chat audience: %w", err)
			}
			key := tierCandidateChatAudienceKey{
				conversationID: conversationID,
				sourceID:       sourceID,
			}
			audiences[key] = int(audience)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate tier candidate chat audiences: %w", err)
		}
		if err = rows.Close(); err != nil {
			return nil, fmt.Errorf("close tier candidate chat audiences: %w", err)
		}
	}
	return audiences, nil
}

func (s *Store) personTierCandidateGroupsContext(
	ctx context.Context,
	afterID int64,
	limit int,
	clusters map[int64]int64,
) ([]*tierCandidateGroup, bool, error) {
	var err error
	canonicalByParticipant := map[int64]string{}
	canonicalCursor := int64(0)
	for {
		canonicalRows, queryErr := s.db.QueryContext(ctx, `SELECT id,canonical_id FROM participants
		 WHERE id>? AND canonical_id IS NOT NULL AND canonical_id<>'' ORDER BY id LIMIT ?`, canonicalCursor, personTierSignalPageSize)
		if queryErr != nil {
			return nil, false, fmt.Errorf("list tier candidate canonical groups: %w", queryErr)
		}
		count := 0
		for canonicalRows.Next() {
			var id int64
			var canonical string
			if err = canonicalRows.Scan(&id, &canonical); err != nil {
				_ = canonicalRows.Close()
				return nil, false, fmt.Errorf("scan tier candidate canonical group: %w", err)
			}
			count++
			canonicalCursor = id
			canonicalByParticipant[id] = canonical
		}
		if err = canonicalRows.Err(); err != nil {
			_ = canonicalRows.Close()
			return nil, false, fmt.Errorf("iterate tier candidate canonical groups: %w", err)
		}
		if err = canonicalRows.Close(); err != nil {
			return nil, false, fmt.Errorf("close tier candidate canonical groups: %w", err)
		}
		if count < personTierSignalPageSize {
			break
		}
	}

	// Only confirmed participant links define promotable candidate groups.
	membersByRoot := map[int64][]int64{}
	for participantID, root := range clusters {
		membersByRoot[root] = append(membersByRoot[root], participantID)
	}
	for root := range membersByRoot {
		slices.Sort(membersByRoot[root])
	}
	canonicalIDsByRoot := map[int64]map[string]struct{}{}
	for participantID, canonical := range canonicalByParticipant {
		root := participantID
		if linkedRoot, ok := clusters[participantID]; ok {
			root = linkedRoot
		}
		if canonicalIDsByRoot[root] == nil {
			canonicalIDsByRoot[root] = map[string]struct{}{}
		}
		canonicalIDsByRoot[root][canonical] = struct{}{}
	}
	canonicalByRoot := map[int64]string{}
	for root, ids := range canonicalIDsByRoot {
		if len(ids) == 1 {
			for canonical := range ids {
				canonicalByRoot[root] = canonical
			}
		}
	}

	boundRows, err := s.db.QueryContext(ctx, `SELECT participant_id FROM person_participants`)
	if err != nil {
		return nil, false, fmt.Errorf("list bound tier candidate participants: %w", err)
	}
	// A person binding on any member excludes the whole candidate group.
	boundRoots := map[int64]struct{}{}
	for boundRows.Next() {
		var participantID int64
		if err = boundRows.Scan(&participantID); err != nil {
			_ = boundRows.Close()
			return nil, false, fmt.Errorf("scan bound tier candidate participant: %w", err)
		}
		root := participantID
		if linkedRoot, ok := clusters[participantID]; ok {
			root = linkedRoot
		}
		boundRoots[root] = struct{}{}
	}
	if err = boundRows.Err(); err != nil {
		_ = boundRows.Close()
		return nil, false, fmt.Errorf("iterate bound tier candidate participants: %w", err)
	}
	if err = boundRows.Close(); err != nil {
		return nil, false, fmt.Errorf("close bound tier candidate participants: %w", err)
	}

	groups := make([]*tierCandidateGroup, 0, limit+1)
	participantCursor := afterID
	for len(groups) <= limit {
		rows, queryErr := s.db.QueryContext(ctx, `SELECT id FROM participants WHERE id>? ORDER BY id LIMIT ?`, participantCursor, personTierSignalPageSize)
		if queryErr != nil {
			return nil, false, fmt.Errorf("list tier candidate groups: %w", queryErr)
		}
		count := 0
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, false, fmt.Errorf("scan tier candidate group: %w", err)
			}
			count++
			participantCursor = id
			root := id
			if linkedRoot, ok := clusters[id]; ok {
				root = linkedRoot
			}
			if root != id {
				continue
			}
			if _, bound := boundRoots[root]; bound {
				continue
			}
			members := membersByRoot[root]
			if len(members) == 0 {
				members = []int64{id}
			}
			groups = append(groups, &tierCandidateGroup{
				id: root, canonical: canonicalByRoot[root], members: members, days: map[string]struct{}{},
			})
			if len(groups) > limit {
				break
			}
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("iterate tier candidate groups: %w", err)
		}
		if err = rows.Close(); err != nil {
			return nil, false, fmt.Errorf("close tier candidate groups: %w", err)
		}
		if len(groups) > limit || count < personTierSignalPageSize {
			break
		}
	}
	more := len(groups) > limit
	if more {
		groups = groups[:limit]
	}
	return groups, more, nil
}

func (s *Store) personTierCandidateMembersContext(ctx context.Context, groups []*tierCandidateGroup) (map[int64]*tierCandidateGroup, error) {
	representatives := map[int64]*tierCandidateGroup{}
	memberSet := map[int64]struct{}{}
	for _, group := range groups {
		for _, id := range group.members {
			representatives[id] = group
			memberSet[id] = struct{}{}
		}
	}
	memberIDs := make([]int64, 0, len(memberSet))
	for id := range memberSet {
		memberIDs = append(memberIDs, id)
	}
	slices.Sort(memberIDs)
	byParticipant := map[int64]*tierCandidateGroup{}
	for start := 0; start < len(memberIDs); start += personTierSignalPageSize {
		chunk := memberIDs[start:min(start+personTierSignalPageSize, len(memberIDs))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := s.db.QueryContext(ctx, `SELECT p.id,COALESCE(p.email_address,''),COALESCE(p.phone_number,''),
 COALESCE(pi.identifier_type,''),COALESCE(pi.identifier_value,''),`+personTierOwnerMatch("p.id", "")+`
	 FROM participants p LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE p.id IN (`+marks+`)
	 ORDER BY p.id,pi.identifier_type,pi.identifier_value`, args...)
		if err != nil {
			return nil, fmt.Errorf("read tier candidate members: %w", err)
		}
		for rows.Next() {
			var id int64
			var email, phone, kind, value string
			var owner bool
			if err = rows.Scan(&id, &email, &phone, &kind, &value, &owner); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan tier candidate member: %w", err)
			}
			group := representatives[id]
			if group == nil {
				continue
			}
			byParticipant[id] = group
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
			_ = rows.Close()
			return nil, fmt.Errorf("iterate tier candidate members: %w", err)
		}
		if err = rows.Close(); err != nil {
			return nil, fmt.Errorf("close tier candidate members: %w", err)
		}
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
	// Each member ID is bound twice below; 499 keeps this query below SQLite's
	// 999-variable limit while leaving the shared candidate page size at 500.
	for start := 0; start < len(members); start += personTierCandidateIdentityMatchChunkSize {
		chunk := members[start:min(start+personTierCandidateIdentityMatchChunkSize, len(members))]
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
