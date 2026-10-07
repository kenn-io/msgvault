package taskclient

import (
	"context"
	"errors"
	"fmt"
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
		return fmt.Errorf("%w: invalid path identity", ErrInvalidRef)
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
// answers a retry, and false when none does.
func (c *KataClient) FindActionTask(ctx context.Context, project, action string) (KataTask, bool, error) {
	if action == "" {
		return KataTask{}, false, ErrRequestRejected
	}
	tasks, err := c.listIssues(ctx, project, url.Values{"meta": {ActionMetadataKey + "=" + action}}, 1)
	if err != nil || len(tasks) == 0 {
		return KataTask{}, false, err
	}
	return tasks[0], true, nil
}

// FindMetadataTasks returns up to limit open or closed issues carrying the
// metadata key in any project, oldest first.
func (c *KataClient) FindMetadataTasks(ctx context.Context, project, key string, limit int) ([]KataTask, error) {
	return c.listIssues(ctx, project, url.Values{"meta": {key}}, limit)
}

// listIssues lists matching issues across projects, oldest first. A grant
// scoped to project cannot list the others, so a refusal there narrows the
// search to project.
func (c *KataClient) listIssues(ctx context.Context, project string, query url.Values, limit int) ([]KataTask, error) {
	query.Set("sort", "oldest")
	query.Set("limit", strconv.Itoa(limit))
	tasks, err := c.listAllIssues(ctx, query.Encode())
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrAuthenticationRequired) {
		return tasks, err
	}
	id, err := c.resolveProject(ctx, project)
	if err != nil {
		return nil, err
	}
	query.Set("project_id", strconv.FormatInt(id, 10))
	return c.listAllIssues(ctx, query.Encode())
}

func (c *KataClient) listAllIssues(ctx context.Context, query string) ([]KataTask, error) {
	transport := *c.transport
	transport.maxResponseBytes = max(transport.maxResponseBytes, issueListMaxResponseBytes)
	var response kata.ListAllIssuesResponseBody
	if err := transport.doJSON(ctx, http.MethodGet, "/api/v1/issues?"+query, nil, nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	tasks := make([]KataTask, 0, len(response.Issues))
	for _, issue := range response.Issues {
		if issue.UID == "" || issue.ProjectName == "" {
			return nil, ErrInvalidResponse
		}
		task := taskFromKataIssue(issue.ProjectName, kata.ShowIssueOut{UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Revision: issue.Revision, Metadata: issue.Metadata, Status: issue.Status, Priority: issue.Priority, Owner: issue.Owner}, issue.Labels, issue.WebURL)
		task.QualifiedRef, task.CreatedAt, task.IssueID = issue.QualifiedID, issue.CreatedAt, issue.ID
		tasks = append(tasks, task)
	}
	return tasks, nil
}
