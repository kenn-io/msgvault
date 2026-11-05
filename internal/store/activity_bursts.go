package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"time"
)

type BurstQuery struct {
	FromDate, ToDate string
	After, Before    *time.Time
	ConversationID   int64
	Subject          ActivitySubject
	Filters          ActivityFilters
	Descending       bool
	AfterKey         *BurstKey
	Limit            int
}
type BurstKey struct {
	FirstAt time.Time
	Key     string
}
type ActivityMessageCounterpart struct {
	ParticipantID, PersonID int64
	DisplayName, Address    string
	Role                    ActivityRole
	Evidence                ActivityEvidence
	Counted                 bool
	Excluded                string
}
type ActivityBurst struct {
	Key                      string
	SourceID, ConversationID int64
	Channel                  ActivityChannel
	LocalDate                string
	FirstAt, LastAt          time.Time
	Inbound, Outbound        int
	AnchorMessageID          int64
	LatestMessageIDs         []int64
	CopyCounts               map[int64]int
	Counterparts             []ActivityMessageCounterpart
	Cold                     bool
}
type BurstPage struct {
	Bursts       []ActivityBurst
	NoiseOmitted int
	NextKey      *BurstKey
}

type activityReadMessage struct {
	ID                                     int64
	Direction                              ActivityDirection
	Meeting, List, Encrypted, LargeInbound bool
	ContactEligible                        bool
	Counterparts                           []ActivityMessageCounterpart
}

func (s *Store) ListActivityBurstsContext(ctx context.Context, q BurstQuery) (BurstPage, error) {
	page := BurstPage{Bursts: []ActivityBurst{}}
	if q.Limit < 1 || q.Limit > 5000 {
		return page, fmt.Errorf("%w: burst limit must be 1..5000", ErrInvalidActivityRequest)
	}
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return page, err
	}
	cte, args, err := s.activityReadCTE(ctx, activityReadWindow{FromDate: q.FromDate, ToDate: q.ToDate, After: q.After, Before: q.Before, ConversationID: q.ConversationID, Subject: q.Subject}, filters)
	if err != nil {
		return page, err
	}
	order, compare := "ASC", ">"
	if q.Descending {
		order, compare = "DESC", "<"
	}
	where := "TRUE"
	if !filters.IncludeNoise {
		where = "noise=0"
	}
	if q.AfterKey != nil {
		where += ` AND (first_at ` + compare + ` ? OR (first_at=? AND key ` + compare + ` ?))`
		args = append(args, q.AfterKey.FirstAt.UTC(), q.AfterKey.FirstAt.UTC(), q.AfterKey.Key)
	}
	args = append(args, q.Limit+1)
	query := cte + activityBurstGroupsCTE + `, selected AS (
 SELECT *,ROW_NUMBER() OVER(ORDER BY first_at ` + order + `,key ` + order + `) AS page_number FROM burst_groups WHERE ` + where + ` ORDER BY first_at ` + order + `,key ` + order + ` LIMIT ?
 ), omitted AS (SELECT COALESCE(SUM(message_count),0) AS n FROM burst_groups WHERE noise=1)
 SELECT omitted.n,b.key,b.source_id,b.conversation_id,b.channel,b.local_date,b.first_at,b.last_at,b.inbound,b.outbound,b.page_number,
 r.message_id,r.direction,r.message_type,r.list_id,r.encrypted,r.large_inbound,r.contact_eligible,r.copy_count
 FROM omitted LEFT JOIN selected b ON TRUE LEFT JOIN representatives r
 ON b.page_number<=` + strconv.Itoa(q.Limit) + ` AND r.source_id=b.source_id AND COALESCE(r.conversation_id,0)=b.conversation_id AND r.local_date=b.local_date
 ORDER BY b.first_at ` + order + `,b.key ` + order + `,r.occurred_at DESC,r.message_id DESC`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return page, fmt.Errorf("read activity bursts: %w", err)
	}
	messages := []activityReadMessage{}
	indices := map[int64]int{}
	lastKey := ""
	hasMore := false
	for rows.Next() {
		var b ActivityBurst
		var key, channel, date, direction, kind, list sql.NullString
		var source, conv, in, out, number, id, encrypted, large, eligible, copies sql.NullInt64
		var first, last nullableTimestamp
		if err = rows.Scan(&page.NoiseOmitted, &key, &source, &conv, &channel, &date, &first, &last, &in, &out, &number, &id, &direction, &kind, &list, &encrypted, &large, &eligible, &copies); err != nil {
			_ = rows.Close()
			return page, fmt.Errorf("scan activity burst: %w", err)
		}
		if !key.Valid {
			continue
		}
		if number.Int64 > int64(q.Limit) {
			hasMore = true
			continue
		}
		if key.String != lastKey {
			b = ActivityBurst{Key: key.String, SourceID: source.Int64, ConversationID: conv.Int64, Channel: ActivityChannel(channel.String), LocalDate: date.String, FirstAt: first.Time, LastAt: last.Time, Inbound: int(in.Int64), Outbound: int(out.Int64), LatestMessageIDs: []int64{}, CopyCounts: map[int64]int{}, Counterparts: []ActivityMessageCounterpart{}}
			page.Bursts = append(page.Bursts, b)
			lastKey = key.String
		}
		if !id.Valid {
			continue
		}
		index := len(page.Bursts) - 1
		burst := &page.Bursts[index]
		if burst.AnchorMessageID == 0 {
			burst.AnchorMessageID = id.Int64
		}
		if len(burst.LatestMessageIDs) < 5 {
			burst.LatestMessageIDs = append(burst.LatestMessageIDs, id.Int64)
		}
		if burst.Channel == ChannelEmail {
			burst.CopyCounts[id.Int64] = int(copies.Int64)
		}
		messages = append(messages, activityReadMessage{ID: id.Int64, Direction: ActivityDirection(direction.String), Meeting: IsMeetingMessageType(kind.String), List: list.Valid, Encrypted: encrypted.Int64 != 0, LargeInbound: large.Int64 != 0, ContactEligible: eligible.Int64 != 0})
		indices[id.Int64] = index
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return page, fmt.Errorf("iterate activity bursts: %w", err)
	}
	if filters.IncludeNoise {
		page.NoiseOmitted = 0
	}
	if err = s.hydrateActivityCounterpartsContext(ctx, messages, filters); err != nil {
		return page, err
	}
	distinct := make([]map[int64]ActivityMessageCounterpart, len(page.Bursts))
	for i := range distinct {
		distinct[i] = map[int64]ActivityMessageCounterpart{}
	}
	for _, m := range messages {
		for _, cp := range m.Counterparts {
			old, found := distinct[indices[m.ID]][cp.ParticipantID]
			if !found || (!old.Counted && cp.Counted) {
				distinct[indices[m.ID]][cp.ParticipantID] = cp
			}
		}
	}
	for i := range page.Bursts {
		for _, cp := range distinct[i] {
			page.Bursts[i].Counterparts = append(page.Bursts[i].Counterparts, cp)
		}
		slices.SortFunc(page.Bursts[i].Counterparts, func(a, b ActivityMessageCounterpart) int {
			if a.ParticipantID < b.ParticipantID {
				return -1
			}
			if a.ParticipantID > b.ParticipantID {
				return 1
			}
			return 0
		})
	}
	if hasMore && len(page.Bursts) > 0 {
		last := page.Bursts[len(page.Bursts)-1]
		page.NextKey = &BurstKey{FirstAt: last.FirstAt, Key: last.Key}
	}
	return page, nil
}

// hydrateActivityCounterpartsContext uses the projector's population and the
// SQL readers' address exclusions.
func (s *Store) hydrateActivityCounterpartsContext(ctx context.Context, messages []activityReadMessage, f ActivityFilters) error {
	ids := make([]int64, len(messages))
	for i := range messages {
		ids[i] = messages[i].ID
	}
	population, err := s.loadActivityCounterpartsContext(ctx, s.db, ids)
	if err != nil {
		return err
	}
	type participant struct {
		name, address string
		excluded      string
	}
	participants := map[int64]participant{}
	wanted := map[int64]bool{}
	for _, list := range population {
		for _, cp := range list {
			wanted[cp.ParticipantID] = true
		}
	}
	participantIDs := make([]int64, 0, len(wanted))
	for id := range wanted {
		participantIDs = append(participantIDs, id)
	}
	for start := 0; start < len(participantIDs); start += activityCandidateIDChunk {
		batch := participantIDs[start:min(start+activityCandidateIDChunk, len(participantIDs))]
		predicate, args := messageExportInClause("p.id", batch)
		exclusionCTE, exclusionArgs := s.activityAddressExclusionsCTE(`SELECT p.id AS participant_id,pp.person_id FROM participants p LEFT JOIN person_participants pp ON pp.participant_id=p.id WHERE `+predicate, f)
		args = append(args, exclusionArgs...)
		rows, err := s.db.QueryContext(ctx, s.Rebind(`WITH `+exclusionCTE+` SELECT p.id,COALESCE(p.display_name,''),COALESCE(NULLIF(p.email_address,''),(SELECT MIN(pi.identifier_value) FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='email'),p.phone_number,''),ax.excluded
 FROM participants p JOIN address_exclusions ax ON ax.participant_id=p.id`), args...)
		if err != nil {
			return fmt.Errorf("read activity participant display: %w", err)
		}
		for rows.Next() {
			var id int64
			var p participant
			if err = rows.Scan(&id, &p.name, &p.address, &p.excluded); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan activity participant: %w", err)
			}
			participants[id] = p
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("iterate activity participants: %w", err)
		}
	}
	maxDirect := f.directLimit()
	for i := range messages {
		m := &messages[i]
		raw := collapseActivityCounterparts(population[m.ID])
		owner := activityOwnerLinked(raw)
		audience := map[string]bool{}
		for _, cp := range raw {
			if owner(cp) {
				continue
			}
			key := fmt.Sprint("participant:", cp.ParticipantID)
			if cp.PersonID != nil {
				key = fmt.Sprint("person:", *cp.PersonID)
			}
			audience[key] = true
		}
		// Match the projector's evidence-first, then role arbitration. Saved
		// aliases share one person link; unsaved rows share a participant link.
		type linkKey struct {
			person bool
			id     int64
		}
		keyFor := func(cp ActivityCounterpart) linkKey {
			if cp.PersonID != nil {
				return linkKey{person: true, id: *cp.PersonID}
			}
			return linkKey{id: cp.ParticipantID}
		}
		strongest := map[linkKey]ActivityEventPerson{}
		for _, cp := range raw {
			link := ActivityEventPerson{
				Role:     classifiedActivityRole(cp.RecipientType, cp.RecipientType == meetingSenderRole, m.Meeting),
				Evidence: classifiedActivityEvidence(m.Direction, cp.RecipientType == meetingSenderRole, len(audience) > maxDirect),
			}
			key := keyFor(cp)
			if old, found := strongest[key]; !found || strongerClassifiedActivityLink(link, old) {
				strongest[key] = link
			}
		}
		byParticipant := map[int64]ActivityMessageCounterpart{}
		for _, cp := range raw {
			p := participants[cp.ParticipantID]
			link := strongest[keyFor(cp)]
			role, evidence := link.Role, link.Evidence
			var excluded string
			if owner(cp) {
				excluded = "owner"
			} else {
				excluded = p.excluded
			}
			refKind := "message"
			if m.Meeting {
				refKind = "meeting"
			}
			c := ActivityMessageCounterpart{ParticipantID: cp.ParticipantID, DisplayName: p.name, Address: p.address, Role: role, Evidence: evidence, Excluded: excluded, Counted: m.ContactEligible && excluded == "" && !m.Encrypted && CountedEvent(evidence, role, m.Direction, refKind, m.List && f.Exclusions.ExcludeListMessages, m.LargeInbound)}
			if cp.PersonID != nil {
				c.PersonID = *cp.PersonID
			}
			old, found := byParticipant[cp.ParticipantID]
			if !found || (!old.Counted && c.Counted) || (old.Counted == c.Counted && classifiedActivityRolePriority(c.Role) < classifiedActivityRolePriority(old.Role)) {
				byParticipant[cp.ParticipantID] = c
			}
		}
		m.Counterparts = []ActivityMessageCounterpart{}
		for _, c := range byParticipant {
			m.Counterparts = append(m.Counterparts, c)
		}
		slices.SortFunc(m.Counterparts, func(a, b ActivityMessageCounterpart) int {
			if a.ParticipantID < b.ParticipantID {
				return -1
			}
			if a.ParticipantID > b.ParticipantID {
				return 1
			}
			return 0
		})
	}
	return nil
}
