package generated

// Temporary typed federation bindings until the OpenAPI generator runs in CI.
// Remove this file when committing the authoritative generated output.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

type FederationIdentityPending struct {
	ArchiveUID string `json:"archive_uid"`
	Role       string `json:"role"`
}
type FederationEnrollmentViewPending struct {
	EnrollmentUID   string     `json:"enrollment_uid"`
	SpokeArchiveUID string     `json:"spoke_archive_uid"`
	Member          string     `json:"member"`
	SpokeName       string     `json:"spoke_name"`
	CreatedAt       time.Time  `json:"created_at"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
}
type FederationEnrollmentPending struct {
	Enrollment  FederationEnrollmentViewPending `json:"enrollment"`
	Token       string                          `json:"token"`
	HubInstance string                          `json:"hub_instance"`
}
type FederationCreatePending struct {
	SpokeInstance string `json:"spoke_instance"`
	Member        string `json:"member"`
	SpokeName     string `json:"spoke_name,omitempty"`
}
type FederationListPending struct {
	Enrollments []FederationEnrollmentViewPending `json:"enrollments"`
}
type FederationResponsePending[T any] struct {
	HTTPResponse *http.Response
	Body         []byte
	StatusCode   int
	JSON200      *T
	JSON201      *T
}
type federationOptionsPending struct{ body any }

func (o federationOptionsPending) GetPathParams() (map[string]any, error) { return nil, nil }
func (o federationOptionsPending) GetQuery() (map[string]any, error)      { return nil, nil }
func (o federationOptionsPending) GetHeader() (map[string]string, error)  { return nil, nil }
func (o federationOptionsPending) GetBody() any                           { return o.body }
func federationPending[T any](ctx context.Context, c *Client, method, path string, body any) (*FederationResponsePending[T], error) {
	req, err := c.apiClient.CreateRequest(ctx, runtime.RequestOptionsParameters{RequestURL: c.apiClient.GetBaseURL() + path, Method: method, Options: federationOptionsPending{body}, ContentType: "application/json"})
	if err != nil {
		return nil, fmt.Errorf("create federation request: %w", err)
	}
	resp, err := c.apiClient.ExecuteRequest(ctx, req, path)
	if err != nil {
		return nil, fmt.Errorf("execute federation request: %w", err)
	}
	out := &FederationResponsePending[T]{HTTPResponse: resp.Raw, Body: resp.Content, StatusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var value T
		if len(resp.Content) > 0 {
			if err := json.Unmarshal(resp.Content, &value); err != nil {
				return out, err
			}
		}
		if resp.StatusCode == http.StatusOK {
			out.JSON200 = &value
		} else {
			out.JSON201 = &value
		}
		return out, nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return out, nil
	}
	return out, runtime.NewClientAPIError(fmt.Errorf("API error (status %d)", resp.StatusCode), runtime.WithStatusCode(resp.StatusCode))
}
func (c *Client) GetFederationIdentityPending(ctx context.Context) (*FederationResponsePending[FederationIdentityPending], error) {
	return federationPending[FederationIdentityPending](ctx, c, http.MethodGet, "/api/v1/federation/identity", nil)
}
func (c *Client) CreateFederationEnrollmentPending(ctx context.Context, body FederationCreatePending) (*FederationResponsePending[FederationEnrollmentPending], error) {
	return federationPending[FederationEnrollmentPending](ctx, c, http.MethodPost, "/api/v1/federation/enrollments", body)
}
func (c *Client) ListFederationEnrollmentsPending(ctx context.Context) (*FederationResponsePending[FederationListPending], error) {
	return federationPending[FederationListPending](ctx, c, http.MethodGet, "/api/v1/federation/enrollments", nil)
}
func (c *Client) RevokeFederationEnrollmentPending(ctx context.Context, uid string) (*FederationResponsePending[struct{}], error) {
	return federationPending[struct{}](ctx, c, http.MethodDelete, "/api/v1/federation/enrollments/"+url.PathEscape(uid), nil)
}
