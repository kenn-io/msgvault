package taskclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	kata "go.kenn.io/kata/pkg/client/generated"
)

// ActionMetadataKey marks an issue with the retry identity that created it.
const ActionMetadataKey = "msgvault.action"

// AddComment appends a comment. Kata replays a repeated idempotency key with
// the same body instead of adding a second comment.
func (c *KataClient) AddComment(ctx context.Context, project, taskID, idempotencyKey, body string) error {
	if strings.TrimSpace(idempotencyKey) == "" {
		return ErrIdempotencyKeyRequired
	}
	if err := validatePathSegment(taskID); err != nil {
		return err
	}
	path, err := c.issuePath(ctx, project, "/"+taskID+"/comments")
	if err != nil {
		return err
	}
	var response kata.CommentResponseBody
	return c.transport.doJSON(ctx, http.MethodPost, path, kata.CommentRequestBody{Actor: new(writeActor), Body: body}, http.Header{"Idempotency-Key": {idempotencyKey}}, &response, http.StatusOK, http.StatusCreated)
}

// FindActionTask returns the earliest open or closed issue carrying an exact
// action marker in any project, since an issue moved after filing still
// answers a retry, and false when none does. A grant scoped to project cannot
// list the others, so a refusal there narrows the search to project.
func (c *KataClient) FindActionTask(ctx context.Context, project, action string) (KataTask, bool, error) {
	if action == "" {
		return KataTask{}, false, ErrRequestRejected
	}
	query := url.Values{"meta": {ActionMetadataKey + "=" + action}, "sort": {"oldest"}, "limit": {"1"}}
	task, found, err := c.oldestIssue(ctx, query.Encode())
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrAuthenticationRequired) {
		return task, found, err
	}
	id, err := c.resolveProject(ctx, project)
	if err != nil {
		return KataTask{}, false, err
	}
	query.Set("project_id", strconv.FormatInt(id, 10))
	return c.oldestIssue(ctx, query.Encode())
}

func (c *KataClient) oldestIssue(ctx context.Context, query string) (KataTask, bool, error) {
	var response kata.ListAllIssuesResponseBody
	if err := c.transport.doJSON(ctx, http.MethodGet, "/api/v1/issues?"+query, nil, nil, &response, http.StatusOK); err != nil {
		return KataTask{}, false, err
	}
	if len(response.Issues) == 0 {
		return KataTask{}, false, nil
	}
	issue := response.Issues[0]
	if issue.UID == "" || issue.ProjectName == "" {
		return KataTask{}, false, ErrInvalidResponse
	}
	task := taskFromKataIssue(issue.ProjectName, kata.ShowIssueOut{UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Revision: issue.Revision, Metadata: issue.Metadata, Status: issue.Status, Priority: issue.Priority, Owner: issue.Owner}, issue.Labels, issue.WebURL)
	task.QualifiedRef = issue.QualifiedID
	return task, true, nil
}
