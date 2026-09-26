package daemonclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// originalMessageMinAPISchemaVersion is the first daemon API schema that
// serves /api/v1/cli/message/original and /api/v1/cli/message/thread.
const originalMessageMinAPISchemaVersion = "2.30.0"

var _ query.OriginalMessageReader = (*Engine)(nil)

// ReadOriginalMessage fetches a message's original MIME and provenance.
func (e *Engine) ReadOriginalMessage(ctx context.Context, ref query.MessageRef) (*query.OriginalMessage, error) {
	if (ref.ID == 0) == (ref.SourceMessageID == "") {
		return nil, query.ErrInvalidMessageRef
	}
	if err := e.requireOriginalMessageCapability(ctx); err != nil {
		return nil, err
	}
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetCLIMessageOriginalResp, error) {
		return client.GetCLIMessageOriginalWithResponse(ctx, &generated.GetCLIMessageOriginalRequestOptions{
			Query: &generated.GetCLIMessageOriginalQuery{
				ID:              optionalPositiveInt64Value(ref.ID),
				SourceMessageID: optionalString(ref.SourceMessageID),
				Account:         optionalString(ref.Account),
			},
		})
	})
	if err != nil {
		return nil, originalExportError(err)
	}
	body := resp.JSON200
	mime, err := base64.StdEncoding.DecodeString(body.Mime)
	if err != nil {
		return nil, fmt.Errorf("decode original MIME: %w", err)
	}
	return &query.OriginalMessage{MessageRecord: messageRecordFromGenerated(body.Message), MIME: mime}, nil
}

// ListThread fetches one page of a conversation listing.
func (e *Engine) ListThread(ctx context.Context, q query.ThreadQuery) (*query.ThreadPage, error) {
	if err := e.requireOriginalMessageCapability(ctx); err != nil {
		return nil, err
	}
	resp, err := APIResponse(e.store, func(client *apiclient.Client) (*generated.GetCLIMessageThreadResp, error) {
		return client.GetCLIMessageThreadWithResponse(ctx, &generated.GetCLIMessageThreadRequestOptions{
			Query: &generated.GetCLIMessageThreadQuery{
				ID:              optionalPositiveInt64Value(q.ID),
				SourceMessageID: optionalString(q.SourceMessageID),
				ThreadID:        optionalString(q.ThreadID),
				Account:         optionalString(q.Account),
				Limit:           optionalPositiveInt64(q.Limit),
				Offset:          optionalPositiveInt64(q.Offset),
			},
		})
	})
	if err != nil {
		return nil, originalExportError(err)
	}
	return threadPageFromGenerated(resp.JSON200), nil
}

func (e *Engine) requireOriginalMessageCapability(ctx context.Context) error {
	supported, err := e.store.SupportsAPISchemaVersion(ctx, originalMessageMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check daemon original-message capability: %w", err)
	}
	if !supported {
		return fmt.Errorf("original message export needs daemon API schema %s or newer: %w: %w",
			originalMessageMinAPISchemaVersion, ErrNotSupported, query.ErrOriginalExportUnsupported)
	}
	return nil
}

// originalExportError maps the routes' stable error codes back to the
// query package's sentinels so callers branch the same way locally and
// through the daemon.
func originalExportError(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "invalid_request":
		return fmt.Errorf("%s: %w", apiErr.Message, query.ErrInvalidMessageRef)
	case apiErrorCodeMessageNotFound:
		return fmt.Errorf("%s: %w", apiErr.Message, store.ErrMessageNotFound)
	case "thread_not_found":
		return fmt.Errorf("%s: %w", apiErr.Message, query.ErrThreadNotFound)
	case "original_mime_unavailable":
		return fmt.Errorf("%s: %w", apiErr.Message, query.ErrOriginalMIMEUnavailable)
	case "original_export_unavailable":
		return fmt.Errorf("%s: %w", apiErr.Message, query.ErrOriginalExportUnsupported)
	case "message_ambiguous":
		return fmt.Errorf("%s: %w", apiErr.Message, query.ErrAmbiguousReference)
	}
	return err
}

func messageRecordFromGenerated(record generated.MessageRecord) query.MessageRecord {
	return query.MessageRecord{
		MessageID:            int64Value(record.MessageID),
		SourceMessageID:      stringValue(record.SourceMessageID),
		ConversationID:       record.ConversationID,
		SourceConversationID: record.SourceConversationID,
		SourceID:             record.SourceID,
		Account:              record.Account,
		SourceType:           record.SourceType,
		LastSyncAt:           copyTime(record.LastSyncAt),
	}
}

func threadPageFromGenerated(page *generated.ThreadPage) *query.ThreadPage {
	out := &query.ThreadPage{
		MessageID:            int64Value(page.MessageID),
		SourceMessageID:      stringValue(page.SourceMessageID),
		ConversationID:       page.ConversationID,
		SourceConversationID: page.SourceConversationID,
		SourceID:             page.SourceID,
		Account:              page.Account,
		SourceType:           page.SourceType,
		LastSyncAt:           copyTime(page.LastSyncAt),
		Total:                page.Total,
		Offset:               int(page.Offset),
		HasMore:              page.HasMore,
		Messages:             make([]query.ThreadMessage, 0, len(page.Messages)),
	}
	for _, msg := range page.Messages {
		out.Messages = append(out.Messages, query.ThreadMessage{
			ID:                  msg.ID,
			SourceMessageID:     msg.SourceMessageID,
			Subject:             msg.Subject,
			SentAt:              copyTime(msg.SentAt),
			From:                addressesFromGenerated(msg.From),
			To:                  addressesFromGenerated(msg.To),
			Cc:                  addressesFromGenerated(msg.Cc),
			HasRaw:              msg.HasRaw,
			AttachmentCount:     int(msg.AttachmentCount),
			DeletedFromSourceAt: copyTime(msg.DeletedFromSourceAt),
		})
	}
	return out
}

func addressesFromGenerated(addresses []generated.Address) []query.Address {
	out := make([]query.Address, 0, len(addresses))
	for _, addr := range addresses {
		out = append(out, query.Address{Email: addr.Email, Name: addr.Name})
	}
	return out
}
