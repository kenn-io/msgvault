package daemonclient

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListCalendarEvents(ctx context.Context, f store.CalendarOccurrenceFilter) (store.CalendarOccurrencePage, error) {
	limit := int64(f.Limit)
	q := generated.ListCalendarEventsQuery{From: f.From.Format(time.RFC3339Nano), To: f.To.Format(time.RFC3339Nano), IncludeCancelled: &f.IncludeCancelled}
	if f.Limit != 0 {
		q.Limit = &limit
	}
	if f.AttendeeEmail != "" {
		q.Attendee = &f.AttendeeEmail
	}
	if f.Cursor != "" {
		q.Cursor = &f.Cursor
	}
	if len(f.SourceIDs) > 0 {
		ids := make([]string, len(f.SourceIDs))
		for i, id := range f.SourceIDs {
			ids[i] = strconv.FormatInt(id, 10)
		}
		value := strings.Join(ids, ",")
		q.SourceIds = &value
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCalendarEventsResp, error) {
		return client.ListCalendarEventsWithResponse(ctx, &generated.ListCalendarEventsRequestOptions{Query: &q})
	})
	if err != nil {
		return store.CalendarOccurrencePage{}, err
	}
	if response.JSON200 == nil {
		return store.CalendarOccurrencePage{}, errors.New("list calendar events: empty response")
	}
	page, err := decodeMeetingResponse[store.CalendarOccurrencePage]("list calendar events", response.Body)
	if err != nil {
		return store.CalendarOccurrencePage{}, err
	}
	return *page, nil
}

func (c *Client) GetCalendarEvent(ctx context.Context, ref string) (store.CalendarOccurrence, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetCalendarEventResp, error) {
		return client.GetCalendarEventWithResponse(ctx, &generated.GetCalendarEventRequestOptions{PathParams: &generated.GetCalendarEventPath{Ref: ref}})
	})
	if err != nil {
		return store.CalendarOccurrence{}, err
	}
	if response.JSON200 == nil {
		return store.CalendarOccurrence{}, errors.New("get calendar event: empty response")
	}
	occurrence, err := decodeMeetingResponse[store.CalendarOccurrence]("get calendar event", response.Body)
	if err != nil {
		return store.CalendarOccurrence{}, err
	}
	return *occurrence, nil
}
