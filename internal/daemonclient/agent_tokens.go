package daemonclient

import (
	"context"
	"errors"
	"net/http"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// IssueAgentToken creates a restricted agent grant and returns its one-time secret.
func (c *Client) IssueAgentToken(ctx context.Context, label string, permissions []string, sourceIDs []int64) (*generated.AgentTokenIssueResponse, error) {
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.IssueAgentTokenResp, error) {
		return client.IssueAgentTokenWithResponse(ctx, &generated.IssueAgentTokenRequestOptions{
			Body: &generated.AgentTokenIssueRequest{Label: label, Permissions: permissions, SourceIds: sourceIDs},
		})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON201 == nil || resp.JSON201.Secret == "" {
		return nil, errors.New("issue agent token: response missing secret")
	}
	return resp.JSON201, nil
}

// ListAgentTokens returns active grant metadata without secrets.
func (c *Client) ListAgentTokens(ctx context.Context) ([]generated.AgentTokenView, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListAgentTokensResp, error) {
		return client.ListAgentTokensWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200.Tokens, nil
}

// RevokeAgentToken deletes a grant by ID, whether or not it exists.
func (c *Client) RevokeAgentToken(ctx context.Context, id string) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(client *apiclient.Client) (*generated.RevokeAgentTokenResp, error) {
		return client.RevokeAgentTokenWithResponse(ctx, &generated.RevokeAgentTokenRequestOptions{
			PathParams: &generated.RevokeAgentTokenPath{ID: id},
		})
	})
	return err
}
