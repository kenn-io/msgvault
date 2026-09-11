package carddav

import (
	"context"
	"errors"
	"net/url"

	"go.kenn.io/msgvault/internal/store"
)

// GoogleDiscoveryURL is Google's canonical CardDAV bootstrap URL. All
// principal and address-book URLs are discovered from this endpoint.
const GoogleDiscoveryURL = "https://www.googleapis.com/.well-known/carddav"

// NewGoogleService uses Google's discovery entry point with the shared sync
// engine. Google can redirect directly to its single contacts collection.
func NewGoogleService(st *store.Store, client *Client) *Service {
	return &Service{store: st, client: client, google: true}
}

func discoverGoogle(ctx context.Context, client *Client, baseURL string) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, client.operationTimeout)
	defer cancel()
	body, err := PropfindBody([]PropertyName{CurrentUserPrincipalProperty, SyncTokenProperty, DisplayNameProperty, CurrentUserPrivilegesProperty})
	if err != nil {
		return Discovery{}, err
	}
	depth := 0
	response, err := client.Do(ctx, Request{Method: "PROPFIND", URL: baseURL, Depth: &depth, Body: body})
	if err != nil {
		return Discovery{}, err
	}
	multi, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return Discovery{}, err
	}
	for _, entry := range multi.Responses {
		if entry.StatusCode != 0 && (entry.StatusCode < 200 || entry.StatusCode >= 300) {
			continue
		}
		properties := mergeSuccessfulProperties(entry.PropStats)
		if len(properties.CurrentUserPrincipal) != 0 {
			discovery, err := Discover(ctx, client, baseURL)
			if err != nil {
				return Discovery{}, err
			}
			for i := range discovery.Books {
				discovery.Books[i].Capabilities = googleCapabilities(discovery.Books[i].Capabilities)
			}
			return discovery, nil
		}
	}
	// Some Google responses omit principal and resourcetype properties. Accept
	// only the redirected collection's own successful sync-token property,
	// never a contact or an arbitrary response href as an address book.
	for _, entry := range multi.Responses {
		if entry.StatusCode != 0 && (entry.StatusCode < 200 || entry.StatusCode >= 300) {
			continue
		}
		properties := mergeSuccessfulProperties(entry.PropStats)
		if properties.SyncToken == "" {
			continue
		}
		target := response.EffectiveURL
		if target == nil {
			return Discovery{}, errors.New("discovery from Google omitted its effective URL")
		}
		book, err := resolveDiscoveryHref(ctx, client, target, entry.Href)
		if err != nil {
			return Discovery{}, err
		}
		if !sameCollectionURL(book, target) {
			continue
		}
		name := properties.DisplayName
		if name == "" {
			name = "Google Contacts"
		}
		return Discovery{
			PrincipalURL: book, HomeURL: book, HomeURLs: []*url.URL{book},
			Books: []DiscoveredBook{{URL: book, DisplayName: name,
				SupportsSyncCollection: true, SupportsMultiget: true, SupportedVCardVersions: []string{"3.0"},
				Capabilities: googleCapabilities(capabilitiesFrom(properties))}},
		}, nil
	}
	return Discovery{}, errors.New("discovery from Google returned neither a principal nor a contacts collection")
}

func googleCapabilities(capabilities BookCapabilities) BookCapabilities {
	if !capabilities.CreateKnown && !capabilities.UpdateKnown && !capabilities.DeleteKnown {
		// Google's contacts collection supports creating, updating, and
		// deleting contacts even when DAV privileges are not advertised.
		// https://developers.google.com/people/carddav
		return BookCapabilities{
			Create: true, CreateKnown: true,
			Update: true, UpdateKnown: true,
			Delete: true, DeleteKnown: true,
		}
	}
	return capabilities
}
