package daemonclient

import (
	"context"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MessageArchiveMinAPISchemaVersion adds archive actions and incoming sender restrictions.
const MessageArchiveMinAPISchemaVersion = "5.1.0"

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
	return decodeMessageTagResponse[emailtags.ArchiveResult](resp, "archive", func(err error) error { return archiveResponseError(change, err) })
}

func archiveResponseError(change emailtags.ArchiveChange, cause error) error {
	if !change.DryRun {
		return emailtags.Failure("remote_unknown", "archive update outcome is unknown; read current tags or retry the desired state", nil, cause)
	}
	return cause
}
