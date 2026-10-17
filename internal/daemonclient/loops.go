package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/loops"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func decodeLoopResponse[T any](body []byte) (T, error) {
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decode open-loop response: %w", err)
	}
	return out, nil
}

func (c *Client) ListOpenLoops(ctx context.Context, opts loops.ListOptions) (loops.ListResponse, error) {
	query := &generated.ListOpenLoopsQuery{Cursor: optionalString(opts.Cursor)}
	if opts.Kind != "" {
		value := generated.ListOpenLoopsQueryKind(opts.Kind)
		query.Kind = &value
	}
	if opts.Status != "" {
		value := generated.ListOpenLoopsQueryStatus(opts.Status)
		query.Status = &value
	}
	if opts.PersonID != 0 {
		query.PersonID = &opts.PersonID
	}
	if opts.SourceID != 0 {
		query.SourceID = &opts.SourceID
	}
	if opts.Limit != 0 {
		value := int64(opts.Limit)
		query.Limit = &value
	}
	if opts.IncludeSnoozed {
		query.IncludeSnoozed = &opts.IncludeSnoozed
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListOpenLoopsResp, error) {
		return api.ListOpenLoopsWithResponse(ctx, &generated.ListOpenLoopsRequestOptions{Query: query})
	})
	if err != nil {
		return loops.ListResponse{}, err
	}
	return decodeLoopResponse[loops.ListResponse](response.Body)
}

func (c *Client) GetOpenLoop(ctx context.Context, id int64) (loops.DetailResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetOpenLoopResp, error) {
		return api.GetOpenLoopWithResponse(ctx, &generated.GetOpenLoopRequestOptions{PathParams: &generated.GetOpenLoopPath{ID: id}})
	})
	if err != nil {
		return loops.DetailResponse{}, err
	}
	return decodeLoopResponse[loops.DetailResponse](response.Body)
}

func (c *Client) GetWaitingLoops(ctx context.Context, opts loops.WaitingOptions) (loops.WaitingResponse, error) {
	query := &generated.GetWaitingLoopsQuery{AllowStale: &opts.AllowStale}
	if opts.Top != 0 {
		value := int64(opts.Top)
		query.Top = &value
	}
	if !opts.AsOf.IsZero() {
		query.AsOf = &opts.AsOf
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetWaitingLoopsResp, error) {
		return api.GetWaitingLoopsWithResponse(ctx, &generated.GetWaitingLoopsRequestOptions{Query: query})
	})
	if err != nil {
		return loops.WaitingResponse{}, err
	}
	return decodeLoopResponse[loops.WaitingResponse](response.Body)
}

func (c *Client) CloseOpenLoop(ctx context.Context, id int64, note string) (loops.DetailResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.CloseOpenLoopResp, error) {
		return api.CloseOpenLoopWithResponse(ctx, &generated.CloseOpenLoopRequestOptions{PathParams: &generated.CloseOpenLoopPath{ID: id}, Body: &generated.CloseOpenLoopBody{Note: optionalString(note)}})
	})
	if err != nil {
		return loops.DetailResponse{}, err
	}
	return decodeLoopResponse[loops.DetailResponse](response.Body)
}

func (c *Client) SnoozeOpenLoop(ctx context.Context, id int64, until time.Time) (loops.DetailResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.SnoozeOpenLoopResp, error) {
		return api.SnoozeOpenLoopWithResponse(ctx, &generated.SnoozeOpenLoopRequestOptions{PathParams: &generated.SnoozeOpenLoopPath{ID: id}, Body: &generated.SnoozeOpenLoopBody{Until: until}})
	})
	if err != nil {
		return loops.DetailResponse{}, err
	}
	return decodeLoopResponse[loops.DetailResponse](response.Body)
}

func (c *Client) UnsnoozeOpenLoop(ctx context.Context, id int64) (loops.DetailResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.UnsnoozeOpenLoopResp, error) {
		return api.UnsnoozeOpenLoopWithResponse(ctx, &generated.UnsnoozeOpenLoopRequestOptions{PathParams: &generated.UnsnoozeOpenLoopPath{ID: id}})
	})
	if err != nil {
		return loops.DetailResponse{}, err
	}
	return decodeLoopResponse[loops.DetailResponse](response.Body)
}

func (c *Client) ListLoopSuppressions(ctx context.Context) ([]loops.Suppression, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListLoopSuppressionsResp, error) {
		return api.ListLoopSuppressionsWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	decoded, err := decodeLoopResponse[loops.SuppressionListResponse](response.Body)
	return decoded.Items, err
}

func (c *Client) CreateLoopSuppression(ctx context.Context, request loops.SuppressionRequest) (loops.SuppressionResult, error) {
	response, err := APIResponseWithStatuses(c, []int{http.StatusOK, http.StatusCreated}, func(api *apiclient.Client) (*generated.CreateLoopSuppressionResp, error) {
		return api.CreateLoopSuppressionWithResponse(ctx, &generated.CreateLoopSuppressionRequestOptions{Body: &generated.CreateLoopSuppressionBody{TargetKind: request.TargetKind, PersonID: request.PersonID, ParticipantID: request.ParticipantID, ConversationID: request.ConversationID, Note: optionalString(request.Note), DryRun: &request.DryRun}})
	})
	if err != nil {
		return loops.SuppressionResult{}, err
	}
	return decodeLoopResponse[loops.SuppressionResult](response.Body)
}

func (c *Client) DeleteLoopSuppression(ctx context.Context, id int64) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(api *apiclient.Client) (*generated.DeleteLoopSuppressionResp, error) {
		return api.DeleteLoopSuppressionWithResponse(ctx, &generated.DeleteLoopSuppressionRequestOptions{PathParams: &generated.DeleteLoopSuppressionPath{ID: id}})
	})
	return err
}

func (c *Client) GetLoopsStatus(ctx context.Context) (loops.StatusResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetLoopsStatusResp, error) {
		return api.GetLoopsStatusWithResponse(ctx)
	})
	if err != nil {
		return loops.StatusResponse{}, err
	}
	return decodeLoopResponse[loops.StatusResponse](response.Body)
}

func (c *Client) RunLoops(ctx context.Context, rebuild bool) (loops.RunResponse, error) {
	if rebuild {
		response, err := APIResponse(c, func(api *apiclient.Client) (*generated.RebuildLoopsResp, error) {
			return api.RebuildLoopsWithResponse(ctx)
		})
		if err != nil {
			return loops.RunResponse{}, err
		}
		return decodeLoopResponse[loops.RunResponse](response.Body)
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.RunLoopsResp, error) { return api.RunLoopsWithResponse(ctx) })
	if err != nil {
		return loops.RunResponse{}, err
	}
	return decodeLoopResponse[loops.RunResponse](response.Body)
}

// ListPersonOpenLoops implements the same Reader contract as the in-process
// Service. A missing certified frontier remains zero rather than a job time.
func (c *Client) ListPersonOpenLoops(ctx context.Context, personID int64, limit int) (loops.Page, error) {
	query := &generated.ListPersonLoopsQuery{}
	if limit != 0 {
		value := int64(limit)
		query.Limit = &value
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ListPersonLoopsResp, error) {
		return api.ListPersonLoopsWithResponse(ctx, &generated.ListPersonLoopsRequestOptions{PathParams: &generated.ListPersonLoopsPath{ID: personID}, Query: query})
	})
	if err != nil {
		return loops.Page{}, err
	}
	list, err := decodeLoopResponse[loops.ListResponse](response.Body)
	if err != nil {
		return loops.Page{}, err
	}
	page := loops.Page{Loops: list.Items, Truncated: list.NextCursor != ""}
	if projection := list.Freshness.Projections["open_loops"]; projection != nil && projection.ComputedAt != nil {
		page.ComputedAt = *projection.ComputedAt
	}
	return page, nil
}

var _ loops.Reader = (*Client)(nil)
