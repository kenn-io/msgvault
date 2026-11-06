package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetTimelineWithResponse returns Markdown in Body when format=markdown.
// Other formats use the generated JSON response decoder.
func (c *Client) GetTimelineWithResponse(ctx context.Context, options *generated.GetTimelineRequestOptions, reqEditors ...runtime.RequestEditorFn) (*generated.GetTimelineResp, error) {
	if options == nil || options.Query == nil || options.Query.Format == nil || *options.Query.Format != "markdown" {
		return c.Client.GetTimelineWithResponse(ctx, options, reqEditors...)
	}
	api := c.APIClient()
	request, err := api.CreateRequest(ctx, runtime.RequestOptionsParameters{
		RequestURL: api.GetBaseURL() + "/api/v1/timeline",
		Method:     http.MethodGet,
		Options:    options,
	}, reqEditors...)
	if err != nil {
		return nil, fmt.Errorf("create timeline request: %w", err)
	}
	response, err := api.ExecuteRequest(ctx, request, "/api/v1/timeline")
	if err != nil {
		return nil, fmt.Errorf("execute timeline request: %w", err)
	}
	out := &generated.GetTimelineResp{HTTPResponse: response.Raw, StatusCode: response.StatusCode, Body: response.Content}
	if response.StatusCode != http.StatusOK {
		return out, runtime.NewClientAPIError(fmt.Errorf("API error (status %d)", response.StatusCode), runtime.WithStatusCode(response.StatusCode))
	}
	return out, nil
}
