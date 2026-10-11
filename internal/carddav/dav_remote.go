package carddav

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// davRemote is the CardDAV Remote. Google uses it with google set, because
// Google's CardDAV needs two workarounds.
type davRemote struct {
	client *Client
	google bool
}

func (r *davRemote) Discover(ctx context.Context, entered string) (Discovery, error) {
	if r.google {
		return discoverGoogle(ctx, r.client, entered)
	}
	return Discover(ctx, r.client, entered)
}

func (r *davRemote) Limits() (time.Duration, int64) {
	return r.client.operationTimeout, r.client.operationBytes
}

func (r *davRemote) Pull(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	if !book.SupportsSyncCollection {
		return r.fetchSnapshot(ctx, book, budget)
	}
	plan, err := r.fetchSyncCollectionPlan(ctx, book, token, budget)
	if err == nil {
		return plan, nil
	}
	var status *StatusError
	switch {
	case errors.As(err, &status) && status.Precondition == "valid-sync-token" && token != "":
		return store.CardDAVSyncPlan{}, fmt.Errorf("%w: %w", ErrInvalidSyncToken, err)
	case errors.As(err, &status) && (status.StatusCode == http.StatusMethodNotAllowed || status.StatusCode == http.StatusNotImplemented):
		// Capability advertisements are hints. A standards-compliant snapshot
		// is the bounded downgrade when sync-collection is unavailable.
		return r.fetchSnapshot(ctx, book, budget)
	default:
		return store.CardDAVSyncPlan{}, err
	}
}

func (r *davRemote) Put(ctx context.Context, href string, body []byte, etag string, create bool) error {
	_, err := r.client.Do(ctx, Request{Method: http.MethodPut, URL: href, Body: body, ETag: etag, Create: create})
	return err
}

func (r *davRemote) Delete(ctx context.Context, href, etag string) error {
	_, err := r.client.Do(ctx, Request{Method: http.MethodDelete, URL: href, ETag: etag})
	return err
}

func (r *davRemote) do(ctx context.Context, request Request, budget *Budget) (*Response, error) {
	var response *Response
	err := GateRequest(ctx, func(ctx context.Context) error {
		var err error
		response, err = r.client.Do(ctx, request)
		return err
	})
	if response != nil {
		if budgetErr := budget.consume(response); budgetErr != nil {
			return nil, budgetErr
		}
	}
	return response, err
}

// fetchSyncCollectionPlan runs sync-collection, substituting an enumerated
// snapshot for the initial empty-token request when the server rejects it
// with 400, as Google does. See fetchSnapshot for why Google never queries.
func (r *davRemote) fetchSyncCollectionPlan(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	if token == "" && r.google {
		return r.fetchEnumeratedSnapshot(ctx, book, budget)
	}
	plan, err := r.fetchSyncCollection(ctx, book, token, budget)
	if token == "" && isStatus(err, http.StatusBadRequest) {
		return r.fetchEnumeratedSnapshot(ctx, book, budget)
	}
	return plan, err
}

func (r *davRemote) fetchSyncCollection(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	collection, err := url.Parse(book.CanonicalURL)
	if err != nil {
		return store.CardDAVSyncPlan{}, ErrUnsafeTarget
	}
	events := map[string]bool{} // true = changed, false = removed
	seenTokens := map[string]bool{}
	pageToken := token
	var nextToken string
	for page := range maxSyncPages {
		seenTokens[pageToken] = true
		body, err := SyncCollectionBody(pageToken)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		// RFC 6578 section 3.3 requires Depth: 0; DAV:sync-level scopes the report.
		depth := 0
		response, err := r.do(ctx, Request{Method: "REPORT", URL: book.CanonicalURL, Depth: &depth, Body: body}, budget)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		if response.EffectiveURL != nil {
			collection = response.EffectiveURL
		}
		changed, removed, continuation, truncated, err := r.parseSyncPage(ctx, collection, multiStatus)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		for _, href := range changed {
			events[href] = true
		}
		for _, href := range removed {
			events[href] = false
		}
		if len(events) > maxSyncMembers {
			return store.CardDAVSyncPlan{}, fmt.Errorf("CardDAV sync exceeds %d members", maxSyncMembers)
		}
		nextToken = continuation
		if !truncated {
			break
		}
		if seenTokens[continuation] {
			return store.CardDAVSyncPlan{}, ErrSyncTokenCycle
		}
		pageToken = continuation
		if page == maxSyncPages-1 {
			return store.CardDAVSyncPlan{}, fmt.Errorf("CardDAV sync exceeds %d pages", maxSyncPages)
		}
	}

	changed := make([]string, 0, len(events))
	removed := make([]string, 0, len(events))
	for href, isChanged := range events {
		if isChanged {
			changed = append(changed, href)
		} else {
			removed = append(removed, href)
		}
	}
	slices.Sort(changed)
	slices.Sort(removed)
	resources, missing, err := r.fetchMembers(ctx, book, collection, changed, budget)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	removed = append(removed, missing...)
	return store.CardDAVSyncPlan{
		ReplaceAll: token == "", NextSyncToken: nextToken,
		Upserts: resources, RemovedHrefs: removed,
	}, nil
}

// fetchMembers retrieves the vCards for hrefs in multiget batches when the book
// advertises multiget, downgrading to individual GETs when the advertisement
// proves wrong. Members that vanished between listing and fetch are returned
// separately as missing.
func (r *davRemote) fetchMembers(
	ctx context.Context, book store.CardDAVAddressBook, collection *url.URL, hrefs []string, budget *Budget,
) ([]store.CardDAVRemoteResource, []string, error) {
	if !book.SupportsMultiget {
		return r.fetchMembersIndividually(ctx, collection, hrefs, budget)
	}
	resources := make([]store.CardDAVRemoteResource, 0, len(hrefs))
	missing := make([]string, 0)
	for offset := 0; offset < len(hrefs); offset += multigetBatch {
		end := min(offset+multigetBatch, len(hrefs))
		cards, absent, err := r.fetchMultiget(ctx, collection, hrefs[offset:end], budget)
		if isStatus(err, http.StatusMethodNotAllowed) || isStatus(err, http.StatusNotImplemented) {
			cards, absent, err = r.fetchMembersIndividually(ctx, collection, hrefs[offset:], budget)
			if err != nil {
				return nil, nil, err
			}
			return append(resources, cards...), append(missing, absent...), nil
		}
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, cards...)
		missing = append(missing, absent...)
	}
	return resources, missing, nil
}

// fetchEnumeratedSnapshot replaces the book's contents by listing its members
// with PROPFIND and fetching them with the usual member path. The sync token
// is read before the listing so anything that changes while the snapshot runs
// is reported by the next incremental sync rather than lost.
func (r *davRemote) fetchEnumeratedSnapshot(
	ctx context.Context, book store.CardDAVAddressBook, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	collection, err := url.Parse(book.CanonicalURL)
	if err != nil {
		return store.CardDAVSyncPlan{}, ErrUnsafeTarget
	}
	token, err := r.fetchCollectionSyncToken(ctx, book.ID, collection, budget)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	hrefs, collection, err := r.fetchMemberListing(ctx, collection, budget)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	resources, _, err := r.fetchMembers(ctx, book, collection, hrefs, budget)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	return store.CardDAVSyncPlan{ReplaceAll: true, NextSyncToken: token, Upserts: resources}, nil
}

// fetchCollectionSyncToken reads the collection's current DAV:sync-token. A
// server that omits the property yields an empty token, which repeats the
// enumerated snapshot on every sync; that is logged so the degradation is not
// silent.
func (r *davRemote) fetchCollectionSyncToken(
	ctx context.Context, bookID int64, collection *url.URL, budget *Budget,
) (string, error) {
	body, err := PropfindBody([]PropertyName{SyncTokenProperty})
	if err != nil {
		return "", err
	}
	depth := 0
	response, err := r.do(ctx, Request{Method: "PROPFIND", URL: collection.String(), Depth: &depth, Body: body}, budget)
	if err != nil {
		return "", err
	}
	multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return "", err
	}
	if response.EffectiveURL != nil {
		collection = response.EffectiveURL
	}
	for _, davResponse := range multiStatus.Responses {
		if davResponse.StatusCode != 0 && (davResponse.StatusCode < 200 || davResponse.StatusCode >= 300) {
			continue
		}
		resolved, err := r.resolveMemberHref(ctx, collection, davResponse.Href, true)
		if err != nil {
			return "", err
		}
		if sameCollectionURL(resolved, collection) {
			token := mergeSuccessfulProperties(davResponse.PropStats).SyncToken
			if token == "" {
				slog.WarnContext(ctx, "CardDAV collection reported no sync token; every sync will enumerate the address book",
					"address_book_id", bookID)
			}
			return token, nil
		}
	}
	slog.WarnContext(ctx, "CardDAV collection omitted its own response; every sync will enumerate the address book",
		"address_book_id", bookID)
	return "", nil
}

// fetchMemberListing enumerates the collection's members with a Depth 1
// PROPFIND. The result feeds a ReplaceAll plan, so anything the listing drops
// is removed locally; the listing therefore fails rather than guesses. RFC
// 4918 requires the collection's own response, so a listing without it is
// treated as incomplete. Nested collections are identified by resourcetype
// and skipped; any other member must carry an ETag.
func (r *davRemote) fetchMemberListing(
	ctx context.Context, collection *url.URL, budget *Budget,
) ([]string, *url.URL, error) {
	body, err := PropfindBody([]PropertyName{GetETagProperty, ResourceTypeProperty})
	if err != nil {
		return nil, nil, err
	}
	depth := 1
	response, err := r.do(ctx, Request{Method: "PROPFIND", URL: collection.String(), Depth: &depth, Body: body}, budget)
	if err != nil {
		if isStatus(err, http.StatusInsufficientStorage) {
			return nil, nil, ErrTruncatedSnapshot
		}
		return nil, nil, err
	}
	multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return nil, nil, err
	}
	if response.EffectiveURL != nil {
		collection = response.EffectiveURL
	}
	hrefs := make([]string, 0, len(multiStatus.Responses))
	seen := map[string]bool{}
	sawCollection := false
	for _, davResponse := range multiStatus.Responses {
		resolved, err := r.resolveMemberHref(ctx, collection, davResponse.Href, true)
		if err != nil {
			return nil, nil, err
		}
		isCollection := sameCollectionURL(resolved, collection)
		if davResponse.StatusCode == http.StatusInsufficientStorage {
			return nil, nil, ErrTruncatedSnapshot
		}
		if isAbsentStatusCode(davResponse.StatusCode) && !isCollection {
			continue
		}
		if davResponse.StatusCode != 0 && (davResponse.StatusCode < 200 || davResponse.StatusCode >= 300) {
			return nil, nil, &StatusError{StatusCode: davResponse.StatusCode}
		}
		tagged, nested := false, false
		for _, propStat := range davResponse.PropStats {
			switch {
			case propStat.StatusCode == http.StatusInsufficientStorage:
				return nil, nil, ErrTruncatedSnapshot
			case isAbsentStatusCode(propStat.StatusCode):
				continue
			case propStat.StatusCode < 200 || propStat.StatusCode >= 300:
				return nil, nil, &StatusError{StatusCode: propStat.StatusCode}
			}
			tagged = tagged || propStat.Properties.GetETag != ""
			nested = nested || propStat.Properties.IsCollection
		}
		if isCollection {
			sawCollection = true
			continue
		}
		if nested {
			continue
		}
		if !tagged {
			return nil, nil, errors.New("CardDAV listed member lacks ETag")
		}
		identity := canonicalDAVURLIdentity(resolved)
		if seen[identity] {
			return nil, nil, ErrIncompleteMultiget
		}
		seen[identity] = true
		hrefs = append(hrefs, identity)
		if len(hrefs) > maxSyncMembers {
			return nil, nil, fmt.Errorf("CardDAV member listing exceeds %d members", maxSyncMembers)
		}
	}
	if !sawCollection {
		return nil, nil, errors.New("CardDAV member listing omitted the collection")
	}
	slices.Sort(hrefs)
	return hrefs, collection, nil
}

func (r *davRemote) fetchMembersIndividually(
	ctx context.Context, collection *url.URL, hrefs []string, budget *Budget,
) ([]store.CardDAVRemoteResource, []string, error) {
	resources := make([]store.CardDAVRemoteResource, 0, len(hrefs))
	missing := make([]string, 0)
	for _, href := range hrefs {
		response, err := r.do(ctx, Request{Method: http.MethodGet, URL: href}, budget)
		if isAbsentStatus(err) {
			missing = append(missing, href)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		effective := response.EffectiveURL
		if effective == nil {
			effective, err = url.Parse(href)
			if err != nil {
				return nil, nil, ErrUnsafeHref
			}
		}
		if _, err := r.resolveMemberHref(ctx, collection, effective.String(), false); err != nil {
			return nil, nil, err
		}
		etag := strings.TrimSpace(response.Header.Get("ETag"))
		if etag == "" || len(response.Body) == 0 {
			return nil, nil, ErrIncompleteMultiget
		}
		resource, err := parseRemoteResource(href, etag, response.Body)
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, resource)
	}
	return resources, missing, nil
}

func (r *davRemote) parseSyncPage(
	ctx context.Context, collection *url.URL, multiStatus MultiStatus,
) ([]string, []string, string, bool, error) {
	if multiStatus.SyncToken == "" {
		return nil, nil, "", false, errors.New("CardDAV sync response lacks a usable sync token")
	}
	changed, removed := []string{}, []string{}
	seen := map[string]bool{}
	truncated := false
	for _, davResponse := range multiStatus.Responses {
		resolved, err := r.resolveMemberHref(ctx, collection, davResponse.Href, true)
		if err != nil {
			return nil, nil, "", false, err
		}
		isCollection := sameCollectionURL(resolved, collection)
		if davResponse.StatusCode != 0 {
			identity := canonicalDAVURLIdentity(resolved)
			switch {
			case davResponse.StatusCode == http.StatusInsufficientStorage && isCollection:
				truncated = true
				continue
			case isAbsentStatusCode(davResponse.StatusCode) && !isCollection:
				if seen[identity] {
					return nil, nil, "", false, ErrIncompleteMultiget
				}
				seen[identity] = true
				removed = append(removed, identity)
				continue
			default:
				return nil, nil, "", false, &StatusError{StatusCode: davResponse.StatusCode}
			}
		}
		if isCollection {
			for _, propStat := range davResponse.PropStats {
				if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
					return nil, nil, "", false, &StatusError{StatusCode: propStat.StatusCode}
				}
			}
			continue
		}
		identity := canonicalDAVURLIdentity(resolved)
		if seen[identity] {
			return nil, nil, "", false, ErrIncompleteMultiget
		}
		seen[identity] = true
		etag := ""
		for _, propStat := range davResponse.PropStats {
			if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
				return nil, nil, "", false, &StatusError{StatusCode: propStat.StatusCode}
			}
			if propStat.Properties.GetETag != "" {
				etag = propStat.Properties.GetETag
			}
		}
		if etag == "" {
			return nil, nil, "", false, errors.New("CardDAV sync member lacks ETag")
		}
		changed = append(changed, identity)
	}
	return changed, removed, multiStatus.SyncToken, truncated, nil
}

func (r *davRemote) fetchMultiget(
	ctx context.Context, collection *url.URL, hrefs []string, budget *Budget,
) ([]store.CardDAVRemoteResource, []string, error) {
	requestHrefs, err := multigetHrefs(collection, hrefs)
	if err != nil {
		return nil, nil, err
	}
	body, err := AddressbookMultigetBody([]PropertyName{GetETagProperty, AddressDataProperty}, requestHrefs)
	if err != nil {
		return nil, nil, err
	}
	depth := 0
	response, err := r.do(ctx, Request{Method: "REPORT", URL: collection.String(), Depth: &depth, Body: body}, budget)
	if err != nil {
		return nil, nil, err
	}
	multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return nil, nil, err
	}
	if response.EffectiveURL != nil {
		collection = response.EffectiveURL
	}
	wanted := make(map[string]bool, len(hrefs))
	for _, href := range hrefs {
		resolved, err := collection.Parse(href)
		if err != nil {
			return nil, nil, ErrIncompleteMultiget
		}
		identity := canonicalDAVURLIdentity(resolved)
		if wanted[identity] {
			return nil, nil, ErrIncompleteMultiget
		}
		wanted[identity] = true
	}
	seen := map[string]bool{}
	resources := make([]store.CardDAVRemoteResource, 0, len(hrefs))
	missing := []string{}
	for _, davResponse := range multiStatus.Responses {
		resolved, err := r.resolveMemberHref(ctx, collection, davResponse.Href, false)
		if err != nil {
			return nil, nil, err
		}
		identity := canonicalDAVURLIdentity(resolved)
		href := identity
		if !wanted[identity] || seen[identity] {
			return nil, nil, ErrIncompleteMultiget
		}
		seen[identity] = true
		if isAbsentStatusCode(davResponse.StatusCode) {
			missing = append(missing, href)
			continue
		}
		if davResponse.StatusCode != 0 {
			return nil, nil, &StatusError{StatusCode: davResponse.StatusCode}
		}
		etag, data := "", ""
		for _, propStat := range davResponse.PropStats {
			if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
				return nil, nil, &StatusError{StatusCode: propStat.StatusCode}
			}
			if propStat.Properties.GetETag != "" {
				etag = propStat.Properties.GetETag
			}
			if propStat.Properties.AddressData != "" {
				data = propStat.Properties.AddressData
			}
		}
		if etag == "" || strings.TrimSpace(data) == "" {
			return nil, nil, ErrIncompleteMultiget
		}
		resource, err := parseRemoteResource(href, etag, []byte(data))
		if err != nil {
			return nil, nil, err
		}
		resources = append(resources, resource)
	}
	if len(seen) != len(wanted) {
		return nil, nil, ErrIncompleteMultiget
	}
	return resources, missing, nil
}

// multigetHrefs renders member identities as absolute-path references. RFC 4918
// accepts either an absolute URI or an absolute path, but Apple's CardDAV rejects
// an absolute URI in addressbook-multiget with HTTP 400.
func multigetHrefs(collection *url.URL, hrefs []string) ([]string, error) {
	rendered := make([]string, 0, len(hrefs))
	for _, href := range hrefs {
		resolved, err := collection.Parse(href)
		if err != nil {
			return nil, ErrUnsafeHref
		}
		target := resolved.EscapedPath()
		if target == "" {
			target = "/"
		}
		if resolved.RawQuery != "" {
			target += "?" + resolved.RawQuery
		}
		rendered = append(rendered, target)
	}
	return rendered, nil
}

// fetchSnapshot replaces the book's contents without a sync token. Google's
// addressbook-query answers a populated book with an empty multistatus, which
// a ReplaceAll plan would apply as the removal of every contact, so Google
// always enumerates with PROPFIND instead.
func (r *davRemote) fetchSnapshot(
	ctx context.Context, book store.CardDAVAddressBook, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	if r.google {
		return r.fetchEnumeratedSnapshot(ctx, book, budget)
	}
	return r.fetchQuerySnapshot(ctx, book, budget)
}

func (r *davRemote) fetchQuerySnapshot(
	ctx context.Context, book store.CardDAVAddressBook, budget *Budget,
) (store.CardDAVSyncPlan, error) {
	collection, err := url.Parse(book.CanonicalURL)
	if err != nil {
		return store.CardDAVSyncPlan{}, ErrUnsafeTarget
	}
	body, err := AddressbookQueryBody([]PropertyName{GetETagProperty, AddressDataProperty})
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	depth := 1
	response, err := r.do(ctx, Request{Method: "REPORT", URL: book.CanonicalURL, Depth: &depth, Body: body}, budget)
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusInsufficientStorage {
			return store.CardDAVSyncPlan{}, ErrTruncatedSnapshot
		}
		return store.CardDAVSyncPlan{}, err
	}
	multiStatus, err := ParseMultiStatus(response.Body, DefaultXMLLimits())
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	if response.EffectiveURL != nil {
		collection = response.EffectiveURL
	}
	resources := make([]store.CardDAVRemoteResource, 0, len(multiStatus.Responses))
	seen := map[string]bool{}
	for _, davResponse := range multiStatus.Responses {
		resolved, err := r.resolveMemberHref(ctx, collection, davResponse.Href, true)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		if davResponse.StatusCode == http.StatusInsufficientStorage {
			return store.CardDAVSyncPlan{}, ErrTruncatedSnapshot
		}
		if isAbsentStatusCode(davResponse.StatusCode) && !sameCollectionURL(resolved, collection) {
			continue
		}
		if davResponse.StatusCode != 0 && (davResponse.StatusCode < 200 || davResponse.StatusCode >= 300) {
			return store.CardDAVSyncPlan{}, &StatusError{StatusCode: davResponse.StatusCode}
		}
		if sameCollectionURL(resolved, collection) {
			for _, propStat := range davResponse.PropStats {
				if propStat.StatusCode == http.StatusInsufficientStorage {
					return store.CardDAVSyncPlan{}, ErrTruncatedSnapshot
				}
				if !isAbsentStatusCode(propStat.StatusCode) &&
					(propStat.StatusCode < 200 || propStat.StatusCode >= 300) {
					return store.CardDAVSyncPlan{}, &StatusError{StatusCode: propStat.StatusCode}
				}
			}
			continue
		}
		href := canonicalDAVURLIdentity(resolved)
		if seen[href] {
			return store.CardDAVSyncPlan{}, ErrIncompleteMultiget
		}
		seen[href] = true
		etag, data := "", ""
		for _, propStat := range davResponse.PropStats {
			if propStat.StatusCode < 200 || propStat.StatusCode >= 300 {
				return store.CardDAVSyncPlan{}, &StatusError{StatusCode: propStat.StatusCode}
			}
			if propStat.Properties.GetETag != "" {
				etag = propStat.Properties.GetETag
			}
			if propStat.Properties.AddressData != "" {
				data = propStat.Properties.AddressData
			}
		}
		if etag == "" || strings.TrimSpace(data) == "" {
			return store.CardDAVSyncPlan{}, ErrIncompleteMultiget
		}
		resource, err := parseRemoteResource(href, etag, []byte(data))
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		resources = append(resources, resource)
	}
	return store.CardDAVSyncPlan{ReplaceAll: true, Upserts: resources}, nil
}

func (r *davRemote) resolveMemberHref(
	ctx context.Context, collection *url.URL, href string, allowCollection bool,
) (*url.URL, error) {
	resolved, err := r.client.ValidateChildHref(ctx, collection, href)
	if err != nil {
		return nil, err
	}
	if sameCollectionURL(resolved, collection) {
		if allowCollection {
			return resolved, nil
		}
		return nil, ErrUnsafeHref
	}
	collectionPath := strings.TrimSuffix(path.Clean(collection.EscapedPath()), "/")
	if path.Dir(path.Clean(resolved.EscapedPath())) != collectionPath {
		return nil, ErrUnsafeHref
	}
	return resolved, nil
}

func (r *davRemote) CreateHref(collectionURL, uid string) (string, error) {
	if strings.TrimSpace(uid) == "" {
		return "", ErrUnsafeTarget
	}
	collection, err := url.Parse(collectionURL)
	if err != nil || !validHTTPURL(collection) || !sameOrigin(r.client.origin, collection) {
		return "", fmt.Errorf("CardDAV publication collection: %w", ErrUnsafeTarget)
	}
	child := *collection
	child.RawQuery = ""
	child.ForceQuery = false
	child.Fragment = ""
	escapedBase := strings.TrimSuffix(collection.EscapedPath(), "/") + "/"
	child.Path = strings.TrimSuffix(collection.Path, "/") + "/" + uid + ".vcf"
	child.RawPath = escapedBase + url.PathEscape(uid) + ".vcf"
	if !sameOrigin(collection, &child) || !sameOrigin(r.client.origin, &child) {
		return "", fmt.Errorf("CardDAV publication href: %w", ErrUnsafeTarget)
	}
	return canonicalDAVURLIdentity(&child), nil
}

func (r *davRemote) Get(ctx context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	target, err := url.Parse(href)
	if err != nil || !validHTTPURL(target) || !sameOrigin(r.client.origin, target) {
		return store.CardDAVRemoteResource{}, false, ErrUnsafeTarget
	}
	href = canonicalDAVURLIdentity(target)
	// An absent card is not an error, so the gate must see the status first.
	var response *Response
	err = GateRequest(ctx, func(ctx context.Context) error {
		var err error
		response, err = r.client.Do(ctx, Request{Method: http.MethodGet, URL: href})
		return err
	})
	if isAbsentStatus(err) {
		return store.CardDAVRemoteResource{Href: href}, true, nil
	}
	if err != nil {
		return store.CardDAVRemoteResource{}, false, err
	}
	etag := strings.TrimSpace(response.Header.Get("ETag"))
	if etag == "" || len(response.Body) == 0 {
		return store.CardDAVRemoteResource{}, false, ErrIncompleteMultiget
	}
	remote, err := parseRemoteResource(href, etag, response.Body)
	return remote, false, err
}
