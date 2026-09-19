package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const agentTokenSenderMinAPISchemaVersion = "2.27.0"

// IssueAgentToken creates a restricted agent grant and returns its one-time secret.
func (c *Client) IssueAgentToken(
	ctx context.Context,
	label string,
	permissions []string,
	sourceIDs []int64,
	senderSelections ...map[int64][]string,
) (*generated.AgentTokenIssueResponse, error) {
	var selections map[int64][]string
	if len(senderSelections) > 0 {
		selections = senderSelections[0]
	}
	if selections != nil {
		compatible, err := c.SupportsAPISchemaVersion(ctx, agentTokenSenderMinAPISchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("check agent-token sender capability: %w", err)
		}
		if !compatible {
			version, _ := c.APISchemaVersion(ctx)
			return nil, fmt.Errorf("agent-token sender selection requires daemon API schema %s or newer (daemon reports %q)", agentTokenSenderMinAPISchemaVersion, version)
		}
	}
	encodedSelections := make(map[string][]string, len(selections))
	for sourceID, values := range selections {
		encodedSelections[strconv.FormatInt(sourceID, 10)] = append([]string(nil), values...)
	}
	_ = encodedSelections
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.IssueAgentTokenResp, error) {
		return client.IssueAgentTokenWithResponse(ctx, &generated.IssueAgentTokenRequestOptions{
			Body: &generated.IssueAgentTokenBody{
				Label: label, Permissions: permissions, SourceIds: sourceIDs,
				SenderSelections: encodedSelections,
			},
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
