package daemonclient

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// EmailFamilyContext reads an email's related copies through the owning daemon.
func (c *Client) EmailFamilyContext(ctx context.Context, id int64) (*store.EmailFamily, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetEmailFamilyResp, error) {
			return client.GetEmailFamilyWithResponse(ctx, &generated.GetEmailFamilyRequestOptions{
				PathParams: &generated.GetEmailFamilyPath{ID: id},
			})
		},
		func(*generated.GetEmailFamilyResp) error {
			return fmt.Errorf("message %d: %w", id, store.ErrMessageNotFound)
		},
	)
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty email family response")
	}
	return emailFamilyFromAPI(resp.JSON200), nil
}

// emailFamilyFromAPI maps the generated response onto the store type so the
// CLI and MCP share one shape.
func emailFamilyFromAPI(in *generated.EmailFamily) *store.EmailFamily {
	out := &store.EmailFamily{
		AnchorMessageID: in.AnchorMessageID,
		Members:         make([]store.EmailFamilyMember, 0, len(in.Members)),
		Truncated:       in.Truncated,
		TruncatedReason: stringValue(in.TruncatedReason),
		PendingLinkage:  in.PendingLinkage,
	}
	for _, m := range in.Members {
		out.Members = append(out.Members, store.EmailFamilyMember{
			MessageID:        m.MessageID,
			SourceID:         m.SourceID,
			SourceType:       m.SourceType,
			SourceIdentifier: m.SourceIdentifier,
			AccountAddress:   stringValue(m.AccountAddress),
			AccountPath:      stringValue(m.AccountPath),
			Draft:            m.Draft,
			ConversationID:   m.ConversationID,
			Subject:          stringValue(m.Subject),
			SentAt:           m.SentAt,
			LinkageOutcome:   m.LinkageOutcome,
			Evidence:         m.Evidence,
		})
	}
	return out
}
