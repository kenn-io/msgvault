package daemonclient

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/verification"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const VerificationArrivalsMinAPISchemaVersion = "3.37.0"

func (c *Client) ListVerifications(ctx context.Context, options verification.ListOptions) (*verification.ArrivalPage, error) {
	query := &generated.ListVerificationsQuery{Sender: optionalString(options.Sender), Kind: optionalString(options.Kind), IncludeExpired: &options.IncludeExpired, Reveal: &options.Reveal}
	if !options.Since.IsZero() {
		since := options.Since.Format(time.RFC3339)
		query.Since = &since
	}
	if options.Limit != 0 {
		query.Limit = int64FromInt(options.Limit)
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListVerificationsResp, error) {
		return client.ListVerificationsWithResponse(ctx, &generated.ListVerificationsRequestOptions{Query: query})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty verification arrivals response")
	}
	wire := resp.JSON200
	page := &verification.ArrivalPage{Data: []verification.Arrival{}, Since: wire.Since, Before: wire.Before, Scanned: int(wire.Scanned), ScanTruncated: wire.ScanTruncated}
	for _, item := range wire.Data {
		if item.Verification.Masked && len(item.Values) > 0 {
			return nil, errors.New("masked verification response contains values")
		}
		arrival := verification.Arrival{MessageID: item.MessageID, SourceID: item.SourceID, SourceMessageID: item.SourceMessageID, ConversationID: item.ConversationID, Verification: verificationFromGenerated(&item.Verification)}
		for _, value := range item.Values {
			arrival.Values = append(arrival.Values, verification.RevealedValue{Kind: value.Kind, Value: value.Value, Host: stringValue(value.Host)})
		}
		page.Data = append(page.Data, arrival)
	}
	return page, nil
}
