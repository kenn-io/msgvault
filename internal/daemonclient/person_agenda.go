package daemonclient

import (
	"context"
	"errors"
	"net/http"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListPersonAgenda(ctx context.Context, personID int64) (generated.PersonAgendaResult, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListPersonAgendaResp, error) {
		return client.ListPersonAgendaWithResponse(ctx, &generated.ListPersonAgendaRequestOptions{
			PathParams: &generated.ListPersonAgendaPath{ID: personID},
		})
	})
	if err != nil {
		return generated.PersonAgendaResult{}, err
	}
	if resp.JSON200 == nil {
		return generated.PersonAgendaResult{}, errors.New("empty person agenda response")
	}
	return *resp.JSON200, nil
}

func (c *Client) CreatePersonAgendaItem(ctx context.Context, personID int64, idempotencyKey string, request generated.PersonAgendaCreateRequest) (generated.PersonAgendaItem, error) {
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.CreatePersonAgendaItemResp, error) {
		return client.CreatePersonAgendaItemWithResponse(ctx, &generated.CreatePersonAgendaItemRequestOptions{
			PathParams: &generated.CreatePersonAgendaItemPath{ID: personID}, Body: &request,
			Header: &generated.CreatePersonAgendaItemHeaders{IdempotencyKey: idempotencyKey},
		})
	})
	if err != nil {
		return generated.PersonAgendaItem{}, err
	}
	if resp.JSON201 == nil {
		return generated.PersonAgendaItem{}, errors.New("empty person agenda mutation response")
	}
	return resp.JSON201.Item, nil
}

func (c *Client) LinkPersonAgendaItem(ctx context.Context, personID int64, request generated.PersonAgendaLinkRequest) (generated.PersonAgendaItem, error) {
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.LinkPersonAgendaItemResp, error) {
		return client.LinkPersonAgendaItemWithResponse(ctx, &generated.LinkPersonAgendaItemRequestOptions{
			PathParams: &generated.LinkPersonAgendaItemPath{ID: personID}, Body: &request,
		})
	})
	if err != nil {
		return generated.PersonAgendaItem{}, err
	}
	if resp.JSON201 == nil {
		return generated.PersonAgendaItem{}, errors.New("empty person agenda mutation response")
	}
	return resp.JSON201.Item, nil
}

func (c *Client) UpdatePersonAgendaItem(ctx context.Context, personID int64, ref string, request generated.PersonAgendaUpdateRequest) (generated.PersonAgendaItem, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.UpdatePersonAgendaItemResp, error) {
		return client.UpdatePersonAgendaItemWithResponse(ctx, &generated.UpdatePersonAgendaItemRequestOptions{
			PathParams: &generated.UpdatePersonAgendaItemPath{ID: personID, Ref: ref}, Body: &request,
		})
	})
	if err != nil {
		return generated.PersonAgendaItem{}, err
	}
	if resp.JSON200 == nil {
		return generated.PersonAgendaItem{}, errors.New("empty person agenda mutation response")
	}
	return resp.JSON200.Item, nil
}

func (c *Client) UnlinkPersonAgendaItem(ctx context.Context, personID int64, ref string) (generated.PersonAgendaItem, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.UnlinkPersonAgendaItemResp, error) {
		return client.UnlinkPersonAgendaItemWithResponse(ctx, &generated.UnlinkPersonAgendaItemRequestOptions{
			PathParams: &generated.UnlinkPersonAgendaItemPath{ID: personID, Ref: ref},
		})
	})
	if err != nil {
		return generated.PersonAgendaItem{}, err
	}
	if resp.JSON200 == nil {
		return generated.PersonAgendaItem{}, errors.New("empty person agenda mutation response")
	}
	return resp.JSON200.Item, nil
}
