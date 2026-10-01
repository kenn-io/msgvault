package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

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
		// Keep the default status route stable across the generated client's
		// optional connection selector. APIResponse retains admission, native
		// busy handling and error decoding for both schema versions.
		native := api.APIClient()
		const path = "/api/v1/carddav/status"
		req, requestErr := native.CreateRequest(ctx, runtime.RequestOptionsParameters{
			RequestURL: native.GetBaseURL() + path, Method: http.MethodGet,
		})
		if requestErr != nil {
			return nil, fmt.Errorf("create CardDAV status request: %w", requestErr)
		}
		resp, requestErr := native.ExecuteRequest(ctx, req, path)
		if requestErr != nil {
			return nil, fmt.Errorf("execute CardDAV status request: %w", requestErr)
		}
		out := &generated.GetCardDAVStatusResp{HTTPResponse: resp.Raw, Body: resp.Content, StatusCode: resp.StatusCode}
		if resp.StatusCode != http.StatusOK {
			return out, runtime.NewClientAPIError(fmt.Errorf("CardDAV status failed (%d)", resp.StatusCode), runtime.WithStatusCode(resp.StatusCode))
		}
		out.JSON200 = new(generated.GetCardDAVStatusResponse)
		if len(resp.Content) > 0 {
			if decodeErr := json.Unmarshal(resp.Content, out.JSON200); decodeErr != nil {
				return out, &runtime.ResponseDecodeError{
					StatusCode: resp.StatusCode, ContentType: resp.Headers.Get("Content-Type"),
					ContentLength: len(resp.Content), TargetType: "GetCardDAVStatusResponse",
					Body: resp.Content, Err: decodeErr,
				}
			}
		}
		return out, nil
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
			"invalid_limit", "identity_match_state_unavailable", "identity_match_not_found",
			"identity_match_review_stale", "identity_match_not_acceptable",
			"identity_match_already_accepted", "identity_match_already_applied",
			"identity_match_state_changed", "identity_match_endpoint_unsupported",
			"identity_match_failed", "person_binding_conflict":
			return fmt.Errorf("daemon request failed (%d, %s)", apiErr.Status, apiErr.Code)
		default:
			return fmt.Errorf("daemon request failed (%d)", apiErr.Status)
		}
	}
	return errors.New("daemon request failed")
}
