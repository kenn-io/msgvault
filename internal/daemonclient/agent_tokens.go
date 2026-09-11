package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// AgentTokenSourceView is a source reference inside an agent token.
type AgentTokenSourceView struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

// AgentTokenView is the public metadata view of an agent grant (never contains the secret).
type AgentTokenView struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Permissions []string               `json:"permissions"`
	Sources     []AgentTokenSourceView `json:"sources"`
	CreatedAt   time.Time              `json:"created_at"`
}

// AgentTokenIssueResult holds the one-time result of issuing a new agent grant.
type AgentTokenIssueResult struct {
	AgentTokenView

	Secret    string `json:"secret"`
	DaemonURL string `json:"daemon_url"`
}

// agentTokenIssueBody is the JSON request body for POST /api/v1/agent-tokens.
type agentTokenIssueBody struct {
	Label       string   `json:"label"`
	Permissions []string `json:"permissions"`
	SourceIDs   []int64  `json:"source_ids"`
}

// agentTokenOptions implements runtime.RequestOptions for the agent-token endpoints.
type agentTokenOptions struct {
	body any
}

func (o *agentTokenOptions) GetPathParams() (map[string]any, error) { return map[string]any{}, nil }
func (o *agentTokenOptions) GetQuery() (map[string]any, error)      { return map[string]any{}, nil }
func (o *agentTokenOptions) GetBody() any                           { return o.body }
func (o *agentTokenOptions) GetHeader() (map[string]string, error) {
	return map[string]string{}, nil
}

// doAgentTokenRequest performs a raw HTTP request to the agent-token API and
// returns the response body bytes and status code. Auth headers are applied by the
// daemonclient's standard request editor. It routes through the same
// operationBusyWaiter seam as all generated-client calls, so a 503
// operation_in_progress response triggers the notify-and-retry loop instead of
// returning a hard error to the caller.
func (c *Client) doAgentTokenRequest(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	waiter := &operationBusyWaiter{c: c}
	for {
		resp, err := c.DoGeneratedRequestWithContext(ctx, method, path, &agentTokenOptions{body: body})
		if err != nil {
			return nil, 0, err
		}
		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, resp.StatusCode, readErr
		}
		if busyErr := operationInProgressFromBody(raw); busyErr != nil {
			waitCtx := c.requestContext()
			if waiter.wait(waitCtx, busyErr) {
				continue
			}
			if ctxErr := waitCtx.Err(); ctxErr != nil {
				return nil, resp.StatusCode, ctxErr
			}
		}
		return raw, resp.StatusCode, nil
	}
}

// errorResponseFromBytes wraps pre-read bytes into a minimal http.Response so
// HandleErrorResponse can decode the daemon's error body.
func errorResponseFromBytes(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func handleErrorResponseFromBytes(status int, body []byte) error {
	resp := errorResponseFromBytes(status, body)
	defer func() { _ = resp.Body.Close() }()
	return HandleErrorResponse(resp)
}

// IssueAgentToken creates a new restricted agent grant and returns its metadata
// and the one-time secret. The caller must store the secret immediately.
func (c *Client) IssueAgentToken(ctx context.Context, label string, permissions []string, sourceIDs []int64) (*AgentTokenIssueResult, error) {
	reqBody := agentTokenIssueBody{
		Label:       label,
		Permissions: permissions,
		SourceIDs:   sourceIDs,
	}
	raw, status, err := c.doAgentTokenRequest(ctx, http.MethodPost, "/api/v1/agent-tokens", reqBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, handleErrorResponseFromBytes(status, raw)
	}
	var result AgentTokenIssueResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Secret == "" {
		return nil, errors.New("issue agent token: response missing secret")
	}
	return &result, nil
}

// ListAgentTokens returns metadata for all active agent grants.
// The secret is never included in the list response.
func (c *Client) ListAgentTokens(ctx context.Context) ([]AgentTokenView, error) {
	raw, status, err := c.doAgentTokenRequest(ctx, http.MethodGet, "/api/v1/agent-tokens", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, handleErrorResponseFromBytes(status, raw)
	}
	var resp struct {
		Tokens []AgentTokenView `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Tokens, nil
}

// RevokeAgentToken deletes a grant by ID. The server returns 204 regardless of
// whether the ID existed, to prevent enumeration.
func (c *Client) RevokeAgentToken(ctx context.Context, id string) error {
	raw, status, err := c.doAgentTokenRequest(ctx, http.MethodDelete, "/api/v1/agent-tokens/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return handleErrorResponseFromBytes(status, raw)
	}
	return nil
}
