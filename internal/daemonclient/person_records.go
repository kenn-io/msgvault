package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ContactScopesMinAPISchemaVersion adds native delegated person operations.
const ContactScopesMinAPISchemaVersion = "3.14.0"

type PersonEditContext struct {
	Person generated.Person `json:"person"`
	ETag   string           `json:"etag"`
}

type PersonStructuredContext struct {
	Profile generated.StructuredPersonProfile `json:"profile"`
	ETag    string                            `json:"etag"`
}

type PersonAttributesContext struct {
	Attributes generated.PersonAttributesResponse `json:"attributes"`
	ETag       string                             `json:"etag"`
}

func (c *Client) GetPersonEditContext(ctx context.Context, id int64) (*PersonEditContext, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetPersonProfileResp, error) {
		return api.GetPersonProfileWithResponse(ctx, &generated.GetPersonProfileRequestOptions{PathParams: &generated.GetPersonProfilePath{ID: id}})
	})
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil || r.Headers200 == nil || r.Headers200.ETag == "" {
		return nil, errors.New("person context was incomplete")
	}
	return &PersonEditContext{Person: *r.JSON200, ETag: r.Headers200.ETag}, nil
}

func (c *Client) SetPersonDisplayName(ctx context.Context, id int64, etag string, name *string) (*generated.Person, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.PatchPersonResp, error) {
		return api.PatchPersonWithResponse(ctx, &generated.PatchPersonRequestOptions{PathParams: &generated.PatchPersonPath{ID: id}, Header: &generated.PatchPersonHeaders{IfMatch: etag}, Body: &generated.PatchPersonBody{DisplayName: name}})
	})
	if err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *Client) GetPersonStructuredContext(ctx context.Context, id int64) (*PersonStructuredContext, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetPersonStructuredProfileResp, error) {
		return api.GetPersonStructuredProfileWithResponse(ctx, &generated.GetPersonStructuredProfileRequestOptions{PathParams: &generated.GetPersonStructuredProfilePath{ID: id}})
	})
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil || r.Headers200 == nil || r.Headers200.ETag == "" {
		return nil, errors.New("structured person context was incomplete")
	}
	return &PersonStructuredContext{Profile: *r.JSON200, ETag: r.Headers200.ETag}, nil
}

func (c *Client) PatchPersonProfile(ctx context.Context, id int64, etag string, patch generated.PersonProfilePatchRequest) (*generated.StructuredPersonProfile, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.PatchPersonStructuredProfileResp, error) {
		return api.PatchPersonStructuredProfileWithResponse(ctx, &generated.PatchPersonStructuredProfileRequestOptions{PathParams: &generated.PatchPersonStructuredProfilePath{ID: id}, Header: &generated.PatchPersonStructuredProfileHeaders{IfMatch: etag}, Body: &patch})
	})
	if err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *Client) GetPersonAttributesContext(ctx context.Context, id int64) (*PersonAttributesContext, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListPersonAttributesResp, error) {
		return api.ListPersonAttributesWithResponse(ctx, &generated.ListPersonAttributesRequestOptions{PathParams: &generated.ListPersonAttributesPath{ID: id}})
	})
	if err != nil {
		return nil, err
	}
	if r.JSON200 == nil || r.Headers200 == nil || r.Headers200.ETag == "" {
		return nil, errors.New("person attribute context was incomplete")
	}
	return &PersonAttributesContext{Attributes: *r.JSON200, ETag: r.Headers200.ETag}, nil
}

func (c *Client) SetPersonAttribute(ctx context.Context, id int64, slug, etag string, body generated.SetPersonAttributeRequest, dryRun bool) (*generated.PersonAttributeWrite, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.SetPersonAttributeResp, error) {
		return api.SetPersonAttributeWithResponse(ctx, &generated.SetPersonAttributeRequestOptions{PathParams: &generated.SetPersonAttributePath{ID: id, Slug: slug}, Header: &generated.SetPersonAttributeHeaders{IfMatch: &etag}, Query: &generated.SetPersonAttributeQuery{DryRun: &dryRun}, Body: &body})
	})
	if err != nil {
		return nil, err
	}
	return r.JSON200, nil
}

func (c *Client) ClearPersonAttribute(ctx context.Context, id int64, slug, etag string, ordinal *int64, expected int64, dryRun bool) (*generated.PersonAttributeWrite, error) {
	r, err := APIResponse(c, func(api *apiclient.Client) (*generated.ClearPersonAttributeResp, error) {
		return api.ClearPersonAttributeWithResponse(ctx, &generated.ClearPersonAttributeRequestOptions{PathParams: &generated.ClearPersonAttributePath{ID: id, Slug: slug}, Header: &generated.ClearPersonAttributeHeaders{IfMatch: &etag}, Query: &generated.ClearPersonAttributeQuery{Ordinal: ordinal, ExpectedValueID: &expected, DryRun: &dryRun}})
	})
	if err != nil {
		return nil, err
	}
	return r.JSON200, nil
}
