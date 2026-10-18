package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

const meetingRecordMaxBytes = 1 << 20
const meetingRecordMaxItems = 10
const meetingRecordFullItems = 3
const meetingRecordFullBytes = 131072
const meetingRecordSummaryBytes = 16384
const meetingRecordMaxPeople = 500
const meetingRecordMaxRecordings = 100

type MeetingParticipant struct {
	Name          string `json:"name,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	ParticipantID int64  `json:"participant_id,omitempty"`
	Provenance    string `json:"provenance"`
}
type MeetingRecording struct {
	AttachmentID    int64    `json:"attachment_id"`
	MimeType        string   `json:"mime_type"`
	Size            int64    `json:"size"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}
type MeetingItem struct {
	MessageID         int64                  `json:"message_id"`
	SourceID          int64                  `json:"source_id"`
	Title             string                 `json:"title"`
	SentAt            time.Time              `json:"sent_at"`
	Basis             string                 `json:"basis"`
	Origin            string                 `json:"origin"`
	Content           *meetingcontent.Packet `json:"content,omitempty"`
	ContentOmitted    bool                   `json:"content_omitted"`
	DeletedFromSource bool                   `json:"deleted_from_source"`
	Recordings        []MeetingRecording     `json:"recordings"`
	FollowUp          string                 `json:"follow_up"`
}
type MeetingPrevious struct {
	Ref         string                     `json:"ref"`
	Start       time.Time                  `json:"start"`
	Item        MeetingItem                `json:"item"`
	OpenActions []meetingcontent.ActionRow `json:"open_actions"`
}
type MeetingEventDetail struct {
	Location             string `json:"location"`
	Description          string `json:"description"`
	DescriptionTruncated bool   `json:"description_truncated"`
}
type MeetingRecord struct {
	Ref          string                     `json:"ref"`
	Occurrence   *CalendarOccurrence        `json:"occurrence,omitempty"`
	Invited      []calevent.Attendee        `json:"invited"`
	Rooms        []calevent.Attendee        `json:"rooms"`
	Participants []MeetingParticipant       `json:"participants"`
	OwnerRSVP    string                     `json:"owner_rsvp"`
	Items        []MeetingItem              `json:"items"`
	ActionItems  []meetingcontent.ActionRow `json:"action_items"`
	Previous     *MeetingPrevious           `json:"previous,omitempty"`
	EventDetail  *MeetingEventDetail        `json:"event_detail,omitempty"`
}

// ValidateMeetingRecordRef checks the shared CLI/API/MCP wire reference.
func ValidateMeetingRecordRef(ref string) error {
	_, occ, id, err := ParseMeetingRef(ref)
	if err != nil || (occ.MessageID <= 0 && id <= 0) || occ.MessageID > 9007199254740991 || id > 9007199254740991 {
		return fmt.Errorf("%w: invalid meeting reference", ErrInvalidArgument)
	}
	return nil
}

func meetingRecordError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) GetMeetingRecordContext(ctx context.Context, scope SourceScope, ref string) (MeetingRecord, error) {
	if err := ValidateMeetingRecordRef(ref); err != nil {
		return MeetingRecord{}, err
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return MeetingRecord{}, err
	}
	defer release()
	r := s.snapshotReader(ctx)
	budget := calrecur.RequestReadBudget
	kind, parsed, member, err := ParseMeetingRef(ref)
	if err != nil {
		return MeetingRecord{}, err
	}
	var occurrence *CalendarOccurrence
	var target linkOccurrence
	var links []EffectiveLink
	if kind == "msg" {
		visible, err := linkMemberVisible(ctx, r, scope, member)
		if err != nil {
			return MeetingRecord{}, err
		}
		if !visible {
			return MeetingRecord{}, ErrNotFound
		}
		links, err = linksForMember(ctx, r, scope, member, &budget)
		if err != nil {
			return MeetingRecord{}, meetingRecordError(err)
		}
		if len(links) > 0 {
			parsed = links[0].Target
			kind = calendarEventRefKind
		}
	}
	if kind == calendarEventRefKind {
		value, err := calendarOccurrenceWithReader(ctx, r, scope, FormatOccurrenceRef(parsed), &budget)
		if err != nil {
			return MeetingRecord{}, err
		}
		occurrence = &value
		_, canonical, _, _ := ParseMeetingRef(value.Ref)
		target, err = resolveLinkOccurrence(ctx, r, scope, canonical, &budget)
		if err != nil {
			return MeetingRecord{}, meetingRecordError(err)
		}
		links, err = meetingRecordMembers(ctx, r, scope, target, &budget)
		if err != nil {
			return MeetingRecord{}, meetingRecordError(err)
		}
		// A msg anchor is evidence in its own right even if a conflicting decision
		// prevents another copy's inverse lookup from finding it.
		if member > 0 && !slices.ContainsFunc(links, func(l EffectiveLink) bool { return l.MemberMessageID == member }) {
			links = append(links, EffectiveLink{MemberMessageID: member, Target: target.Ref, Basis: "user", Origin: "user"})
		}
		ref = value.Ref
	} else {
		links = []EffectiveLink{{MemberMessageID: member}}
	}
	record := MeetingRecord{Ref: ref, Occurrence: occurrence, Invited: []calevent.Attendee{}, Rooms: []calevent.Attendee{}, Participants: []MeetingParticipant{}, Items: []MeetingItem{}, ActionItems: []meetingcontent.ActionRow{}}
	if occurrence != nil {
		if len(occurrence.Attendees) > meetingRecordMaxPeople {
			return MeetingRecord{}, ErrQueryTooBroad
		}
		for _, attendee := range occurrence.Attendees {
			if attendee.Role == "room" {
				record.Rooms = append(record.Rooms, attendee)
			} else {
				record.Invited = append(record.Invited, attendee)
			}
		}
		identities, err := s.OwnerIdentitiesContext(ctx, occurrence.SourceID)
		if err != nil {
			return MeetingRecord{}, err
		}
		for _, identity := range identities {
			for _, attendee := range record.Invited {
				if strings.EqualFold(strings.TrimSpace(attendee.Email), identity.Address) {
					record.OwnerRSVP = attendee.RSVP
					break
				}
			}
			if record.OwnerRSVP != "" {
				break
			}
		}
		detailRows, err := calendarEventRowsByMessageIDs(ctx, r, scope, []int64{occurrence.MessageID})
		if err != nil {
			return MeetingRecord{}, err
		}
		if len(detailRows) != 1 {
			return MeetingRecord{}, ErrNotFound
		}
		e := detailRows[0].Event
		// A cancellation stub changes status but inherits the master's details.
		if occurrence.SeriesMessageID > 0 && e.Kind == calendarExceptionKind && !e.HasBounds {
			e = target.Row.Event
		}
		record.EventDetail = &MeetingEventDetail{Location: e.Location, Description: e.Description, DescriptionTruncated: e.DescriptionTruncated}
	}
	items, err := meetingRecordItemHeaders(ctx, r, scope, links)
	if err != nil {
		return MeetingRecord{}, err
	}
	items = items[:min(len(items), meetingRecordMaxItems)]
	for i := range items {
		if err := s.populateMeetingRecordItem(ctx, &items[i], i < meetingRecordFullItems); err != nil {
			return MeetingRecord{}, err
		}
		record.Items = append(record.Items, items[i])
		appendMeetingRecordEvidence(&record, items[i])
	}
	if occurrence != nil {
		record.Previous, err = s.meetingRecordPrevious(ctx, r, scope, target, &budget)
		if err != nil {
			return MeetingRecord{}, meetingRecordError(err)
		}
	}
	if len(record.Participants) > meetingRecordMaxPeople {
		return MeetingRecord{}, ErrQueryTooBroad
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return MeetingRecord{}, err
	}
	if len(encoded) > meetingRecordMaxBytes {
		return MeetingRecord{}, ErrQueryTooBroad
	}
	return record, nil
}

func meetingRecordMembers(ctx context.Context, r rowReader, scope SourceScope, target linkOccurrence, budget *int) ([]EffectiveLink, error) {
	copies := []linkOccurrence{target}
	if target.Row.Event.ICalUID != "" {
		predicate, args := meetingRecordCopyPredicate(target)
		rows, broad, err := boundedLinkEventRows(ctx, r, scope, predicate, args)
		if err != nil {
			return nil, err
		}
		if broad {
			return nil, ErrQueryTooBroad
		}
		seen := map[string]bool{FormatOccurrenceRef(target.Ref): true}
		for _, row := range rows {
			ref := OccurrenceRef{MessageID: row.MessageID}
			if row.Event.Kind == "series" {
				ref.Key = target.Ref.Key
				if ref.Key == "" && target.Row.Event.Original != nil {
					ref.Key = target.Row.Event.Original.Key()
				}
			}
			if row.Event.Kind == "series" && ref.Key == "" {
				continue
			}
			other, err := resolveLinkOccurrence(ctx, r, scope, ref, budget)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			key := FormatOccurrenceRef(other.Ref)
			if !seen[key] && occurrenceLogical(other) == occurrenceLogical(target) {
				seen[key] = true
				copies = append(copies, other)
			}
		}
	}
	byID := map[int64]EffectiveLink{}
	for _, copy := range copies {
		links, err := membersForOccurrence(ctx, r, scope, copy.Ref, budget)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			if _, ok := byID[link.MemberMessageID]; !ok {
				byID[link.MemberMessageID] = link
			}
		}
	}
	result := make([]EffectiveLink, 0, len(byID))
	for _, link := range byID {
		result = append(result, link)
	}
	slices.SortFunc(result, func(a, b EffectiveLink) int {
		if a.MemberMessageID < b.MemberMessageID {
			return -1
		}
		if a.MemberMessageID > b.MemberMessageID {
			return 1
		}
		return 0
	})
	return result, nil
}

func meetingRecordCopyPredicate(target linkOccurrence) (string, []any) {
	uid := target.Row.Event.ICalUID
	original, recurring := target.Original, target.Ref.Key != "" || target.Row.Event.Original != nil
	if target.Row.Event.Original != nil {
		original = *target.Row.Event.Original
	} else if target.Ref.Key != "" && (original.Instant.IsZero() && original.Date == "") {
		if parsed, err := calrecur.ParseKey(target.Ref.Key); err == nil {
			original = parsed
		}
	}
	if !recurring {
		return `e.ical_uid = ? AND e.kind = 'single'`, []any{uid}
	}
	exceptionPredicate, exceptionArgs := linkOriginalIdentityPredicate(original)
	var seriesPredicate string
	var seriesArgs []any
	if original.AllDay {
		date, err := time.Parse(time.DateOnly, original.Date)
		if err == nil {
			seriesPredicate = `(e.start_date <= ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)`
			seriesArgs = []any{original.Date, date.UTC().Add(-24 * time.Hour)}
		} else {
			seriesPredicate = `(e.start_date <= ? OR e.recurrence_json LIKE '%RDATE%')`
			seriesArgs = []any{original.Date}
		}
	} else {
		seriesPredicate = `(e.start_at <= ? OR e.recurrence_json LIKE '%RDATE%') AND (e.recurrence_end_at IS NULL OR e.recurrence_end_at >= ?)`
		seriesArgs = []any{original.Instant, original.Instant}
	}
	predicate := `e.ical_uid = ? AND ((e.kind = 'series' AND ` + seriesPredicate + `) OR (e.kind = 'exception' AND ` + exceptionPredicate + `))`
	args := append([]any{uid}, seriesArgs...)
	args = append(args, exceptionArgs...)
	return predicate, args
}
