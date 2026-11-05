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
 ON b.page_number<=` + strconv.Itoa(q.Limit) + ` AND r.thread_source_id=b.source_id AND r.thread_conversation_id=b.conversation_id AND r.local_date=b.local_date
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

// hydrateActivityCounterpartsContext reads the same classified facts as totals.
func (s *Store) hydrateActivityCounterpartsContext(ctx context.Context, messages []activityReadMessage, f ActivityFilters) error {
	indices := make(map[int64]int, len(messages))
	for i := range messages {
		messages[i].Counterparts = []ActivityMessageCounterpart{}
		indices[messages[i].ID] = i
	}
	ids := make([]int64, 0, len(indices))
	for id := range indices {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += activityCandidateIDChunk {
		batch := ids[start:min(start+activityCandidateIDChunk, len(ids))]
		cte, args, err := s.activityReadCTE(ctx, activityReadWindow{MessageIDs: batch}, f)
		if err != nil {
			return err
		}
		listExcluded := "0"
		if f.Exclusions.ExcludeListMessages {
			listExcluded = `CASE WHEN f.list_id IS NOT NULL THEN 1 ELSE 0 END`
		}
		counted := CountedEventWhere(CountedEventInputs{Evidence: "f.evidence", Role: "f.role", Direction: "f.direction", RefKind: "f.ref_kind", ListExcluded: listExcluded, LargeInbound: "f.large_inbound"})
		rows, err := s.db.QueryContext(ctx, s.Rebind(cte+`, counterpart_flags AS (
 SELECT f.*,CASE WHEN f.contact_eligible=1 AND f.encrypted=0 AND f.excluded='' AND `+counted+` THEN 1 ELSE 0 END AS counterpart_counted
 FROM facts f WHERE f.participant_id IS NOT NULL
 ), ranked_counterparts AS (
 SELECT *,ROW_NUMBER() OVER (PARTITION BY message_id,participant_id ORDER BY counterpart_counted DESC,
 CASE role WHEN 'sender' THEN 0 WHEN 'organizer' THEN 1 WHEN 'addressed' THEN 2 WHEN 'attendee' THEN 3 WHEN 'member' THEN 4 ELSE 5 END) AS counterpart_rank
 FROM counterpart_flags
 ) SELECT f.message_id,p.id,COALESCE(f.person_id,0),COALESCE(p.display_name,''),
 COALESCE(NULLIF(p.email_address,''),(SELECT MIN(pi.identifier_value) FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='email'),p.phone_number,''),
 f.role,f.evidence,f.excluded,f.counterpart_counted
 FROM ranked_counterparts f JOIN participants p ON p.id=f.participant_id
 WHERE counterpart_rank=1 ORDER BY f.message_id,p.id`), args...)
		if err != nil {
			return fmt.Errorf("read activity counterparts: %w", err)
		}
		for rows.Next() {
			var id int64
			var cp ActivityMessageCounterpart
			var counted int
			if err = rows.Scan(&id, &cp.ParticipantID, &cp.PersonID, &cp.DisplayName, &cp.Address, &cp.Role, &cp.Evidence, &cp.Excluded, &counted); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan activity counterpart: %w", err)
			}
			cp.Counted = counted != 0
			i := indices[id]
			messages[i].Counterparts = append(messages[i].Counterparts, cp)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("iterate activity counterparts: %w", err)
		}
	}
	return nil
}
