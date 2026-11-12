package daemonclient

import (
	"context"
	"errors"
	"net/http"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type FederationIdentity = generated.FederationIdentityResponse
type FederationEnrollmentResult = generated.FederationCreateResponse

func (c *Client) FederationIdentity(ctx context.Context) (*FederationIdentity, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetFederationIdentityResp, error) {
		return client.GetFederationIdentityWithResponse(ctx)
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
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.CreateFederationEnrollmentResp, error) {
		return client.CreateFederationEnrollmentWithResponse(ctx, &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{SpokeInstance: spoke, Member: member, SpokeName: &name}})
	})
	if err != nil {
		return nil, err
	}
	if resp.JSON201 == nil || resp.JSON201.Token == "" {
		return nil, errors.New("federation enrollment response is empty")
	}
	return resp.JSON201, nil
}
func (c *Client) ListFederationEnrollments(ctx context.Context) ([]generated.FederationEnrollment, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListFederationEnrollmentsResp, error) {
		return client.ListFederationEnrollmentsWithResponse(ctx)
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
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(client *apiclient.Client) (*generated.RevokeFederationEnrollmentResp, error) {
		return client.RevokeFederationEnrollmentWithResponse(ctx, &generated.RevokeFederationEnrollmentRequestOptions{PathParams: &generated.RevokeFederationEnrollmentPath{UID: uid}})
	})
	return err
}
