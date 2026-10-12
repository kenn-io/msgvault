package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) PrepareKataEvidence(ctx context.Context, request generated.KataEvidencePrepareRequest) (generated.KataEvidencePrepareResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.PrepareKataEvidenceResp, error) {
		return client.PrepareKataEvidenceWithResponse(ctx, &generated.PrepareKataEvidenceRequestOptions{Body: &request})
	})
	if err != nil {
		return generated.KataEvidencePrepareResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.KataEvidencePrepareResponse{}, errors.New("empty Kata evidence response")
	}
	return *resp.JSON200, nil
}

// KataIssueConflictError reports that an Idempotency-Key already filed Issue
// with different details.
type KataIssueConflictError struct {
	Issue generated.KataIssueReceipt
}

func (e *KataIssueConflictError) Error() string {
	return fmt.Sprintf("idempotency_conflict: this key already filed %s (%s) with different details", e.Issue.QualifiedRef, e.Issue.Status)
}

func (c *Client) CreateKataIssue(ctx context.Context, idempotencyKey string, request generated.KataIssueCreateRequest) (generated.KataIssueResponse, error) {
	resp, err := generatedResponse(c, func(client *apiclient.Client) (*generated.CreateKataIssueResp, error) {
		return client.CreateKataIssueWithResponse(ctx, &generated.CreateKataIssueRequestOptions{
			Body: &request, Header: &generated.CreateKataIssueHeaders{IdempotencyKey: idempotencyKey},
		})
	}, func(resp any, err error) error {
		return responseError(resp, err, []int{http.StatusCreated}, kataIssueErrorBody)
	})
	if err != nil {
		return generated.KataIssueResponse{}, err
	}
	if resp.JSON201 == nil {
		return generated.KataIssueResponse{}, errors.New("empty Kata issue response")
	}
	return *resp.JSON201, nil
}

// kataIssueErrorBody keeps the issue a 409 names, since the caller acts on it.
func kataIssueErrorBody(status int, body []byte) error {
	var conflict generated.KataIssueConflictResponse
	if status == http.StatusConflict && json.Unmarshal(body, &conflict) == nil && conflict.Issue != nil {
		return &KataIssueConflictError{Issue: *conflict.Issue}
	}
	return handleErrorBody(status, body)
}

func (c *Client) LinkKataEvidence(ctx context.Context, ref string, request generated.KataEvidenceLinkRequest) (generated.KataIssueResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.LinkKataEvidenceResp, error) {
		return client.LinkKataEvidenceWithResponse(ctx, &generated.LinkKataEvidenceRequestOptions{
			PathParams: &generated.LinkKataEvidencePath{Ref: url.PathEscape(ref)}, Body: &request,
		})
	})
	if err != nil {
		return generated.KataIssueResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.KataIssueResponse{}, errors.New("empty Kata issue response")
	}
	return *resp.JSON200, nil
}

func (c *Client) FindKataIssues(ctx context.Context, query generated.FindKataIssuesQuery) (generated.KataIssueListResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.FindKataIssuesResp, error) {
		return client.FindKataIssuesWithResponse(ctx, &generated.FindKataIssuesRequestOptions{Query: &query})
	})
	if err != nil {
		return generated.KataIssueListResponse{}, err
	}
	return *resp.JSON200, nil
}

func (c *Client) GetKataIssueContext(ctx context.Context, ref string, query generated.GetKataIssueContextQuery) (generated.KataIssueContextResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetKataIssueContextResp, error) {
		return client.GetKataIssueContextWithResponse(ctx, &generated.GetKataIssueContextRequestOptions{
			PathParams: &generated.GetKataIssueContextPath{Ref: url.PathEscape(ref)}, Query: &query,
		})
	})
	if err != nil {
		return generated.KataIssueContextResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.KataIssueContextResponse{}, errors.New("empty Kata issue context response")
	}
	return *resp.JSON200, nil
}
