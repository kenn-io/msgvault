package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/activitydate"
)

type coldActivityMessage struct {
	activityReadMessage

	candidate ActivityCandidate
	dating    activitydate.Dating
	channel   ActivityChannel
}

type coldActivityGroup struct {
	burst    ActivityBurst
	messages []coldActivityMessage
	nonNoise bool
}

// ListColdActivityBurstsContext reconstructs complete cold conversation-days
// from live message metadata, without restoring or writing projection rows.
func (s *Store) ListColdActivityBurstsContext(ctx context.Context, q BurstQuery) (BurstPage, error) {
	page := BurstPage{Bursts: []ActivityBurst{}}
	if q.Limit < 1 || q.Limit > 5000 {
		return page, fmt.Errorf("%w: burst limit must be 1..5000", ErrInvalidActivityRequest)
	}
	groups, filters, err := s.coldActivityGroupsContext(ctx, q)
	if err != nil {
		return page, err
	}
	admitted := make([]ActivityBurst, 0, len(groups))
	for _, g := range groups {
		if !filters.IncludeNoise && !g.nonNoise {
			page.NoiseOmitted += len(g.messages)
			continue
		}
		b := g.burst
		if q.AfterKey != nil {
			comparison := compareColdBurstKey(BurstKey{FirstAt: b.FirstAt, Key: b.Key}, *q.AfterKey)
			if (!q.Descending && comparison <= 0) || (q.Descending && comparison >= 0) {
				continue
			}
		}
		admitted = append(admitted, b)
	}
	slices.SortFunc(admitted, func(a, b ActivityBurst) int {
		n := compareColdBurstKey(BurstKey{FirstAt: a.FirstAt, Key: a.Key}, BurstKey{FirstAt: b.FirstAt, Key: b.Key})
		if q.Descending {
			return -n
		}
		return n
	})
	if len(admitted) > q.Limit {
		last := admitted[q.Limit-1]
		page.NextKey = &BurstKey{FirstAt: last.FirstAt, Key: last.Key}
		admitted = admitted[:q.Limit]
	}
	page.Bursts = admitted
	return page, nil
}

// CountColdActivityMessagesContext counts messages in admitted
// logical groups across the whole window. Burst limits and cursors do not
// narrow omissions; request eligibility, scope, subject and noise filters do.
func (s *Store) CountColdActivityMessagesContext(ctx context.Context, q BurstQuery) (int, error) {
	groups, filters, err := s.coldActivityGroupsContext(ctx, q)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, g := range groups {
		if filters.IncludeNoise || g.nonNoise {
			count += len(g.messages)
		}
	}
	return count, nil
}

func compareColdBurstKey(a, b BurstKey) int {
	if n := a.FirstAt.Compare(b.FirstAt); n != 0 {
		return n
	}
	return cmp.Compare(a.Key, b.Key)
}

func (s *Store) coldActivityGroupsContext(ctx context.Context, q BurstQuery) (map[string]*coldActivityGroup, ActivityFilters, error) {
	filters, err := s.normalizedActivityFilters(ctx, q.Filters)
	if err != nil {
		return nil, filters, err
	}
	if q.ConversationID < 0 || (q.After != nil && q.Before != nil && !q.After.Before(*q.Before)) {
		return nil, filters, fmt.Errorf("%w: invalid activity window", ErrInvalidActivityRequest)
	}
	zone, err := s.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return nil, filters, err
	}
	loc, err := activitydate.LoadZone(zone.Target)
	if err != nil {
		return nil, filters, err
	}
	where := `m.conversation_id IN (SELECT p.conversation_id FROM conversation_policy p JOIN conversations c ON c.id=p.conversation_id WHERE p.effective_tier='cold' AND c.conversation_type IN ` + chatConversationTypesSQL + `)`
	scope, args := sourceScopeClause(ctx, "m.source_id")
	where += scope
	if q.ConversationID > 0 {
		where += ` AND m.conversation_id=?`
		args = append(args, q.ConversationID)
	}
	var lower, upper *time.Time
	if q.After != nil {
		lower = new(q.After.UTC().Add(-24 * time.Hour))
	}
	if q.Before != nil {
		upper = new(q.Before.UTC().Add(24 * time.Hour))
	}
	if q.FromDate != "" || q.ToDate != "" {
		if !validActivityDate(q.FromDate) || !validActivityDate(q.ToDate) || q.FromDate > q.ToDate {
			return nil, filters, fmt.Errorf("%w: invalid local dates", ErrInvalidActivityRequest)
		}
		// A bounded SQL superset covers every IANA offset and explicit day precision.
		// DateEvent below applies the exact archive-zone civil-date boundary.
		from, _ := time.Parse(time.DateOnly, q.FromDate)
		to, _ := time.Parse(time.DateOnly, q.ToDate)
		dateLower, dateUpper := from.Add(-24*time.Hour), to.Add(48*time.Hour)
		if lower == nil || dateLower.After(*lower) {
			lower = &dateLower
		}
		if upper == nil || dateUpper.Before(*upper) {
			upper = &dateUpper
		}
	}
	var timestampWindows []string
	for _, column := range []string{"m.sent_at", "m.received_at", "m.internal_date"} {
		var bounds []string
		if lower != nil {
			bounds = append(bounds, column+">=?")
			args = append(args, *lower)
		}
		if upper != nil {
			bounds = append(bounds, column+"<?")
			args = append(args, *upper)
		}
		if len(bounds) > 0 {
			timestampWindows = append(timestampWindows, "("+strings.Join(bounds, " AND ")+")")
		}
	}
	if len(timestampWindows) > 0 {
		where += " AND (" + strings.Join(timestampWindows, " OR ") + ")"
	}
	subject, subjectArgs, err := activitySubjectClause(q.Subject, "m")
	if err != nil {
		return nil, filters, err
	}
	// Materialize the scoped cold window before live eligibility and counterpart
	// classification. Otherwise SQLite can evaluate those correlated queries for
	// every archived message, even when no cold conversation matches.
	args = append(args, subjectArgs...)
	rows, err := s.db.QueryContext(ctx, s.Rebind(`WITH cold_window AS MATERIALIZED (
 SELECT m.id FROM messages m WHERE `+where+`
 ) SELECT m.id,m.list_id IS NOT NULL,
 `+matrixPendingEncryptedSQL+`
 FROM cold_window cw JOIN messages m ON m.id=cw.id JOIN sources src ON src.id=m.source_id
 WHERE `+s.activityLiveEligibleSQL()+subject+` ORDER BY m.source_id,m.id`), args...)
	if err != nil {
		return nil, filters, fmt.Errorf("read cold activity: %w", err)
	}
	messages := []coldActivityMessage{}
	ids := []int64{}
	for rows.Next() {
		var m coldActivityMessage
		if err = rows.Scan(&m.ID, &m.List, &m.Encrypted); err != nil {
			_ = rows.Close()
			return nil, filters, fmt.Errorf("scan cold activity: %w", err)
		}
		messages = append(messages, m)
		ids = append(ids, m.ID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, filters, fmt.Errorf("iterate cold activity: %w", err)
	}
	candidates, err := s.LoadActivityCandidatesByIDContext(ctx, ids)
	if err != nil {
		return nil, filters, err
	}
	byID := make(map[int64]ActivityCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.MessageID] = c
	}
	classifiedMessages := []coldActivityMessage{}
	for _, m := range messages {
		if err = ctx.Err(); err != nil {
			return nil, filters, err
		}
		c := byID[m.ID]
		dating, dateErr := activitydate.DateEvent(activitydate.Timestamps{SentAt: c.SentAt, ReceivedAt: c.ReceivedAt, InternalDate: c.InternalDate, DateOnly: c.DateOnly}, loc)
		if dateErr != nil {
			continue
		}
		// SQL bounds include a day of padding for timestamps stored with offsets.
		// Apply the exact half-open window to the dated instant.
		if (q.After != nil && dating.OccurredAt.Before(*q.After)) ||
			(q.Before != nil && !dating.OccurredAt.Before(*q.Before)) {
			continue
		}
		if (q.FromDate != "" && dating.LocalDate < q.FromDate) || (q.ToDate != "" && dating.LocalDate > q.ToDate) {
			continue
		}
		classified := ClassifyActivityCandidate(c, filters.directLimit())
		m.candidate = c
		m.dating = dating
		m.channel = classified.Channel
		m.Direction = classified.Direction
		m.Meeting = IsMeetingMessageType(c.MessageType)
		m.ContactEligible = m.Meeting || countedMessageType(c.MessageType, c.ConversationType)
		m.LargeInbound = m.Direction == DirectionInbound && classified.Broadcast
		classifiedMessages = append(classifiedMessages, m)
	}
	facts := make([]activityReadMessage, len(classifiedMessages))
	for i, m := range classifiedMessages {
		facts[i] = m.activityReadMessage
	}
	if err = s.hydrateColdActivityCounterpartsContext(ctx, facts, filters); err != nil {
		return nil, filters, err
	}
	groups := map[string]*coldActivityGroup{}
	for i, m := range classifiedMessages {
		m.activityReadMessage = facts[i]
		conv := int64(0)
		if m.candidate.ConversationID != nil {
			conv = *m.candidate.ConversationID
		}
		key := fmt.Sprintf("burst:%d:%d:%s", m.candidate.SourceID, conv, m.dating.LocalDate)
		g := groups[key]
		if g == nil {
			g = &coldActivityGroup{burst: ActivityBurst{Key: key, SourceID: m.candidate.SourceID, ConversationID: conv, Channel: m.channel, LocalDate: m.dating.LocalDate, FirstAt: m.dating.OccurredAt, LastAt: m.dating.OccurredAt, Cold: true, LatestMessageIDs: []int64{}, CopyCounts: map[int64]int{}, Counterparts: []ActivityMessageCounterpart{}}}
			groups[key] = g
		}
		if m.dating.OccurredAt.Before(g.burst.FirstAt) {
			g.burst.FirstAt = m.dating.OccurredAt
		}
		if m.dating.OccurredAt.After(g.burst.LastAt) {
			g.burst.LastAt = m.dating.OccurredAt
		}
		if m.channel < g.burst.Channel {
			g.burst.Channel = m.channel
		}
		counted := coldActivityCounted(m, q.Subject)
		if counted {
			if m.Direction == DirectionInbound {
				g.burst.Inbound++
			}
			if m.Direction == DirectionOutbound {
				g.burst.Outbound++
			}
		}
		g.nonNoise = g.nonNoise || coldActivityNonNoise(m, filters)
		g.messages = append(g.messages, m)
	}
	for _, g := range groups {
		finishColdActivityGroup(g)
	}
	return groups, filters, nil
}

// Cold messages have no projection rows. Supply their live classification to
// the shared counterpart query instead of reading eligible activity_events.
func (s *Store) hydrateColdActivityCounterpartsContext(ctx context.Context, messages []activityReadMessage, filters ActivityFilters) error {
	// Recompute roles and evidence at the effective limit, even while stale
	// projection rows await retraction.
	filters.MaxDirect = filters.directLimit()
	byID := make(map[int64]activityReadMessage, len(messages))
	for _, m := range messages {
		byID[m.ID] = m
	}
	return s.hydrateActivityCounterpartsWithCTEContext(ctx, messages, filters, func(ids []int64) (string, []any, error) {
		values := make([]string, 0, len(ids))
		args := make([]any, 0, len(ids)*5)
		for _, id := range ids {
			m := byID[id]
			refKind := RefKindMessage
			if m.Meeting {
				refKind = RefKindMeeting
			}
			eligible, encrypted := 0, 0
			if m.ContactEligible {
				eligible = 1
			}
			if m.Encrypted {
				encrypted = 1
			}
			values = append(values, `(CAST(? AS BIGINT),CAST(? AS TEXT),CAST(? AS TEXT),CAST(? AS INTEGER),CAST(? AS INTEGER))`)
			args = append(args, id, m.Direction, refKind, eligible, encrypted)
		}
		classification, addressArgs := s.activityClassificationCTE(ctx, filters)
		return `WITH RECURSIVE cold_facts(message_id,direction,ref_kind,contact_eligible,encrypted) AS (VALUES ` + strings.Join(values, ",") + `), eligible AS (
 SELECT f.*,m.list_id FROM cold_facts f JOIN messages m ON m.id=f.message_id
 ), ` + classification, append(args, addressArgs...), nil
	})
}

func coldActivityNonNoise(m coldActivityMessage, filters ActivityFilters) bool {
	// Recipient aliases inherit the strongest contact role; noise follows
	// original sender membership, as in the warm SQL.
	return (!m.List || !filters.Exclusions.ExcludeListMessages) && !m.ExcludedSender
}

// Hydrated counterparts already apply strongest-link arbitration and live
// counting policy. Match the spine's counted fact for the requested subject.
func coldActivityCounted(m coldActivityMessage, subject ActivitySubject) bool {
	for _, cp := range m.Counterparts {
		if !cp.Counted {
			continue
		}
		if subject.PersonID > 0 && cp.PersonID != subject.PersonID {
			continue
		}
		if len(subject.ParticipantIDs) > 0 && !slices.Contains(subject.ParticipantIDs, cp.ParticipantID) {
			continue
		}
		return true
	}
	return false
}

func finishColdActivityGroup(g *coldActivityGroup) {
	slices.SortFunc(g.messages, func(a, b coldActivityMessage) int {
		if n := b.dating.OccurredAt.Compare(a.dating.OccurredAt); n != 0 {
			return n
		}
		return cmp.Compare(b.ID, a.ID)
	})
	counterparts := map[int64]ActivityMessageCounterpart{}
	for _, m := range g.messages {
		if g.burst.AnchorMessageID == 0 {
			g.burst.AnchorMessageID = m.ID
		}
		if len(g.burst.LatestMessageIDs) < 5 {
			g.burst.LatestMessageIDs = append(g.burst.LatestMessageIDs, m.ID)
		}
		for _, cp := range m.Counterparts {
			old, found := counterparts[cp.ParticipantID]
			if !found || (!old.Counted && cp.Counted) {
				counterparts[cp.ParticipantID] = cp
			}
		}
	}
	for _, cp := range counterparts {
		g.burst.Counterparts = append(g.burst.Counterparts, cp)
	}
	slices.SortFunc(g.burst.Counterparts, func(a, b ActivityMessageCounterpart) int { return cmp.Compare(a.ParticipantID, b.ParticipantID) })
}
