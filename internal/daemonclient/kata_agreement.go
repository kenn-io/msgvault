package daemonclient

import (
	"context"
	"errors"
	"net/url"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const KataAgreementMinAPISchemaVersion = "3.45.0"

func (c *Client) GetKataAgreement(ctx context.Context, ref string) (generated.KataAgreementResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetKataAgreementResp, error) {
		return client.GetKataAgreementWithResponse(ctx, &generated.GetKataAgreementRequestOptions{PathParams: &generated.GetKataAgreementPath{Ref: url.PathEscape(ref)}})
	})
	if err != nil {
		return generated.KataAgreementResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.KataAgreementResponse{}, errors.New("empty Kata agreement response")
	}
	return *response.JSON200, nil
}

func (c *Client) RecordKataAgreement(ctx context.Context, ref string, input generated.KataAgreementRecordRequest) (generated.KataAgreementResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.RecordKataAgreementResp, error) {
		return client.RecordKataAgreementWithResponse(ctx, &generated.RecordKataAgreementRequestOptions{PathParams: &generated.RecordKataAgreementPath{Ref: url.PathEscape(ref)}, Body: &input})
	})
	if err != nil {
		return generated.KataAgreementResponse{}, err
	}
	if response.JSON200 == nil {
		return generated.KataAgreementResponse{}, errors.New("empty Kata agreement response")
	}
	return *response.JSON200, nil
}
