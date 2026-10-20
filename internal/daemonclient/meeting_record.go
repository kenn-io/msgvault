package daemonclient

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonMeetingsResponse is the daemon's scoped, cursor-paged meeting history.
type PersonMeetingsResponse struct {
	Items      []store.MeetingRecordSummary `json:"items"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

func (c *Client) GetMeeting(ctx context.Context, ref string) (store.MeetingRecord, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetMeetingResp, error) {
		return client.GetMeetingWithResponse(ctx, &generated.GetMeetingRequestOptions{PathParams: &generated.GetMeetingPath{Ref: ref}})
	})
	if err != nil {
		return store.MeetingRecord{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingRecord{}, errors.New("get meeting: empty response")
	}
	record, err := decodeMeetingResponse[store.MeetingRecord]("get meeting", response.Body)
	if err != nil {
		return store.MeetingRecord{}, err
	}
	return *record, nil
}

func (c *Client) ListPersonMeetings(ctx context.Context, ref string, q store.PersonMeetingsQuery) (PersonMeetingsResponse, error) {
	query := generated.ListPersonMeetingsQuery{}
	if !q.From.IsZero() {
		value := q.From.Format(time.RFC3339Nano)
		query.From = &value
	}
	if !q.To.IsZero() {
		value := q.To.Format(time.RFC3339Nano)
		query.To = &value
	}
	if q.Limit != 0 {
		value := int64(q.Limit)
		query.Limit = &value
	}
	if q.Cursor != "" {
		query.Cursor = &q.Cursor
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListPersonMeetingsResp, error) {
		return client.ListPersonMeetingsWithResponse(ctx, &generated.ListPersonMeetingsRequestOptions{PathParams: &generated.ListPersonMeetingsPath{Ref: ref}, Query: &query})
	})
	if err != nil {
		return PersonMeetingsResponse{}, err
	}
	if response.JSON200 == nil {
		return PersonMeetingsResponse{}, errors.New("list person meetings: empty response")
	}
	page, err := decodeMeetingResponse[PersonMeetingsResponse]("list person meetings", response.Body)
	if err != nil {
		return PersonMeetingsResponse{}, err
	}
	return *page, nil
}

func (c *Client) SuggestMeetingLinks(ctx context.Context, ref string, limit int) ([]store.MeetingSuggestion, error) {
	query := generated.SuggestMeetingLinksQuery{}
	if limit != 0 {
		value := int64(limit)
		query.Limit = &value
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SuggestMeetingLinksResp, error) {
		return client.SuggestMeetingLinksWithResponse(ctx, &generated.SuggestMeetingLinksRequestOptions{PathParams: &generated.SuggestMeetingLinksPath{Ref: ref}, Query: &query})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("suggest meeting links: empty response")
	}
	items, err := decodeMeetingResponse[[]store.MeetingSuggestion]("suggest meeting links", response.Body)
	if err != nil {
		return nil, err
	}
	return *items, nil
}

func (c *Client) LinkMeeting(ctx context.Context, ref string, id int64) (store.MeetingRecord, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.LinkMeetingResp, error) {
		return client.LinkMeetingWithResponse(ctx, &generated.LinkMeetingRequestOptions{PathParams: &generated.LinkMeetingPath{Ref: ref}, Body: &generated.LinkMeetingBody{MessageID: id}})
	})
	if err != nil {
		return store.MeetingRecord{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingRecord{}, errors.New("link meeting: empty response")
	}
	record, err := decodeMeetingResponse[store.MeetingRecord]("link meeting", response.Body)
	if err != nil {
		return store.MeetingRecord{}, err
	}
	return *record, nil
}

func (c *Client) UnlinkMeeting(ctx context.Context, ref string, id int64) (store.MeetingRecord, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.UnlinkMeetingResp, error) {
		return client.UnlinkMeetingWithResponse(ctx, &generated.UnlinkMeetingRequestOptions{PathParams: &generated.UnlinkMeetingPath{Ref: ref, MessageID: id}})
	})
	if err != nil {
		return store.MeetingRecord{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingRecord{}, errors.New("unlink meeting: empty response")
	}
	record, err := decodeMeetingResponse[store.MeetingRecord]("unlink meeting", response.Body)
	if err != nil {
		return store.MeetingRecord{}, err
	}
	return *record, nil
}
