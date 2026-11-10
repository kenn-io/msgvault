package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MessageArchiveMinAPISchemaVersion adds archive actions and incoming sender restrictions.
const MessageArchiveMinAPISchemaVersion = "4.1.0"

// ArchiveMessage sets the desired state without retrying an uncertain write.
func (c *Client) ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange) (*emailtags.ArchiveResult, error) {
	if change.Archived == nil {
		return nil, emailtags.Failure("invalid_request", "archived must be true or false", nil, nil)
	}
	options := &generated.ArchiveMessageRequestOptions{Body: &generated.ArchiveMessageBody{Archived: *change.Archived, DryRun: &change.DryRun}}
	resp, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodPost, fmt.Sprintf("/api/v1/messages/%d/archive", id), options, c.httpClient, maxMessageTagErrorBodyBytes)
	if err != nil {
		return nil, archiveResponseError(change, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, archiveResponseError(change, fmt.Errorf("read archive response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		var failure emailtags.MessageTagError
		if err := json.Unmarshal(body, &failure); err != nil || failure.Code == "" {
			return nil, archiveResponseError(change, fmt.Errorf("archive request failed (HTTP %d)", resp.StatusCode))
		}
		return nil, &failure
	}
	var result emailtags.ArchiveResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, archiveResponseError(change, fmt.Errorf("decode archive response: %w", err))
	}
	return &result, nil
}

func archiveResponseError(change emailtags.ArchiveChange, cause error) error {
	return messageWriteResponseError(!change.DryRun, "archive update outcome is unknown; read current tags or retry the desired state", cause)
}
