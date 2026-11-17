package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) MeetingRecord(ctx context.Context, ref string) (store.MeetingRecord, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetMeetingRecordResp, error) {
		return client.GetMeetingRecordWithResponse(ctx, &generated.GetMeetingRecordRequestOptions{Query: &generated.GetMeetingRecordQuery{Ref: ref}})
	})
	if err != nil {
		return store.MeetingRecord{}, err
	}
	if response.JSON200 == nil {
		return store.MeetingRecord{}, errors.New("meeting record: empty response")
	}
	decoded, err := decodeMeetingResponse[store.MeetingRecord]("meeting record", response.Body)
	if err != nil {
		return store.MeetingRecord{}, err
	}
	return *decoded, nil
}
