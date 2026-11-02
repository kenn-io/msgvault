package daemonclient

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetStats fetches stats from the daemon API.
func (c *Client) GetStats() (*store.Stats, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetStatsResp, error) {
		return client.GetStatsWithResponse(c.requestContext())
	})
	if err != nil {
		return nil, err
	}
	return storeStatsFromGenerated(*resp.JSON200), nil
}

// VectorSearchAvailableForMessageType reports whether the daemon's text-vector
// lane can serve searches for messageType. An empty advertised scope means the
// text index covers every message type.
func (c *Client) VectorSearchAvailableForMessageType(ctx context.Context, messageType string) (bool, error) {
	stats, err := c.vectorSearchStats(ctx)
	if err != nil {
		return false, err
	}
	if !vectorSearchAvailable(stats) {
		return false, nil
	}
	if len(stats.VectorTextMessageTypes) == 0 {
		return true, nil
	}
	messageType = strings.TrimSpace(messageType)
	return slices.ContainsFunc(stats.VectorTextMessageTypes, func(indexedType string) bool {
		return strings.EqualFold(strings.TrimSpace(indexedType), messageType)
	}), nil
}

func (c *Client) vectorSearchStats(ctx context.Context) (*generated.StatsResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetStatsResp, error) {
		return client.GetStatsWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.JSON200 == nil {
		return &generated.StatsResponse{}, nil
	}
	return resp.JSON200, nil
}

func vectorSearchAvailable(stats *generated.StatsResponse) bool {
	if stats == nil {
		return false
	}
	// Newer daemons report the text lane separately: a multimodal-only
	// daemon is vector-"ready" without serving semantic message search, so
	// the shared status must not enable text-vector tools there.
	if textStatus := stats.VectorTextStatus; textStatus != nil && *textStatus != "" {
		return *textStatus != vectorStatusDisabled
	}
	if status := stats.VectorStatus; status != nil && *status != "" {
		return *status != vectorStatusDisabled
	}
	if stats.VectorSearch == nil {
		return false
	}
	return stats.VectorSearch.Enabled
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
		Verification:      verificationFromGenerated(m.Verification),
		ID:                m.ID,
		SourceID:          int64Value(m.SourceID),
		SourceMessageID:   stringValue(m.SourceMessageID),
		ConversationID:    int64Value(m.ConversationID),
		Subject:           m.Subject,
		MessageType:       stringValue(m.MessageType),
		From:              m.From,
		FromEmail:         stringValue(m.FromEmail),
		FromName:          stringValue(m.FromName),
		FromPhone:         stringValue(m.FromPhone),
		To:                m.To,
		Cc:                m.Cc,
		Bcc:               m.Bcc,
		SentAt:            parseTime(m.SentAt),
		DeletedAt:         deletedAt,
		Snippet:           m.Snippet,
		Labels:            m.Labels,
		HasAttachments:    m.HasAttachments,
		HiddenTextPresent: m.HiddenTextPresent,
		SizeEstimate:      m.SizeBytes,
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
	bodyHTML := stringValue(m.BodyHTML)
	bodyText := ""
	bodyFormat := ""
	if m.BodyFormat != nil {
		bodyFormat = string(*m.BodyFormat)
	}
	switch bodyFormat {
	case "text":
		bodyText = m.Body
	case "":
		// Older daemons do not report the body format. If HTML is present,
		// the body field is ambiguous, so keep it out of plain-text readers.
		if bodyHTML == "" {
			bodyText = m.Body
		}
	}
	var deletedAt *time.Time
	if m.DeletedAt != nil {
		t := parseTime(*m.DeletedAt)
		if !t.IsZero() {
			deletedAt = &t
		}
	}
	msg := &store.APIMessage{
		Verification:      verificationFromGenerated(m.Verification),
		RelatedMessageID:  m.RelatedMessageID,
		ConversationTitle: stringValue(m.ConversationTitle),

		ID:                m.ID,
		IsFromMe:          boolValue(m.IsFromMe),
		Calendar:          calendarProjectionFromGenerated(m.Calendar),
		SourceID:          int64Value(m.SourceID),
		SourceMessageID:   stringValue(m.SourceMessageID),
		ConversationID:    int64Value(m.ConversationID),
		Subject:           m.Subject,
		MessageType:       stringValue(m.MessageType),
		From:              m.From,
		FromEmail:         stringValue(m.FromEmail),
		FromName:          stringValue(m.FromName),
		FromPhone:         stringValue(m.FromPhone),
		To:                m.To,
		Cc:                m.Cc,
		Bcc:               m.Bcc,
		SentAt:            parseTime(m.SentAt),
		DeletedAt:         deletedAt,
		Snippet:           m.Snippet,
		Labels:            m.Labels,
		HasAttachments:    m.HasAttachments,
		HiddenTextPresent: m.HiddenTextPresent,
		SizeEstimate:      m.SizeBytes,
		Body:              m.Body,
		BodyText:          bodyText,
		BodyHTML:          bodyHTML,
		Attachments:       apiAttachmentsFromGenerated(m.Attachments),
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
			Docbank:     docbankRefsFromGenerated(a.Docbank),
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
		return client.ListMessagesWithResponse(c.requestContext(), &generated.ListMessagesRequestOptions{
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
	return c.GetMessageContext(c.requestContext(), id)
}

// GetMessageContext fetches a single message by ID using the supplied context.
func (c *Client) GetMessageContext(ctx context.Context, id int64) (*store.APIMessage, error) {
	resp, err := APIResponseWithNotFound(
		c,
		func(client *apiclient.Client) (*generated.GetMessageResp, error) {
			return client.GetMessageWithResponse(ctx, &generated.GetMessageRequestOptions{
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

	message := generatedDetailToAPIMessage(resp.JSON200)
	if message != nil && resp.JSON200.BodyFormat == nil && message.BodyHTML != "" && message.SourceMessageID != "" {
		// Older daemons return one ambiguous body field alongside body_html.
		// Fetch the CLI detail, which preserves plain-text and HTML variants
		// separately, by the resolved archive ID before exposing the message.
		variants, err := c.GetCLIMessage(ctx, strconv.FormatInt(message.ID, 10))
		if err != nil {
			return nil, fmt.Errorf("get legacy message body variants for message %d: %w", id, err)
		}
		if variants == nil || variants.ID != message.ID ||
			(message.SourceID != 0 && variants.SourceID != 0 && variants.SourceID != message.SourceID) ||
			(message.SourceMessageID != "" && variants.SourceMessageID != "" && variants.SourceMessageID != message.SourceMessageID) {
			return nil, fmt.Errorf("legacy message body variants resolved a different message for message %d", id)
		}
		message.BodyText = variants.BodyText
		if variants.BodyHTML != "" {
			message.BodyHTML = variants.BodyHTML
		}
	}
	return message, nil
}

// SearchMessages searches messages via the daemon API.
// Callers (API layer) always provide page-aligned offsets.
func (c *Client) SearchMessages(query string, offset, limit int) ([]store.APIMessage, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	page := (offset / limit) + 1

	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchMessagesResp, error) {
		return client.SearchMessagesWithResponse(c.requestContext(), &generated.SearchMessagesRequestOptions{
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
		return client.ListAccountsWithResponse(c.requestContext())
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
