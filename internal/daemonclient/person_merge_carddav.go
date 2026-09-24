package daemonclient

import (
	"context"
	"errors"
	"fmt"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonMergeContext contains the two exact profile revisions required by the
// merge route. A caller must use these ETags without rewriting them.
type PersonMergeContext struct {
	Survivor     generated.Person `json:"survivor"`
	SurvivorETag string           `json:"survivor_etag"`
	Absorbed     generated.Person `json:"absorbed"`
	AbsorbedETag string           `json:"absorbed_etag"`
}

func (c *Client) GetPersonMergeContext(ctx context.Context, survivorID, absorbedID int64) (*PersonMergeContext, error) {
	read := func(id int64) (*generated.GetPersonProfileResp, error) {
		return APIResponse(c, func(api *apiclient.Client) (*generated.GetPersonProfileResp, error) {
			return api.GetPersonProfileWithResponse(ctx, &generated.GetPersonProfileRequestOptions{PathParams: &generated.GetPersonProfilePath{ID: id}})
		})
	}
	survivor, err := read(survivorID)
	if err != nil {
		return nil, err
	}
	absorbed, err := read(absorbedID)
	if err != nil {
		return nil, err
	}
	if survivor.JSON200 == nil || survivor.Headers200 == nil || absorbed.JSON200 == nil || absorbed.Headers200 == nil {
		return nil, errors.New("person merge context response was incomplete")
	}
	return &PersonMergeContext{Survivor: *survivor.JSON200, SurvivorETag: survivor.Headers200.ETag,
		Absorbed: *absorbed.JSON200, AbsorbedETag: absorbed.Headers200.ETag}, nil
}

func (c *Client) MergePerson(ctx context.Context, survivorID, absorbedID int64, survivorETag, absorbedETag, key string) (*generated.PersonMergeResult, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.MergePersonsResp, error) {
		return api.MergePersonsWithResponse(ctx, &generated.MergePersonsRequestOptions{
			PathParams: &generated.MergePersonsPath{ID: survivorID},
			Body:       &generated.MergePersonsBody{AbsorbedPersonID: absorbedID},
			Header:     &generated.MergePersonsHeaders{IfMatch: survivorETag + ", " + absorbedETag, IdempotencyKey: key},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("person merge response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) GetCardDAVPublication(ctx context.Context, id int64) (*generated.CardDAVPublicationResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetCardDAVPublicationResp, error) {
		return api.GetCardDAVPublicationWithResponse(ctx, &generated.GetCardDAVPublicationRequestOptions{PathParams: &generated.GetCardDAVPublicationPath{PersonID: id}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("CardDAV publication response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) PreviewCardDAVPublication(ctx context.Context, id int64) (*generated.CardDAVPublicationPreviewResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.PreviewCardDAVPublicationResp, error) {
		return api.PreviewCardDAVPublicationWithResponse(ctx, &generated.PreviewCardDAVPublicationRequestOptions{PathParams: &generated.PreviewCardDAVPublicationPath{PersonID: id}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("CardDAV preview response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) ApproveCardDAVPublication(ctx context.Context, id int64, token string) (*generated.CardDAVPublicationResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ApproveCardDAVPublicationResp, error) {
		return api.ApproveCardDAVPublicationWithResponse(ctx, &generated.ApproveCardDAVPublicationRequestOptions{
			PathParams: &generated.ApproveCardDAVPublicationPath{PersonID: id},
			Body:       &generated.ApproveCardDAVPublicationBody{ApprovalToken: token},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("CardDAV approval response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) SyncCardDAV(ctx context.Context, full bool) (*generated.SyncResult, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.SyncCardDAVResp, error) {
		return api.SyncCardDAVWithResponse(ctx, &generated.SyncCardDAVRequestOptions{Body: &generated.SyncCardDAVBody{Full: &full}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("CardDAV sync response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) GetCardDAVSyncStatus(ctx context.Context) (*generated.CardDAVStatusResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetCardDAVStatusResp, error) {
		return api.GetCardDAVStatusWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("CardDAV status response was empty")
	}
	return response.JSON200, nil
}

// SafeMCPError keeps daemon-provided prose out of MCP tool errors: upstream
// failure details can include private contact data. The stable code and HTTP
// status still identify conflicts and blockers.
func SafeMCPError(err error) error {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		switch apiErr.Code {
		case "person_merge_revision_conflict", "person_merge_idempotency_conflict",
			"person_carddav_published", "person_merge_required",
			"carddav_review_stale", "carddav_inference_review_required",
			"consent_required", "credential_unavailable", "scoring_disabled",
			"automatic_match_evaluation_required":
			return fmt.Errorf("daemon request failed (%d, %s)", apiErr.Status, apiErr.Code)
		default:
			return fmt.Errorf("daemon request failed (%d)", apiErr.Status)
		}
	}
	return errors.New("daemon request failed")
}
