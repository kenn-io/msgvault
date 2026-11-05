package taskclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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
	tasks, err := c.listAcrossProjects(ctx, project, url.Values{"meta": {ActionMetadataKey + "=" + action}}, 1)
	if err != nil || len(tasks) == 0 {
		return KataTask{}, false, err
	}
	return tasks[0], true, nil
}

// FindMetadataTasks returns up to limit open or closed issues carrying the
// metadata key in any project, newest first.
func (c *KataClient) FindMetadataTasks(ctx context.Context, project, key string, limit int) ([]KataTask, error) {
	return c.listAcrossProjects(ctx, project, url.Values{"meta": {key}, "sort": {""}}, limit)
}

// listAcrossProjects lists matching issues across projects, oldest first by default. A grant
// scoped to project cannot list the others, so a refusal there narrows the
// search to project.
func (c *KataClient) listAcrossProjects(ctx context.Context, project string, query url.Values, limit int) ([]KataTask, error) {
	if !query.Has("sort") {
		query.Set("sort", "oldest")
	}
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
		task.QualifiedRef = issue.QualifiedID
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// OpenIssues lists the project's most recently updated open issues without
// their bodies. Every Kata API msgvault accepts orders an unsorted list this
// way, while sort=created needs a newer one.
func (c *KataClient) OpenIssues(ctx context.Context, project string, limit int) ([]KataTask, bool, error) {
	return c.listIssueSummaries(ctx, project, url.Values{"status": {"open"}, "limit": {strconv.Itoa(limit)}})
}

// listIssueSummaries streams an issue list and drops each body as it arrives,
// so the response bound counts everything but bodies and the list's cost
// does not grow with them.
func (c *KataClient) listIssueSummaries(ctx context.Context, project string, query url.Values) ([]KataTask, bool, error) {
	path, err := c.issuePath(ctx, project, "?"+query.Encode())
	if err != nil {
		return nil, false, err
	}
	resp, err := c.transport.send(ctx, http.MethodGet, path, nil, nil, http.StatusOK)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	reader := &bodyDroppingReader{reader: resp.Body, maximum: max(c.transport.maxResponseBytes, issueListMaxResponseBytes)}
	var response struct {
		kata.ListIssuesResponseBody `json:",inline"`
		Issues                      []issueSummary `json:"issues"`
	}
	if err := decodeResponse(reader, &response, json.WithUnmarshalers(json.UnmarshalFromFunc(reader.drop))); err != nil {
		return nil, false, err
	}
	issues := make([]kata.IssueOut, 0, len(response.Issues))
	for _, issue := range response.Issues {
		issues = append(issues, issue.IssueOut)
	}
	return tasksFromKataIssues(project, issues), response.Truncated, nil
}

// issueSummary is a listed issue whose body was dropped unread.
type issueSummary struct {
	kata.IssueOut `json:",inline"`
	Body          droppedBody `json:"body"`
}

type droppedBody struct{}

// bodyDroppingReader bounds the bytes it keeps, and separately the bytes of
// any one dropped body, at maximum.
type bodyDroppingReader struct {
	reader        io.Reader
	maximum       int64
	kept, dropped int64
	dropping      bool
}

func (r *bodyDroppingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.dropping {
		r.dropped += int64(n)
	} else {
		r.kept += int64(n)
	}
	if r.kept > r.maximum || r.dropped > r.maximum {
		return n, ErrResponseTooLarge
	}
	return n, err
}

func (r *bodyDroppingReader) drop(decoder *jsontext.Decoder, _ *droppedBody) error {
	r.dropping, r.dropped = true, 0
	defer func() { r.dropping = false }()
	if decoder.PeekKind() != '"' {
		return ErrInvalidResponse
	}
	return decoder.SkipValue()
}
