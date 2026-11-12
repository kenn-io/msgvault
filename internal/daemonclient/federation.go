package daemonclient

import (
	"context"
	"errors"
	"net/http"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type FederationIdentity = generated.FederationIdentityPending
type FederationEnrollmentResult struct {
	Enrollment  generated.FederationEnrollmentViewPending `json:"enrollment"`
	Token       string                                    `json:"token"`
	HubInstance string                                    `json:"hub_instance"`
	HubURL      string                                    `json:"hub_url,omitempty"`
}

func (c *Client) FederationIdentity(ctx context.Context) (*FederationIdentity, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.FederationResponsePending[generated.FederationIdentityPending], error) {
		return client.GetFederationIdentityPending(ctx)
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("federation identity response is empty")
	}
	return resp.JSON200, nil
}
func (c *Client) CreateFederationEnrollment(ctx context.Context, spoke, member, name string) (*FederationEnrollmentResult, error) {
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.FederationResponsePending[generated.FederationEnrollmentPending], error) {
		return client.CreateFederationEnrollmentPending(ctx, generated.FederationCreatePending{SpokeInstance: spoke, Member: member, SpokeName: name})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON201 == nil || resp.JSON201.Token == "" {
		return nil, errors.New("federation enrollment response is empty")
	}
	e := resp.JSON201
	return &FederationEnrollmentResult{Enrollment: e.Enrollment, Token: e.Token, HubInstance: e.HubInstance}, nil
}
func (c *Client) ListFederationEnrollments(ctx context.Context) ([]generated.FederationEnrollmentViewPending, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.FederationResponsePending[generated.FederationListPending], error) {
		return client.ListFederationEnrollmentsPending(ctx)
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("federation list response is empty")
	}
	return resp.JSON200.Enrollments, nil
}
func (c *Client) RevokeFederationEnrollment(ctx context.Context, uid string) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(client *apiclient.Client) (*generated.FederationResponsePending[struct{}], error) {
		return client.RevokeFederationEnrollmentPending(ctx, uid)
	})
	return err
}
