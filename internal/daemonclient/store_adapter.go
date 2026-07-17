package daemonclient

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetStats fetches stats from the daemon API.
func (c *Client) GetStats() (*store.Stats, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetStatsResp, error) {
		return client.GetStatsWithResponse(context.Background())
	})
	if err != nil {
		return nil, err
	}
	return storeStatsFromGenerated(*resp.JSON200), nil
}

// VectorSearchAvailable reports whether the daemon can serve vector search,
// so callers (e.g. the MCP server) know whether to register vector-backed
// tools. It reads the public stats endpoint.
//
// Vector init is asynchronous: a daemon can be serving while the subsystem is
// still `initializing`, at which point vector stats are nil but the tools
// should still be registered. The daemon reports its subsystem state in
// `vector_status` (values from internal/api/vector_status.go:
// disabled/initializing/ready/stale/error). Any non-disabled status is treated
// as capable — including `error`, so a tool call surfaces the daemon's 503
// detail rather than the tool silently going missing. Older daemons that omit
// `vector_status` fall back to the `vector_search.enabled` flag.
func (c *Client) VectorSearchAvailable(ctx context.Context) (bool, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetStatsResp, error) {
		return client.GetStatsWithResponse(ctx)
	})
	if err != nil {
		return false, err
	}
	if resp == nil || resp.JSON200 == nil {
		return false, nil
	}
	if status := resp.JSON200.VectorStatus; status != nil && *status != "" {
		return *status != vectorStatusDisabled, nil
	}
	if resp.JSON200.VectorSearch == nil {
		return false, nil
	}
	return resp.JSON200.VectorSearch.Enabled, nil
}

// vectorStatusDisabled mirrors api.VectorStatusDisabled. It is duplicated here
// rather than imported because internal/api imports this package, so importing
// it back would create a cycle.
const vectorStatusDisabled = "disabled"

// parseTime parses RFC3339 time string.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func generatedMessageToAPIMessage(m generated.MessageSummary) store.APIMessage {
	var deletedAt *time.Time
	if m.DeletedAt != nil {
		t := parseTime(*m.DeletedAt)
		if !t.IsZero() {
			deletedAt = &t
		}
	}
	return store.APIMessage{
		ID:              m.ID,
		SourceID:        int64Value(m.SourceID),
		SourceMessageID: stringValue(m.SourceMessageID),
		ConversationID:  int64Value(m.ConversationID),
		Subject:         m.Subject,
		MessageType:     stringValue(m.MessageType),
		From:            m.From,
		FromEmail:       stringValue(m.FromEmail),
		FromName:        stringValue(m.FromName),
		FromPhone:       stringValue(m.FromPhone),
		To:              m.To,
		Cc:              m.Cc,
		Bcc:             m.Bcc,
		SentAt:          parseTime(m.SentAt),
		DeletedAt:       deletedAt,
		Snippet:         m.Snippet,
		Labels:          m.Labels,
		HasAttachments:  m.HasAttachments,
		SizeEstimate:    m.SizeBytes,
	}
}

func apiMessagesFromGenerated(msgs []generated.MessageSummary) []store.APIMessage {
	if msgs == nil {
		return nil
	}
	messages := make([]store.APIMessage, len(msgs))
	for i, m := range msgs {
		messages[i] = generatedMessageToAPIMessage(m)
	}
	return messages
}

func generatedDetailToAPIMessage(m *generated.MessageDetail) *store.APIMessage {
	if m == nil {
		return nil
	}
	var deletedAt *time.Time
	if m.DeletedAt != nil {
		t := parseTime(*m.DeletedAt)
		if !t.IsZero() {
			deletedAt = &t
		}
	}
	msg := &store.APIMessage{
		ID:              m.ID,
		SourceID:        int64Value(m.SourceID),
		SourceMessageID: stringValue(m.SourceMessageID),
		ConversationID:  int64Value(m.ConversationID),
		Subject:         m.Subject,
		MessageType:     stringValue(m.MessageType),
		From:            m.From,
		FromEmail:       stringValue(m.FromEmail),
		FromName:        stringValue(m.FromName),
		FromPhone:       stringValue(m.FromPhone),
		To:              m.To,
		Cc:              m.Cc,
		Bcc:             m.Bcc,
		SentAt:          parseTime(m.SentAt),
		DeletedAt:       deletedAt,
		Snippet:         m.Snippet,
		Labels:          m.Labels,
		HasAttachments:  m.HasAttachments,
		SizeEstimate:    m.SizeBytes,
		Body:            m.Body,
		Attachments:     apiAttachmentsFromGenerated(m.Attachments),
	}
	return msg
}

func apiAttachmentsFromGenerated(attachments []generated.AttachmentInfo) []store.APIAttachment {
	if attachments == nil {
		return nil
	}
	out := make([]store.APIAttachment, len(attachments))
	for i, a := range attachments {
		out[i] = store.APIAttachment{
			ID:          a.ID,
			Filename:    a.Filename,
			MimeType:    a.MimeType,
			Size:        a.SizeBytes,
			ContentHash: stringValue(a.ContentHash),
			URL:         stringValue(a.URL),
		}
	}
	return out
}

// ListMessages fetches a paginated list of messages.
// Callers (API layer) always provide page-aligned offsets.
func (c *Client) ListMessages(offset, limit int) ([]store.APIMessage, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListMessagesResp, error) {
		return client.ListMessagesWithResponse(context.Background(), &generated.ListMessagesRequestOptions{
			Query: &generated.ListMessagesQuery{
				Page:     int64FromInt(page),
				PageSize: int64FromInt(limit),
			},
		})
	})
	if err != nil {
		return nil, 0, err
	}
	return apiMessagesFromGenerated(resp.JSON200.Messages), resp.JSON200.Total, nil
}

// GetMessage fetches a single message by ID.
func (c *Client) GetMessage(id int64) (*store.APIMessage, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetMessageResp, error) {
			return client.GetMessageWithResponse(context.Background(), &generated.GetMessageRequestOptions{
				PathParams: &generated.GetMessagePath{ID: id},
			})
		},
		func(*generated.GetMessageResp) error {
			return fmt.Errorf("message %d: %w", id, store.ErrMessageNotFound)
		},
	)
	if err != nil {
		return nil, err
	}

	return generatedDetailToAPIMessage(resp.JSON200), nil
}

// SearchMessages searches messages via the daemon API.
// Callers (API layer) always provide page-aligned offsets.
func (c *Client) SearchMessages(query string, offset, limit int) ([]store.APIMessage, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchMessagesResp, error) {
		return client.SearchMessagesWithResponse(context.Background(), &generated.SearchMessagesRequestOptions{
			Query: &generated.SearchMessagesQuery{
				Q:        query,
				Page:     int64FromInt(page),
				PageSize: int64FromInt(limit),
			},
		})
	})
	if err != nil {
		return nil, 0, err
	}

	sr, err := DecodeGeneratedSearchBody[generated.SearchResult]("search", resp.Body)
	if err != nil {
		return nil, 0, err
	}

	return apiMessagesFromGenerated(sr.Messages), sr.Total, nil
}

// AccountInfo represents an account in list responses.
type AccountInfo struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	LastSyncAt  string `json:"last_sync_at,omitempty"`
	NextSyncAt  string `json:"next_sync_at,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// ListAccounts fetches configured accounts from the daemon API.
func (c *Client) ListAccounts() ([]AccountInfo, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListAccountsResp, error) {
		return client.ListAccountsWithResponse(context.Background())
	})
	if err != nil {
		return nil, err
	}
	return accountInfosFromGenerated(resp.JSON200.Accounts), nil
}

func accountInfosFromGenerated(accounts []generated.AccountInfo) []AccountInfo {
	if accounts == nil {
		return nil
	}
	out := make([]AccountInfo, len(accounts))
	for i, account := range accounts {
		out[i] = AccountInfo{
			ID:          account.ID,
			Email:       account.Email,
			DisplayName: stringValue(account.DisplayName),
			LastSyncAt:  stringValue(account.LastSyncAt),
			NextSyncAt:  stringValue(account.NextSyncAt),
			Schedule:    stringValue(account.Schedule),
			Enabled:     account.Enabled,
		}
	}
	return out
}
