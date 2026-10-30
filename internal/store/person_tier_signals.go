package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/persontiers"
)

const personTierPageSize = 500

// personTierRowPage is how many events one read holds at a time.
var personTierRowPage = 5000

// PersonTierReadiness reports whether the activity projection has caught up
// with the archive and was built under the configured settings. Counts read
// while it is behind can be too low or follow older settings, so previews warn
// instead of refusing.
type PersonTierReadiness struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons"`
}

// personTierReadiness lists why the activity projection is behind, or returns
// Ready when it is not. Queue, transition, and identity-epoch freshness is the
// same barrier contact state uses; the watermark check adds messages the
// projection has not queued yet.
func (s *Store) personTierReadiness(ctx context.Context, tx *loggedTx) (PersonTierReadiness, error) {
	result := PersonTierReadiness{Reasons: []string{}}
	_, stale, err := s.contactStateFreshnessTxContext(ctx, tx)
	if err != nil {
		return result, err
	}
	if stale {
		result.Reasons = append(result.Reasons, "projection_pending")
	}
	watermark, err := activityWatermark(ctx, tx)
	if err != nil {
		return result, err
	}
	var liveMax int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM messages WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL`).Scan(&liveMax); err != nil {
		return result, fmt.Errorf("read live messages for tier readiness: %w", err)
	}
	if watermark < liveMax {
		result.Reasons = append(result.Reasons, "watermark_behind")
	}
	result.Ready = len(result.Reasons) == 0
	return result, nil
}

// personTierSignals reads the windowed counts for one page of saved people.
// asOf is already in the projection timezone and maxDirect is the limit the
// projection was built under; windows include both boundary dates. The
// projection holds each person's direct events; the inbound audience cut comes
// from the projector's own classifier. Events arrive in keyset pages and fold
// into the counters, so memory follows the page size and the number of people
// and days, not the number of events.
func (s *Store) personTierSignals(ctx context.Context, tx *loggedTx, personIDs []int64, asOf time.Time, cfg persontiers.Config, maxDirect int) (map[int64]persontiers.Signals, error) {
	tally := newTierTally(cfg, asOf)
	var after tierSignalRow
	for {
		rows, err := s.personTierRows(ctx, tx, personIDs, asOf, cfg, after)
		if err != nil {
			return nil, err
		}
		broadcast, err := s.personTierBroadcast(ctx, tx, rows, cfg, maxDirect)
		if err != nil {
			return nil, err
		}
		tally.add(rows, broadcast)
		if len(rows) < personTierRowPage {
			break
		}
		after = rows[len(rows)-1]
	}
	result := tally.result
	for _, id := range personIDs {
		if _, ok := result[id]; !ok {
			result[id] = persontiers.Signals{}
		}
	}
	if err := personTierBindings(ctx, tx, int64Placeholders(len(personIDs)), personMergeSnapshotIDArgs(personIDs), result); err != nil {
		return nil, err
	}
	return result, nil
}

// tierSignalRow is one direct activity link the tier rules can count.
type tierSignalRow struct {
	PersonID  int64
	MessageID int64
	LocalDate string
	Channel   string
	Inbound   bool
	Meeting   bool
	List      bool
}

// personTierRows reads up to personTierRowPage direct links in the long window,
// in (person, date, message) order after the given row. The WHERE clause is the
// counting rule: sent email and chat messages between the person and the owner,
// not drafts, unsent texts, or call records, and meetings in either direction.
// Unsent uses the projector's own predicate, so a sent copy that kept draft
// evidence after dedup still counts.
func (s *Store) personTierRows(ctx context.Context, tx *loggedTx, personIDs []int64, asOf time.Time, cfg persontiers.Config, after tierSignalRow) ([]tierSignalRow, error) {
	args := personMergeSnapshotIDArgs(personIDs)
	args = append(args,
		asOf.AddDate(0, 0, -(cfg.LongWindowDays-1)).Format(time.DateOnly),
		asOf.Format(time.DateOnly),
		after.PersonID, after.LocalDate, after.MessageID, personTierRowPage)
	rows, err := tx.QueryContext(ctx, `SELECT aep.person_id,aep.message_id,aep.local_date,ae.channel,
 CASE WHEN ae.direction='inbound' THEN 1 ELSE 0 END,
 CASE WHEN ae.ref_kind='meeting' THEN 1 ELSE 0 END,
 CASE WHEN m.list_id IS NULL THEN 0 ELSE 1 END
 FROM activity_event_persons aep
 JOIN activity_events ae ON ae.message_id=aep.message_id
 JOIN messages m ON m.id=ae.message_id
 JOIN conversations c ON c.id=m.conversation_id
 JOIN sources src ON src.id=m.source_id
 WHERE aep.person_id IN (`+int64Placeholders(len(personIDs))+`) AND aep.evidence='direct'
 AND aep.local_date BETWEEN ? AND ?
 AND (aep.person_id,aep.local_date,aep.message_id) > (?,?,?)
 AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND (
 (ae.ref_kind='meeting' AND aep.role IN ('attendee','organizer')) OR
 (ae.ref_kind='message' AND ((ae.direction='inbound' AND aep.role='sender') OR
 (ae.direction='outbound' AND aep.role IN ('addressed','member')))
 AND (COALESCE(m.message_type,'') IN ('','`+MessageTypeEmail+`') OR `+identityindex.IsChatSQL("m.message_type", "c.conversation_type")+`)
 AND NOT `+s.activityUnsentSQL()+`))
 ORDER BY aep.person_id,aep.local_date,aep.message_id LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read person tier signals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]tierSignalRow, 0, personTierRowPage)
	for rows.Next() {
		var r tierSignalRow
		var inbound, meeting, list int
		if err = rows.Scan(&r.PersonID, &r.MessageID, &r.LocalDate, &r.Channel, &inbound, &meeting, &list); err != nil {
			return nil, fmt.Errorf("scan person tier signals: %w", err)
		}
		r.Inbound, r.Meeting, r.List = inbound != 0, meeting != 0, list != 0
		result = append(result, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person tier signals: %w", err)
	}
	return result, nil
}

// personTierBroadcast reports, per inbound message, whether the projector's
// classifier treats its audience as above the direct limit. It skips list mail
// the rules drop anyway. Candidates load in batches and reduce to one bool per
// message, so memory follows the batch rather than the year's mail.
func (s *Store) personTierBroadcast(ctx context.Context, tx *loggedTx, rows []tierSignalRow, cfg persontiers.Config, maxDirect int) (map[int64]bool, error) {
	broadcast := map[int64]bool{}
	var ids []int64
	for _, r := range rows {
		if _, seen := broadcast[r.MessageID]; r.Inbound && !seen && (!r.List || !cfg.ExcludeListMessages) {
			broadcast[r.MessageID] = false
			ids = append(ids, r.MessageID)
		}
	}
	for start := 0; start < len(ids); start += activityCandidateIDChunk {
		batch := ids[start:min(start+activityCandidateIDChunk, len(ids))]
		candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, batch)
		if err != nil {
			return nil, fmt.Errorf("load tier audiences: %w", err)
		}
		for _, c := range candidates {
			broadcast[c.MessageID] = ClassifyActivityCandidate(c, maxDirect).Broadcast
		}
	}
	return broadcast, nil
}

// tierTally counts rows as pages arrive. A row counts unless the message came
// from a mailing list (when excluded) or is an inbound message to more people
// than the direct limit; an excluded row takes the first reason that applies.
// A meeting adds to the active and meeting days but is not a message channel.
type tierTally struct {
	cfg    persontiers.Config
	short  string
	result map[int64]persontiers.Signals
	seen   map[int64]*tierDays
}

type tierDays struct{ short, long, meeting, channel map[string]struct{} }

func newTierTally(cfg persontiers.Config, asOf time.Time) *tierTally {
	return &tierTally{
		cfg:    cfg,
		short:  asOf.AddDate(0, 0, -(cfg.ShortWindowDays - 1)).Format(time.DateOnly),
		result: map[int64]persontiers.Signals{},
		seen:   map[int64]*tierDays{},
	}
}

func (t *tierTally) add(rows []tierSignalRow, broadcast map[int64]bool) {
	for _, r := range rows {
		s := t.result[r.PersonID]
		switch {
		case r.List && t.cfg.ExcludeListMessages:
			s.ExcludedListEvents++
			t.result[r.PersonID] = s
			continue
		case r.Inbound && broadcast[r.MessageID]:
			s.LargeAudienceInboundEvents++
			t.result[r.PersonID] = s
			continue
		}
		d := t.seen[r.PersonID]
		if d == nil {
			d = &tierDays{map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}}
			t.seen[r.PersonID] = d
		}
		inShort := r.LocalDate >= t.short
		d.long[r.LocalDate] = struct{}{}
		if !r.Meeting {
			d.channel[r.Channel] = struct{}{}
		}
		s.LastDate = max(s.LastDate, r.LocalDate)
		if inShort {
			d.short[r.LocalDate] = struct{}{}
		}
		switch {
		case r.Meeting:
			d.meeting[r.LocalDate] = struct{}{}
		case r.Inbound:
			s.InboundLong++
			if inShort {
				s.InboundShort++
			}
		default:
			s.OutboundLong++
			if inShort {
				s.OutboundShort++
			}
		}
		s.ActiveDaysShort, s.ActiveDaysLong = len(d.short), len(d.long)
		s.MeetingDaysLong, s.ChannelsLong = len(d.meeting), len(d.channel)
		t.result[r.PersonID] = s
	}
}

// personTierBindings reads each person's addresses and identifiers for
// the exclusions, and whether any of their participants is one of the owner's
// own addresses in any account. The projection drops owner-linked participants
// only within the message's own account, so this is the stricter cross-account
// check. Primary email is authoritative over email aliases, and native IDs are
// exact.
func personTierBindings(ctx context.Context, tx *loggedTx, marks string, args []any, signals map[int64]persontiers.Signals) error {
	rows, err := tx.QueryContext(ctx, `SELECT pp.person_id,COALESCE(p.email_address,''),COALESCE(p.phone_number,''),
 COALESCE(pi.identifier_type,''),COALESCE(pi.identifier_value,''),
 EXISTS (SELECT 1 FROM account_identities ai WHERE (`+ownerEmailMatch("p.email_address", "ai.address")+`) OR EXISTS (
 SELECT 1 FROM participant_identifiers oi WHERE oi.participant_id=p.id AND `+
		ownerIdentifierMatch("oi.identifier_type", "oi.identifier_value", "ai.address", " AND (p.email_address IS NULL OR TRIM(p.email_address)='')")+`))
 FROM person_participants pp JOIN participants p ON p.id=pp.participant_id
 LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE pp.person_id IN (`+marks+`)
 ORDER BY pp.person_id,p.id,pi.identifier_type,pi.identifier_value`, args...)
	if err != nil {
		return fmt.Errorf("read person tier bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var email, phone, kind, value string
		var owner bool
		if err = rows.Scan(&id, &email, &phone, &kind, &value, &owner); err != nil {
			return fmt.Errorf("scan person tier bindings: %w", err)
		}
		signal := signals[id]
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
		signals[id] = signal
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterate person tier bindings: %w", err)
	}
	return nil
}
