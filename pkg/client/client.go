// Package client exposes the generated msgvault HTTP API client.
package client

import (
	"context"
	"fmt"
	"net/http"

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
	return &Client{
		Client:    generated.NewClient(apiClient),
		apiClient: apiClient,
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
