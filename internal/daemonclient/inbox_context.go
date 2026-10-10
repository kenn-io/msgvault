package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// InboxContext reads bounded archived content through the current caller's
// admitted contract. It never follows redirects or retries a request.
func (c *Client) InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	descriptor, err := c.MCPCapabilities(ctx)
	if err != nil || !descriptor.HasInboxContextContract() {
		return nil, inboxcontrol.ErrUnavailable
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	var body generated.GetInboxContextBody
	if json.Unmarshal(data, &body) != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodPost, "/api/v1/inbox/context", &generated.GetInboxContextRequestOptions{Body: &body}, &transport)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	// A fully escaped maximum-size text body still fits within this envelope.
	data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, inboxcontrol.ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		cause := inboxcontrol.ErrUnavailable
		switch response.StatusCode {
		case http.StatusBadRequest:
			cause = inboxcontrol.ErrInvalid
		case http.StatusUnauthorized, http.StatusForbidden:
			cause = inboxcontrol.ErrDenied
		}
		return nil, fmt.Errorf("%w (HTTP %d)", cause, response.StatusCode)
	}
	// Pointers preserve required-field presence, including known empty text
	// and false markers. A missing marker must not silently become false.
	var wire struct {
		Target      *inboxcontrol.Target `json:"target"`
		MessageID   *int64               `json:"message_id"`
		Text        *string              `json:"text"`
		Truncated   *bool                `json:"truncated"`
		Unavailable *bool                `json:"unavailable"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Target == nil || wire.MessageID == nil || wire.Text == nil || wire.Truncated == nil || wire.Unavailable == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	messageID := request.Target.ItemID
	if request.Target.Scope == inboxcontrol.ScopeChat {
		messageID = request.MessageID
	}
	limit := request.MaxBytes
	if limit == 0 {
		limit = inboxcontrol.DefaultContextBytes
	}
	if *wire.Target != request.Target || *wire.MessageID != messageID || len(*wire.Text) > limit || !utf8.ValidString(*wire.Text) || (*wire.Unavailable && (*wire.Text != "" || *wire.Truncated)) {
		return nil, inboxcontrol.ErrUnavailable
	}
	return &inboxcontrol.Context{Target: *wire.Target, MessageID: *wire.MessageID, Text: *wire.Text, Truncated: *wire.Truncated, Unavailable: *wire.Unavailable}, nil
}
