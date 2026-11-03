package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// RunCommentAction makes one request. A lost response must be inspected before retrying.
func (c *Client) RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest) (*meta.ActionResult, error) {
	body := generated.RunCommentActionBody{Action: generated.CommentActionBodyAction(req.Action), Body: optionalString(req.Body), Confirm: optionalString(req.Confirm), DryRun: &req.DryRun}
	if req.ResolveUnknown != "" {
		v := generated.CommentActionBodyResolveUnknown(req.ResolveUnknown)
		body.ResolveUnknown = &v
	}
	var result meta.ActionResult
	err := c.commentRequest(ctx, http.MethodPost, fmt.Sprintf("/api/v1/messages/%d/comment-actions", id), &generated.RunCommentActionRequestOptions{Body: &body}, !req.DryRun, &result)
	if err != nil {
		if failure, ok := errors.AsType[*meta.ActionError](err); ok {
			return failure.Result, err
		}
		return nil, err
	}
	return &result, nil
}
func (c *Client) ListCommentActions(ctx context.Context, id int64) ([]meta.Receipt, error) {
	var result []meta.Receipt
	err := c.commentRequest(ctx, http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/comment-actions", id), &generated.ListCommentActionsRequestOptions{}, false, &result)
	return result, err
}
func (c *Client) UpdateCommentTriage(ctx context.Context, id int64, state, expect string) (string, error) {
	body := generated.UpdateCommentTriageBody{State: generated.CommentTriageBodyState(state)}
	if expect != "" {
		v := generated.CommentTriageBodyExpect(expect)
		body.Expect = &v
	}
	var result struct {
		State string `json:"state"`
	}
	err := c.commentRequest(ctx, http.MethodPut, fmt.Sprintf("/api/v1/messages/%d/comment-triage", id), &generated.UpdateCommentTriageRequestOptions{Body: &body}, true, &result)
	return result.State, err
}
func (c *Client) ListCommentQueue(ctx context.Context, f meta.CommentQueueRequest) (*meta.CommentQueueResponse, error) {
	limit := int64(f.Limit)
	options := &generated.ListCommentQueueRequestOptions{Query: &generated.ListCommentQueueQuery{Source: optionalString(f.Source), State: optionalString(f.State), BeforeID: &f.BeforeID}}
	if limit != 0 {
		options.Query.Limit = &limit
	}
	var result meta.CommentQueueResponse
	if err := c.commentRequest(ctx, http.MethodGet, "/api/v1/comment-queue", options, false, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
func (c *Client) commentRequest(ctx context.Context, method, path string, options runtime.RequestOptions, mutation bool, result any) error {
	uncertain := func(cause error) error {
		if mutation {
			return &meta.ActionError{Code: "outcome_unknown", Message: "Comment update outcome is unknown; inspect receipts and the provider before resolving", Cause: cause}
		}
		return cause
	}
	resp, err := c.doGeneratedRequestWithHTTPClient(ctx, method, path, options, c.httpClient, 1<<20)
	if err != nil {
		return uncertain(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return uncertain(fmt.Errorf("read comment response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		var failure meta.ActionError
		if err := json.Unmarshal(body, &failure); err == nil && failure.Code != "" {
			return &failure
		}
		// Standard daemon refusals happen before action dispatch. Preserve
		// their typed codes; malformed responses remain uncertain.
		var refusal apiErrorBody
		if json.Unmarshal(body, &refusal) == nil && refusal.Error != "" && refusal.Message != "" {
			return handleErrorBody(resp.StatusCode, body)
		}
		return uncertain(fmt.Errorf("comment request failed (HTTP %d)", resp.StatusCode))
	}
	if err := json.Unmarshal(body, result); err != nil {
		return uncertain(fmt.Errorf("decode comment response: %w", err))
	}
	return nil
}
