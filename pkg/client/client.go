// Package client exposes the generated msgvault HTTP API client.
package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Client is a typed msgvault API client generated from the checked-in OpenAPI
// contract.
type Client struct {
	*generated.Client

	apiClient runtime.APIClient
}

// RequestEditorFn mutates generated requests before they are sent.
type RequestEditorFn = runtime.RequestEditorFn

// New creates a typed client using the default generated HTTP transport.
func New(baseURL string, opts ...runtime.APIClientOption) (*Client, error) {
	opts = append([]runtime.APIClientOption{runtime.WithHTTPClient(httpClientDoer{client: http.DefaultClient})}, opts...)
	apiClient, err := runtime.NewAPIClient(baseURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create API client: %w", err)
	}
	return NewWithAPIClient(apiClient), nil
}

// NewWithAPIClient wraps a caller-provided generated runtime API client.
func NewWithAPIClient(apiClient runtime.APIClient) *Client {
	wrapped := &floatNormalizingAPIClient{inner: apiClient}
	return &Client{
		Client:    generated.NewClient(wrapped),
		apiClient: wrapped,
	}
}

// floatNormalizingAPIClient works around an upstream defect in the generated
// runtime. The generated request options build their path/query maps via
// runtime.AsMap, a json.Marshal/Unmarshal round-trip that decodes every JSON
// number as float64. The runtime then stringifies those values with
// fmt.Sprintf("%v", ...), which renders float64 values >= ~10^6 in scientific
// notation (e.g. float64(24489626) -> "2.4489626e+07"). Server handlers parse
// path/query IDs with strconv.ParseInt and reject the scientific form with a
// 400. We normalize the maps before the runtime stringifies them.
//
// Delete this wrapper once the runtime dependency renders integral values
// without scientific notation.
type floatNormalizingAPIClient struct {
	inner runtime.APIClient
}

func (c *floatNormalizingAPIClient) GetBaseURL() string {
	return c.inner.GetBaseURL()
}

func (c *floatNormalizingAPIClient) CreateRequest(
	ctx context.Context,
	params runtime.RequestOptionsParameters,
	reqEditors ...runtime.RequestEditorFn,
) (*http.Request, error) {
	if params.Options != nil {
		params.Options = normalizingRequestOptions{inner: params.Options}
	}
	return c.inner.CreateRequest(ctx, params, reqEditors...)
}

func (c *floatNormalizingAPIClient) ExecuteRequest(
	ctx context.Context,
	req *http.Request,
	operationPath string,
) (*runtime.Response, error) {
	return c.inner.ExecuteRequest(ctx, req, operationPath)
}

// normalizingRequestOptions wraps runtime.RequestOptions and rewrites float64
// values in the path and query maps to plain decimal strings. Body and header
// values are delegated unchanged: the body is marshaled from the typed struct
// rather than round-tripped, so it never suffers the float64 defect.
type normalizingRequestOptions struct {
	inner runtime.RequestOptions
}

func (o normalizingRequestOptions) GetPathParams() (map[string]any, error) {
	params, err := o.inner.GetPathParams()
	if err != nil {
		return nil, err
	}
	return normalizeFloatMap(params), nil
}

func (o normalizingRequestOptions) GetQuery() (map[string]any, error) {
	query, err := o.inner.GetQuery()
	if err != nil {
		return nil, err
	}
	return normalizeFloatMap(query), nil
}

func (o normalizingRequestOptions) GetBody() any {
	return o.inner.GetBody()
}

func (o normalizingRequestOptions) GetHeader() (map[string]string, error) {
	return o.inner.GetHeader()
}

func normalizeFloatMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = normalizeFloatValue(v)
	}
	return out
}

func normalizeFloatValue(v any) any {
	switch val := v.(type) {
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case map[string]any:
		return normalizeFloatMap(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalizeFloatValue(item)
		}
		return out
	default:
		return v
	}
}

// APIClient returns the generated runtime client used for requests.
func (c *Client) APIClient() runtime.APIClient {
	if c == nil {
		return nil
	}
	return c.apiClient
}

// AddAccount accepts both documented success statuses. The generated
// convenience method treats only 201 as success even though the daemon returns
// 200 when the account already exists.
func (c *Client) AddAccount(
	ctx context.Context,
	options *generated.AddAccountRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*generated.AddAccountResponseJSON, error) {
	resp, err := c.AddAccountWithResponse(ctx, options, reqEditors...)
	if err != nil {
		return nil, err
	}
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	err = runtime.NewClientAPIError(
		fmt.Errorf("unexpected status code: %d", resp.StatusCode),
		runtime.WithStatusCode(resp.StatusCode))
	return nil, fmt.Errorf("add account: %w", err)
}

// StageDeletion accepts both documented success statuses. The generated
// convenience method treats only 201 as success even though the daemon
// returns 200 for dry-run staging requests.
func (c *Client) StageDeletion(
	ctx context.Context,
	options *generated.StageDeletionRequestOptions,
	reqEditors ...runtime.RequestEditorFn,
) (*generated.StageDeletionResponseJSON, error) {
	resp, err := c.StageDeletionWithResponse(ctx, options, reqEditors...)
	if err != nil {
		return nil, err
	}
	if resp.JSON201 != nil {
		return resp.JSON201, nil
	}
	if resp.JSON200 != nil {
		return resp.JSON200, nil
	}
	err = runtime.NewClientAPIError(
		fmt.Errorf("unexpected status code: %d", resp.StatusCode),
		runtime.WithStatusCode(resp.StatusCode))
	return nil, fmt.Errorf("stage deletion: %w", err)
}

type httpClientDoer struct {
	client *http.Client
}

func (d httpClientDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	// #nosec G704 -- this typed client intentionally sends caller-created
	// requests to the caller-configured msgvault API base URL.
	return client.Do(req)
}
