package taskclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	kata "go.kenn.io/kata/pkg/client/generated"
)

// NotifyIssue submits one native inbox request. It deliberately does not retry:
// a transport error may follow a committed request that the recipient cleared.
func (c *KataClient) NotifyIssue(ctx context.Context, project, taskID, recipient, message string) error {
	if err := validatePathSegment(taskID); err != nil {
		return fmt.Errorf("%w: invalid path identity", ErrInvalidRef)
	}
	if strings.TrimSpace(recipient) == "" || strings.TrimSpace(message) == "" {
		return ErrRequestRejected
	}
	path, err := c.issuePath(ctx, project, "/"+taskID+"/notifications")
	if err != nil {
		return err
	}
	request := kata.NotifyIssueRequestBody{Actor: new(writeActor), To: &recipient, Message: message}
	var response kata.NotifyIssueResponseBody
	return c.transport.doJSON(ctx, http.MethodPost, path, request, nil, &response, http.StatusOK)
}
