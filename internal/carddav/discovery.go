package carddav

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/store"
)

// Discovery is one complete CardDAV collection snapshot.
type Discovery struct {
	PrincipalURL *url.URL
	HomeURL      *url.URL
	HomeURLs     []*url.URL
	Books        []DiscoveredBook
}

// DiscoveredBook describes one address-book collection in server order.
type DiscoveredBook struct {
	URL                    *url.URL
	DiscoveryAliasURL      *url.URL
	DisplayName            string
	DiscoveryIndex         int
	SupportsSyncCollection bool
	SupportsMultiget       bool
	SupportedVCardVersions []string
	Capabilities           BookCapabilities
}

// BookCapabilities retains the distinction between a denied privilege and a
// server which did not advertise current-user-privilege-set at all.
type BookCapabilities struct {
	Create      bool
	CreateKnown bool
	Update      bool
	UpdateKnown bool
	Delete      bool
	DeleteKnown bool
}

// Discover follows RFC 6764/6352 discovery from the entered URL through the
// principal and address-book home set, then enumerates every book at Depth 1.
func Discover(ctx context.Context, client *Client, enteredURL string) (Discovery, error) {
	if client == nil {
		return Discovery{}, errors.New("CardDAV discovery requires a client")
	}
	operationCtx, cancel := context.WithTimeout(ctx, client.operationTimeout)
	defer cancel()
	return discoverWithBudget(operationCtx, client, AddressBookKind, enteredURL, &Budget{remaining: client.operationBytes})
}

// DiscoverCollections runs Discover for kind. The caller owns the operation
// deadline and transfer budget.
func DiscoverCollections(
	operationCtx context.Context, client *Client, kind Kind, enteredURL string, budget *Budget,
) (Discovery, error) {
	return discoverWithBudget(operationCtx, client, kind, enteredURL, budget)
}

func discoverWithBudget(
	operationCtx context.Context, client *Client, kind Kind, enteredURL string, budget *Budget,
) (Discovery, error) {
	entered, err := url.Parse(enteredURL)
	if err != nil {
		return Discovery{}, fmt.Errorf("parse %s base URL: %w", kind.name, ErrUnsafeTarget)
	}
	if _, err := client.validateTarget(operationCtx, entered); err != nil {
		return Discovery{}, err
	}

	principal, directErr := discoverHref(operationCtx, client, entered, CurrentUserPrincipalProperty, budget)
	if directErr != nil || principal == nil {
		if !discoveryFallbackAllowed(directErr) {
			return Discovery{}, directErr
		}
		wellKnown := *client.origin
		wellKnown.Path = "/.well-known/" + kind.wellKnown
		principal, err = discoverHref(operationCtx, client, &wellKnown, CurrentUserPrincipalProperty, budget)
		if err != nil {
			return Discovery{}, fmt.Errorf("discover %s principal: %w", kind.name, err)
		}
	}
	if principal == nil {
		return Discovery{}, fmt.Errorf("%s discovery did not return current-user-principal", kind.name)
	}

	homes, err := discoverHrefs(operationCtx, client, principal, kind.homeSet, budget)
	if err != nil {
		return Discovery{}, fmt.Errorf("discover %s collection home: %w", kind.name, err)
	}
	if len(homes) == 0 {
		return Discovery{}, fmt.Errorf("%s discovery did not return %s", kind.name, kind.homeSet.Local)
	}
	books := make([]DiscoveredBook, 0)
	seenBooks := map[string]bool{}
	for _, home := range homes {
		discovered, err := discoverBooks(operationCtx, client, kind, home, budget)
		if err != nil {
			return Discovery{}, err
		}
		for _, book := range discovered {
			key := CollectionURLIdentity(book.URL)
			if seenBooks[key] {
				continue
			}
			seenBooks[key] = true
			book.DiscoveryIndex = len(books)
			books = append(books, book)
			if len(books) > 1000 {
				return Discovery{}, fmt.Errorf("%s discovery exceeded 1000 collections", kind.name)
			}
		}
	}
	return Discovery{PrincipalURL: principal, HomeURL: homes[0], HomeURLs: homes, Books: books}, nil
}

// DiscoverConnection validates the configured connection without persisting it.
func (s *Service) DiscoverConnection(ctx context.Context, baseURL string) (Discovery, error) {
	if s == nil || s.remote == nil {
		return Discovery{}, errors.New("CardDAV service is not configured")
	}
	return s.remote.Discover(ctx, baseURL)
}

// DiscoverAndPersist validates the configured connection and atomically
// replaces the durable discovery snapshot through the shared service.
func (s *Service) DiscoverAndPersist(ctx context.Context, baseURL, username string) (Discovery, error) {
	if s == nil || s.store == nil || s.remote == nil {
		return Discovery{}, errors.New("CardDAV service is not configured")
	}
	discovery, err := s.DiscoverConnection(ctx, baseURL)
	if err != nil {
		return Discovery{}, err
	}
	if err := s.PersistDiscovery(ctx, baseURL, username, discovery, false); err != nil {
		return Discovery{}, err
	}
	return discovery, nil
}

// PersistDiscovery commits a previously completed network discovery. Keeping
// this step separate lets callers publish crash-safe connection files before
// the destructive replacement transaction begins.
func (s *Service) PersistDiscovery(
	ctx context.Context, baseURL, username string, discovery Discovery, credentialsChanged bool,
) error {
	if s == nil || s.store == nil {
		return errors.New("CardDAV service is not configured")
	}
	if discovery.PrincipalURL == nil || discovery.HomeURL == nil {
		return errors.New("CardDAV discovery is incomplete")
	}
	homeURLs := discoveryHomeURLStrings(discovery)
	input := store.CardDAVDiscoveryInput{
		ConnectionName: s.ConnectionName(),
		BaseURL:        baseURL, Username: username, CredentialsChanged: credentialsChanged,
		PrincipalURL: discovery.PrincipalURL.String(), HomeURL: discovery.HomeURL.String(), HomeURLs: homeURLs,
		Books: make([]store.CardDAVDiscoveredBook, 0, len(discovery.Books)),
	}
	for _, book := range discovery.Books {
		input.Books = append(input.Books, store.CardDAVDiscoveredBook{
			CanonicalURL: book.URL.String(), DiscoveryAliasURL: urlString(book.DiscoveryAliasURL),
			DisplayName: book.DisplayName, DiscoveryIndex: book.DiscoveryIndex,
			SupportsSyncCollection: book.SupportsSyncCollection, SupportsMultiget: book.SupportsMultiget,
			SupportedVCardVersions: append([]string(nil), book.SupportedVCardVersions...),
			CanCreate:              capabilityValue(book.Capabilities.CreateKnown, book.Capabilities.Create),
			CanUpdate:              capabilityValue(book.Capabilities.UpdateKnown, book.Capabilities.Update),
			CanDelete:              capabilityValue(book.Capabilities.DeleteKnown, book.Capabilities.Delete),
		})
	}
	if _, _, err := s.store.ReplaceCardDAVDiscoveryContext(ctx, input); err != nil {
		return err
	}
	return nil
}

func discoveryHomeURLStrings(discovery Discovery) []string {
	homes := discovery.HomeURLs
	if len(homes) == 0 && discovery.HomeURL != nil {
		homes = []*url.URL{discovery.HomeURL}
	}
	result := make([]string, 0, len(homes))
	for _, home := range homes {
		if home != nil {
			result = append(result, home.String())
		}
	}
	return result
}

func capabilityValue(known, value bool) *bool {
	if !known {
		return nil
	}
	return &value
}

func urlString(value *url.URL) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func discoverHref(
	ctx context.Context, client *Client, target *url.URL, property PropertyName, budget *Budget,
) (*url.URL, error) {
	hrefs, err := discoverHrefs(ctx, client, target, property, budget)
	if err != nil {
		return nil, err
	}
	if len(hrefs) == 0 {
		return nil, nil //nolint:nilnil // A missing optional DAV property is distinct from a request failure.
	}
	if len(hrefs) != 1 {
		return nil, fmt.Errorf("DAV discovery returned conflicting %s values", property.Local)
	}
	return hrefs[0], nil
}

func discoverHrefs(
	ctx context.Context, client *Client, target *url.URL, property PropertyName, budget *Budget,
) ([]*url.URL, error) {
	body, err := PropfindBody([]PropertyName{property})
	if err != nil {
		return nil, err
	}
	depth := 0
	response, err := client.doWithBudget(
		ctx, Request{Method: "PROPFIND", URL: target.String(), Depth: &depth, Body: body}, budget)
	if err != nil {
		return nil, err
	}
	effectiveTarget := response.EffectiveURL
	if effectiveTarget == nil {
		effectiveTarget = target
	}
	multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return nil, err
	}
	if len(multiStatus.Responses) != 1 {
		return nil, fmt.Errorf("DAV discovery expected one response, got %d", len(multiStatus.Responses))
	}
	davResponse := multiStatus.Responses[0]
	if _, err := resolveDiscoveryHref(ctx, client, effectiveTarget, davResponse.Href); err != nil {
		return nil, err
	}
	if davResponse.StatusCode != 0 && (davResponse.StatusCode < 200 || davResponse.StatusCode >= 300) {
		return nil, &StatusError{StatusCode: davResponse.StatusCode}
	}
	hrefs := []string{}
	for _, propStat := range davResponse.PropStats {
		if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
			continue
		}
		switch property {
		case CurrentUserPrincipalProperty:
			if propStat.Properties.CurrentUserPrincipal != "" {
				hrefs = append(hrefs, propStat.Properties.CurrentUserPrincipal)
			}
		case CalendarHomeSetProperty:
			hrefs = append(hrefs, propStat.Properties.CalendarHomeSet...)
		case AddressbookHomeSetProperty:
			hrefs = append(hrefs, propStat.Properties.AddressbookHomeSet...)
		}
	}
	resolved := make([]*url.URL, 0, len(hrefs))
	seen := map[string]bool{}
	for _, href := range hrefs {
		value, err := resolveDiscoveryHref(ctx, client, effectiveTarget, href)
		if err != nil {
			return nil, err
		}
		key := CollectionURLIdentity(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		resolved = append(resolved, value)
		if len(resolved) > 1000 {
			return nil, errors.New("DAV discovery exceeded 1000 property hrefs")
		}
	}
	return resolved, nil
}

func discoverBooks(
	ctx context.Context, client *Client, kind Kind, home *url.URL, budget *Budget,
) ([]DiscoveredBook, error) {
	body, err := PropfindBody(append([]PropertyName{
		ResourceTypeProperty,
		DisplayNameProperty,
		SupportedReportSetProperty,
	}, kind.discovery...))
	if err != nil {
		return nil, err
	}
	depth := 1
	response, err := client.doWithBudget(
		ctx, Request{Method: "PROPFIND", URL: home.String(), Depth: &depth, Body: body}, budget)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s collections: %w", kind.name, err)
	}
	effectiveHome := response.EffectiveURL
	if effectiveHome == nil {
		effectiveHome = home
	}
	multiStatus, err := kind.parseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return nil, fmt.Errorf("parse %s collections: %w", kind.name, err)
	}
	if len(multiStatus.Responses) > 1000 {
		return nil, fmt.Errorf("%s discovery exceeded 1000 responses", kind.name)
	}

	foundHome := false
	books := make([]DiscoveredBook, 0, len(multiStatus.Responses))
	seen := map[string]bool{}
	for index, davResponse := range multiStatus.Responses {
		resolved, err := resolveDiscoveryHref(ctx, client, effectiveHome, davResponse.Href)
		if err != nil {
			return nil, fmt.Errorf("resolve %s discovery response %d: %w", kind.name, index+1, err)
		}
		isHome := SameCollectionURL(resolved, effectiveHome)
		if davResponse.StatusCode != 0 && (davResponse.StatusCode < 200 || davResponse.StatusCode >= 300) {
			if isHome {
				return nil, fmt.Errorf("%s discovery response %d failed: %w", kind.name, index+1, &StatusError{StatusCode: davResponse.StatusCode})
			}
			continue
		}
		properties := mergeSuccessfulProperties(davResponse.PropStats)
		if isHome {
			if !properties.ResourceTypePresent {
				return nil, fmt.Errorf("%s discovery response %d omitted successful resourcetype", kind.name, index+1)
			}
			if foundHome || !properties.IsCollection {
				return nil, fmt.Errorf("%s discovery returned an invalid home collection response", kind.name)
			}
			foundHome = true
			continue
		}
		if !properties.ResourceTypePresent {
			continue
		}
		if !kind.isMember(properties) {
			continue
		}
		if !properties.IsCollection {
			return nil, fmt.Errorf("%s discovery response %d identified a non-collection resource", kind.name, index+1)
		}
		if _, err := client.ValidateChildHref(ctx, effectiveHome, davResponse.Href); err != nil {
			return nil, err
		}
		advertised, err := resolveDiscoveryHref(ctx, client, home, davResponse.Href)
		if err != nil {
			return nil, err
		}
		var alias *url.URL
		if !SameCollectionURL(advertised, resolved) {
			alias = advertised
		}
		key := CollectionURLIdentity(resolved)
		if seen[key] {
			return nil, fmt.Errorf("%s discovery returned duplicate collection %s", kind.name, key)
		}
		seen[key] = true
		displayName := strings.TrimSpace(properties.DisplayName)
		if displayName == "" {
			displayName = kind.defaultName
		}
		books = append(books, DiscoveredBook{
			URL: resolved, DiscoveryAliasURL: alias, DisplayName: displayName, DiscoveryIndex: len(books),
			SupportsSyncCollection: properties.SupportsSync,
			SupportsMultiget:       properties.SupportsMultiget,
			SupportedVCardVersions: append([]string(nil), properties.SupportedVCard...),
			Capabilities:           capabilitiesFrom(properties),
		})
	}
	if !foundHome {
		return nil, fmt.Errorf("%s discovery did not include the home collection response", kind.name)
	}
	return books, nil
}

func mergeSuccessfulProperties(propStats []PropStat) Properties {
	var merged Properties
	for _, propStat := range propStats {
		if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
			continue
		}
		properties := propStat.Properties
		if properties.SyncToken != "" {
			merged.SyncToken = properties.SyncToken
		}
		if properties.CurrentUserPrincipal != "" {
			merged.CurrentUserPrincipal = properties.CurrentUserPrincipal
		}
		if properties.DisplayName != "" {
			merged.DisplayName = properties.DisplayName
		}
		if properties.ResourceTypePresent {
			merged.ResourceTypePresent = true
			merged.IsCollection = merged.IsCollection || properties.IsCollection
			merged.IsAddressBook = merged.IsAddressBook || properties.IsAddressBook
			merged.IsCalendar = merged.IsCalendar || properties.IsCalendar
		}
		if properties.CalendarComponentsPresent {
			merged.CalendarComponentsPresent = true
			merged.CalendarComponents = append(merged.CalendarComponents, properties.CalendarComponents...)
		}
		if properties.PrivilegesPresent {
			merged.PrivilegesPresent = true
			for _, privilege := range properties.Privileges {
				if !slices.Contains(merged.Privileges, privilege) {
					merged.Privileges = append(merged.Privileges, privilege)
				}
			}
		}
		merged.SupportsSync = merged.SupportsSync || properties.SupportsSync
		merged.SupportsMultiget = merged.SupportsMultiget || properties.SupportsMultiget
		for _, version := range properties.SupportedVCard {
			if !slices.Contains(merged.SupportedVCard, version) {
				merged.SupportedVCard = append(merged.SupportedVCard, version)
			}
		}
	}
	return merged
}

func capabilitiesFrom(properties Properties) BookCapabilities {
	if !properties.PrivilegesPresent {
		return BookCapabilities{}
	}
	granted := map[string]bool{}
	for _, privilege := range properties.Privileges {
		granted[privilege] = true
		// RFC 3744 section 3.8 explicitly excludes collection membership
		// changes from DAV:write. Creating and deleting members require the
		// separate DAV:bind and DAV:unbind privileges.
		if privilege == "write" || privilege == "all" {
			granted["write-content"] = true
		}
		if privilege == "all" {
			granted["bind"] = true
			granted["unbind"] = true
		}
	}
	return BookCapabilities{
		Create: granted["bind"], CreateKnown: true,
		Update: granted["write-content"], UpdateKnown: true,
		Delete: granted["unbind"], DeleteKnown: true,
	}
}

func resolveDiscoveryHref(ctx context.Context, client *Client, base *url.URL, href string) (*url.URL, error) {
	if href == "" || strings.TrimSpace(href) != href || strings.Contains(href, "\\") {
		return nil, ErrUnsafeHref
	}
	resolved, err := base.Parse(href)
	if err != nil || resolved.User != nil || resolved.Fragment != "" || !sameOrigin(client.origin, resolved) {
		return nil, ErrUnsafeHref
	}
	if _, err := client.validateTarget(ctx, resolved); err != nil {
		return nil, fmt.Errorf("discovery href: %w", ErrUnsafeHref)
	}
	return resolved, nil
}

func discoveryFallbackAllowed(err error) bool {
	if err == nil {
		return true
	}
	var status *StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// SameCollectionURL reports whether two URLs name the same collection.
func SameCollectionURL(left, right *url.URL) bool {
	return CollectionURLIdentity(left) == CollectionURLIdentity(right)
}

// CanonicalDAVURLIdentity normalizes the origin and removes the fragment for a DAV resource identity.
func CanonicalDAVURLIdentity(value *url.URL) string {
	clone := *value
	clone.Scheme = strings.ToLower(clone.Scheme)
	clone.Host = net.JoinHostPort(strings.ToLower(clone.Hostname()), netguard.EffectivePort(&clone))
	clone.Fragment = ""
	return clone.String()
}

// CollectionURLIdentity returns the normalized identity key used to compare
// collection URLs.
func CollectionURLIdentity(value *url.URL) string {
	clone := *value
	clone.Scheme = strings.ToLower(clone.Scheme)
	clone.Host = net.JoinHostPort(strings.ToLower(clone.Hostname()), netguard.EffectivePort(&clone))
	clone.Fragment = ""
	clone.Path = path.Clean(clone.Path)
	if !strings.HasSuffix(clone.Path, "/") {
		clone.Path += "/"
	}
	return clone.String()
}
