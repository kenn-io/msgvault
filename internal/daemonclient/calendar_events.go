package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	params := &generated.ListCalendarEventsQuery{From: query.From, To: query.To, Cursor: &query.Cursor}
	params.Limit = query.Limit
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListCalendarEventsResp, error) {
		return client.ListCalendarEventsWithResponse(ctx, &generated.ListCalendarEventsRequestOptions{Query: params})
	})
	if err != nil {
		return store.CalendarOccurrencePage{}, err
	}
	if response.JSON200 == nil {
		return store.CalendarOccurrencePage{}, errors.New("calendar events: empty response")
	}
	return calendarOccurrencePageFromAPI(response.JSON200), nil
}

func calendarOccurrencePageFromAPI(decoded *generated.ListCalendarEventsResponse) store.CalendarOccurrencePage {
	page := store.CalendarOccurrencePage{NextCursor: stringValue(decoded.NextCursor)}
	if decoded.Occurrences != nil {
		page.Occurrences = make([]store.CalendarOccurrence, 0, len(decoded.Occurrences))
		for _, o := range decoded.Occurrences {
			page.Occurrences = append(page.Occurrences, calendarOccurrenceFromAPI(o))
		}
	}
	if decoded.Unexpandable != nil {
		page.Unexpandable = make([]store.CalendarUnexpandable, 0, len(decoded.Unexpandable))
		for _, u := range decoded.Unexpandable {
			page.Unexpandable = append(page.Unexpandable, store.CalendarUnexpandable(u))
		}
	}
	return page
}

func (c *Client) GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetCalendarEventResp, error) {
		return client.GetCalendarEventWithResponse(ctx, &generated.GetCalendarEventRequestOptions{PathParams: &generated.GetCalendarEventPath{Ref: ref}})
	})
	if err != nil {
		return store.CalendarOccurrenceDetail{}, err
	}
	if response.JSON200 == nil {
		return store.CalendarOccurrenceDetail{}, errors.New("calendar event: empty response")
	}
	o := response.JSON200
	occurrence := calendarOccurrenceFromAPI(generated.CalendarOccurrence{AllDay: o.AllDay, Attendees: o.Attendees, DeletedFromSource: o.DeletedFromSource, End: o.End, EndDate: o.EndDate, MessageID: o.MessageID, Ref: o.Ref, SeriesMessageID: o.SeriesMessageID, SourceID: o.SourceID, Start: o.Start, StartDate: o.StartDate, Status: o.Status, Subject: o.Subject})
	return store.CalendarOccurrenceDetail{CalendarOccurrence: occurrence, Location: stringValue(o.Location), Description: stringValue(o.Description), JoinURL: stringValue(o.JoinURL)}, nil
}

func calendarOccurrenceFromAPI(o generated.CalendarOccurrence) store.CalendarOccurrence {
	var attendees []calevent.Attendee
	if o.Attendees != nil {
		attendees = make([]calevent.Attendee, 0, len(o.Attendees))
		for _, a := range o.Attendees {
			attendees = append(attendees, calevent.Attendee{Email: a.Email, DisplayName: a.DisplayName, Role: a.Role, RSVP: a.Rsvp, IsCopySelf: a.IsSelf})
		}
	}
	return store.CalendarOccurrence{Ref: o.Ref, MessageID: o.MessageID, SeriesMessageID: o.SeriesMessageID, SourceID: o.SourceID, Subject: o.Subject, Start: o.Start, End: o.End, AllDay: o.AllDay, StartDate: o.StartDate, EndDate: o.EndDate, Status: o.Status, DeletedFromSource: o.DeletedFromSource, Attendees: attendees}
}
