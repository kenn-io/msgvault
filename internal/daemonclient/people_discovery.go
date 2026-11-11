package daemonclient

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PersonDiscoveryMinAPISchemaVersion guarantees exact organization scope and
// current organization alias lookup. Older daemons can ignore unknown filters.
const PersonDiscoveryMinAPISchemaVersion = "5.1.0"

var _ peoplebrowser.SemanticPeopleSearcher = (*PeopleBrowser)(nil)
var _ peoplebrowser.OrganizationLister = (*PeopleBrowser)(nil)

// RequirePersonDiscovery checks support for scoped discovery and alias lookup.
func (c *Client) RequirePersonDiscovery(ctx context.Context) error {
	supported, err := c.SupportsAPISchemaVersion(ctx, PersonDiscoveryMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check person discovery capability: %w", err)
	}
	if !supported {
		return &APIError{Status: 503, Code: "person_discovery_scope_unsupported", Message: fmt.Sprintf("Person discovery requires daemon API schema %s or newer; upgrade the daemon", PersonDiscoveryMinAPISchemaVersion)}
	}
	return nil
}

// RequirePersonDiscoveryScope fails closed before sending a possibly ignored
// scope. Unscoped calls preserve compatibility with existing daemon versions.
func (c *Client) RequirePersonDiscoveryScope(ctx context.Context, organizationID int64) error {
	if organizationID == 0 {
		return nil
	}
	return c.RequirePersonDiscovery(ctx)
}

func (b *PeopleBrowser) SearchSemanticPeople(ctx context.Context, request peoplebrowser.SemanticPeopleRequest) (*peoplebrowser.SemanticPeoplePage, error) {
	if err := b.engine.store.RequirePersonDiscoveryScope(ctx, request.OrganizationID); err != nil {
		return nil, err
	}
	body := generated.SearchPeopleBody{Query: request.Query, Limit: new(int64(request.Limit))}
	if request.OrganizationID != 0 {
		body.OrganizationID = &request.OrganizationID
	}
	response, err := APIResponse(b.engine.store, func(client *apiclient.Client) (*generated.SearchPeopleResp, error) {
		return client.SearchPeopleWithResponse(ctx, &generated.SearchPeopleRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("empty semantic people response")
	}
	page := &peoplebrowser.SemanticPeoplePage{Results: make([]peoplebrowser.SemanticPersonMatch, 0, len(response.JSON200.Results))}
	for _, result := range response.JSON200.Results {
		page.Results = append(page.Results, peoplebrowser.SemanticPersonMatch{PersonID: result.Person.ID, DisplayName: result.Person.DisplayName, VCardUID: result.Person.VcardUID, Score: result.Score})
	}
	return page, nil
}

func (b *PeopleBrowser) ListOrganizations(ctx context.Context, query string, limit, offset int) (*peoplebrowser.OrganizationPage, error) {
	if err := b.engine.store.RequirePersonDiscovery(ctx); err != nil {
		return nil, err
	}
	response, err := APIResponse(b.engine.store, func(client *apiclient.Client) (*generated.ListOrganizationsResp, error) {
		return client.ListOrganizationsWithResponse(ctx, &generated.ListOrganizationsRequestOptions{Query: &generated.ListOrganizationsQuery{
			Q: optionalString(query), Limit: new(int64(limit)), Offset: new(int64(offset)),
		}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("empty organization response")
	}
	data := response.JSON200
	page := &peoplebrowser.OrganizationPage{Organizations: make([]peoplebrowser.OrganizationSummary, 0, len(data.Organizations)), Total: data.Total, Limit: int(data.Limit), Offset: int(data.Offset)}
	for _, organization := range data.Organizations {
		page.Organizations = append(page.Organizations, peoplebrowser.OrganizationSummary{ID: organization.ID, Name: organization.Name})
	}
	page.HasMore = len(page.Organizations) > 0 && int64(page.Offset)+int64(len(page.Organizations)) < page.Total
	return page, nil
}
