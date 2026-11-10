package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Tag errors carry before/after snapshots and IMAP flags, not just an error message.
const maxMessageTagErrorBodyBytes = 1 << 20

// MessageTags never retries a provider mutation after a transport error.
func (c *Client) MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error) {
	method := http.MethodGet
	path := fmt.Sprintf("/api/v1/messages/%d/tags", id)
	var options runtime.RequestOptions = &generated.GetMessageTagsRequestOptions{Query: &generated.GetMessageTagsQuery{Mailbox: optionalString(mailbox)}}
	if change != nil {
		method = http.MethodPost
		options = &generated.UpdateMessageTagsRequestOptions{Body: &generated.UpdateMessageTagsBody{Add: change.Add, Remove: change.Remove, Mailbox: optionalString(change.Mailbox), DryRun: &change.DryRun}}
	}
	resp, err := c.doGeneratedRequestWithHTTPClient(ctx, method, path, options, c.httpClient, maxMessageTagErrorBodyBytes)
	if err != nil {
		return nil, messageTagResponseError(change, err)
	}
	result, err := decodeMessageTagResponse[emailtags.MessageTagResult](resp, "message tags", func(err error) error { return messageTagResponseError(change, err) })
	if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
		return failure.Result, err
	}
	return result, err
}

// decodeMessageTagResponse handles the shared tag/archive failure envelope.
// The request layer bounds error bodies; only unstructured failures are uncertain.
func decodeMessageTagResponse[T any](resp *http.Response, operation string, responseError func(error) error) (*T, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, responseError(fmt.Errorf("read %s response: %w", operation, err))
	}
	if resp.StatusCode != http.StatusOK {
		var failure emailtags.MessageTagError
		if err := json.Unmarshal(body, &failure); err != nil || failure.Code == "" {
			return nil, responseError(fmt.Errorf("%s request failed (HTTP %d)", operation, resp.StatusCode))
		}
		return nil, &failure
	}
	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, responseError(fmt.Errorf("decode %s response: %w", operation, err))
	}
	return &result, nil
}

func messageTagResponseError(change *emailtags.MessageTagChange, cause error) error {
	return messageWriteResponseError(change != nil && !change.DryRun, "tag update outcome is unknown; read current tags before retrying", cause)
}

func messageWriteResponseError(write bool, message string, cause error) error {
	if write {
		return emailtags.Failure("remote_unknown", message, nil, cause)
	}
	return cause
}
