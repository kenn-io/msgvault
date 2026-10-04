package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MessageTags never retries a provider mutation after a transport error.
func (c *Client) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	if id <= 0 {
		return nil, emailtags.Failure("invalid_request", "message ID must be positive", nil, nil)
	}
	method := http.MethodGet
	path := fmt.Sprintf("/api/v1/messages/%d/tags", id)
	var options runtime.RequestOptions = &generated.GetMessageTagsRequestOptions{Query: &generated.GetMessageTagsQuery{Mailbox: optionalString(mailbox)}}
	if change != nil {
		method = http.MethodPost
		options = &generated.UpdateMessageTagsRequestOptions{Body: &generated.UpdateMessageTagsBody{Add: change.Add, Remove: change.Remove, Mailbox: optionalString(change.Mailbox), DryRun: &change.DryRun}}
	}
	resp, err := c.DoGeneratedRequestWithContext(ctx, method, path, options)
	if err != nil {
		return nil, messageTagResponseError(change, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, messageTagResponseError(change, fmt.Errorf("read message tags: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		var failure emailtags.Error
		if err := json.Unmarshal(body, &failure); err != nil || failure.Code == "" {
			return nil, messageTagResponseError(change, fmt.Errorf("message tags request failed (HTTP %d)", resp.StatusCode))
		}
		return failure.Result, &failure
	}
	var result emailtags.Result
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, messageTagResponseError(change, fmt.Errorf("decode message tags: %w", err))
	}
	return &result, nil
}

func messageTagResponseError(change *emailtags.Change, cause error) error {
	if change != nil && !change.DryRun {
		return emailtags.Failure("remote_unknown", "tag update outcome is unknown; read current tags before retrying", nil, cause)
	}
	return cause
}
