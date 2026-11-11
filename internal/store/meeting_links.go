package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

const (
	meetingRefEvent   = "evt"
	meetingRefMessage = "msg"
)

type OccurrenceRef struct {
	MessageID int64
	Identity  calrecur.Identity
}

// ParseMeetingLinkRef parses a note or event occurrence reference.
func ParseMeetingLinkRef(value string) (MeetingLinkRef, error) {
	kind, value, ok := strings.Cut(value, ":")
	if !ok || kind != meetingRefEvent && kind != meetingRefMessage {
		return MeetingLinkRef{}, errors.New("use msg:<id> or evt:<id>[@occurrence key]")
	}
	message, key, hasKey := strings.Cut(value, "@")
	id, err := strconv.ParseInt(message, 10, 64)
	if err != nil || id <= 0 || hasKey && (kind != meetingRefEvent || key == "") {
		return MeetingLinkRef{}, errors.New("invalid meeting reference")
	}
	var identity calrecur.Identity
	if hasKey {
		var base string
		base, identity, ok = calrecur.ParseInstanceID("event_" + key)
		if !ok || base != "event" {
			return MeetingLinkRef{}, errors.New("invalid occurrence key")
		}
	}
	return MeetingLinkRef{Kind: kind, MessageID: id, Identity: identity}, nil
}

func (s *Store) meetingEventRows(ctx context.Context, predicate string, args []any, budget *int) ([]CalendarEventRow, error) {
	rows, err := queryCalendarEventRowsWithinBudget(ctx, s.db, predicate, args, *budget)
	if err != nil {
		return nil, err
	}
	*budget -= len(rows)
	owners := make(map[int64]string)
	for i := range rows {
		owner, ok := owners[rows[i].SourceID]
		if !ok {
			source, err := s.GetSourceByIDContext(ctx, rows[i].SourceID)
			if err != nil {
				return nil, err
			}
			owner = calendarMailbox(source.SyncConfig)
			owners[rows[i].SourceID] = owner
		}
		rows[i].CalendarID = owner
	}
	return rows, nil
}

func formatMeetingOccurrence(ref OccurrenceRef) string {
	value := fmt.Sprintf("evt:%d", ref.MessageID)
	if ref.Identity != (calrecur.Identity{}) {
		value += "@" + ref.Identity.Key()
	}
	return value
}
