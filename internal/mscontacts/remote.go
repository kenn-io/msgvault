// Package mscontacts syncs Microsoft 365 and Outlook.com contacts through
// Microsoft Graph as a carddav.Remote. Each contact folder is one address book.
package mscontacts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
)

// GraphBaseURL is the Microsoft Graph v1.0 endpoint.
const GraphBaseURL = "https://graph.microsoft.com/v1.0"

// uidProperty is the Graph extended property that holds the vCard UID of a
// contact that msgvault created. Outlook contacts do not have it.
const uidProperty = "String {6f3a1c52-8d4e-4b7a-9c1f-2e5d7a9b0c3d} Name msgvaultUID"

// vcardProperty holds the full vCard that msgvault last wrote to a contact,
// including what Graph cannot hold: properties without a field, and details
// such as a phone TYPE. Every create and update sets it.
const vcardProperty = "String {6f3a1c52-8d4e-4b7a-9c1f-2e5d7a9b0c3d} Name msgvaultVCard"

// sentProperty holds the Graph fields that msgvault last wrote, as JSON.
// Outlook moves fields around, so only this record tells which vCard lines
// Graph held. Every create and update sets it.
const sentProperty = "String {6f3a1c52-8d4e-4b7a-9c1f-2e5d7a9b0c3d} Name msgvaultSent"

const (
	operationTimeout = 5 * time.Minute
	operationBytes   = 256 << 20
	maxBooks         = 1000
	// pageBytes caps one Graph response, as the CardDAV client caps one DAV
	// response.
	pageBytes = 32 << 20
)

// Remote is the Graph contacts backend of a CardDAV connection.
//
// A card made by msgvault has the href <book>/uid/<UID>, so the href is known
// before Graph assigns an ID. Any other card has the href <book>/id/<Graph ID>.
type Remote struct {
	graph *msgraph.Client
	base  string
}

var _ carddav.Remote = (*Remote)(nil)

// NewRemote creates a Remote for the signed-in user. baseURL is GraphBaseURL
// except in tests.
func NewRemote(baseURL string, token msgraph.TokenFunc) *Remote {
	graph := msgraph.NewClient(baseURL, token, 5)
	// Immutable IDs survive a move between folders. Without a page size, Graph
	// returns 10 contacts for each page.
	graph.Headers = map[string]string{"Prefer": `IdType="ImmutableId", odata.maxpagesize=1000`}
	return &Remote{graph: graph, base: strings.TrimRight(baseURL, "/")}
}

func (r *Remote) Limits() (time.Duration, int64) { return operationTimeout, operationBytes }

type folder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// Discover lists the default Contacts folder and every folder below it.
func (r *Remote) Discover(ctx context.Context, _ string) (carddav.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var root folder
	if err := r.graph.GetJSON(ctx, r.base+"/me/contactFolders/contacts?$select=id,displayName", &root); err != nil {
		return carddav.Discovery{}, statusError(fmt.Errorf("read the Contacts folder: %w", err))
	}
	folders := []folder{root}
	for i := 0; i < len(folders); i++ {
		_, err := msgraph.PageThrough(ctx, r.graph,
			r.base+"/me/contactFolders/"+url.PathEscape(folders[i].ID)+"/childFolders?$select=id,displayName",
			func(page []folder) { folders = append(folders, page...) })
		if err != nil {
			return carddav.Discovery{}, statusError(fmt.Errorf("list contact folders: %w", err))
		}
		if len(folders) > maxBooks {
			return carddav.Discovery{}, fmt.Errorf("graph contacts exceed %d folders", maxBooks)
		}
	}
	principal, err := url.Parse(r.base + "/me")
	if err != nil {
		return carddav.Discovery{}, fmt.Errorf("parse Graph base URL: %w", err)
	}
	home := principal.JoinPath("contactFolders")
	discovery := carddav.Discovery{PrincipalURL: principal, HomeURL: home, HomeURLs: []*url.URL{home}}
	for i, f := range folders {
		book, err := url.Parse(r.bookURL(f.ID))
		if err != nil {
			return carddav.Discovery{}, fmt.Errorf("parse contact folder URL: %w", err)
		}
		discovery.Books = append(discovery.Books, carddav.DiscoveredBook{
			URL: book, DisplayName: f.DisplayName, DiscoveryIndex: i,
			SupportsSyncCollection: true, SupportedVCardVersions: []string{"4.0"},
			Capabilities: carddav.BookCapabilities{
				Create: true, CreateKnown: true, Update: true, UpdateKnown: true, Delete: true, DeleteKnown: true,
			},
		})
	}
	return discovery, nil
}

func (r *Remote) bookURL(folderID string) string {
	return r.base + "/me/contactFolders/" + url.PathEscape(folderID) + "/contacts"
}

// Pull walks the folder's delta query. Delta cannot return the UID property,
// so when anything changed, the folder is listed again as a full snapshot.
func (r *Remote) Pull(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *carddav.Budget,
) (store.CardDAVSyncPlan, error) {
	start := token
	if start == "" {
		start = book.CanonicalURL + "/delta?$select=id"
	}
	changed := false
	next, err := pages(ctx, r, start, budget, func(page []contact) { changed = changed || len(page) > 0 })
	if errors.Is(err, msgraph.ErrGone) {
		return store.CardDAVSyncPlan{}, fmt.Errorf("%w: %w", carddav.ErrInvalidSyncToken, err)
	}
	if err != nil {
		return store.CardDAVSyncPlan{}, statusError(err)
	}
	if next == "" {
		return store.CardDAVSyncPlan{}, errors.New("graph contact delta returned no delta link")
	}
	if token != "" && !changed {
		return store.CardDAVSyncPlan{NextSyncToken: next}, nil
	}
	// ponytail: any change lists the whole folder again, one request for each
	// 1,000 contacts. Fetch only the changed IDs if large folders need it.
	plan := store.CardDAVSyncPlan{ReplaceAll: true, NextSyncToken: next}
	var parseErr error
	_, err = pages(ctx, r, book.CanonicalURL+"?$expand="+expandUID(), budget, func(page []contact) {
		for _, c := range page {
			resource, err := r.resource(book.CanonicalURL, c)
			if err != nil {
				parseErr = errors.Join(parseErr, err)
				continue
			}
			plan.Upserts = append(plan.Upserts, resource)
		}
	})
	if err != nil {
		return store.CardDAVSyncPlan{}, statusError(err)
	}
	if parseErr != nil {
		return store.CardDAVSyncPlan{}, parseErr
	}
	return plan, nil
}

// pages follows nextLink and charges each page to the budget. It returns the
// final deltaLink, if any.
func pages(ctx context.Context, r *Remote, start string, budget *carddav.Budget, fn func([]contact)) (string, error) {
	link := start
	for {
		var body []byte
		err := r.call(ctx, func(ctx context.Context) (err error) {
			body, err = r.graph.GetRawLimited(ctx, link, pageBytes)
			return err
		})
		if err != nil {
			return "", err
		}
		if err := budget.Consume(int64(len(body))); err != nil {
			return "", err
		}
		var page msgraph.ListResponse[contact]
		if err := json.Unmarshal(body, &page); err != nil {
			return "", fmt.Errorf("decode Graph contacts page: %w", err)
		}
		fn(page.Value)
		if page.NextLink == "" {
			return page.DeltaLink, nil
		}
		link = page.NextLink
	}
}

func expandUID() string {
	return url.QueryEscape("singleValueExtendedProperties($filter=id eq '" + uidProperty + "' or id eq '" + vcardProperty + "' or id eq '" + sentProperty + "')")
}

// resource renders a Graph contact in book as a stored CardDAV resource.
func (r *Remote) resource(bookURL string, c contact) (store.CardDAVRemoteResource, error) {
	href, uid := bookURL+"/id/"+url.PathEscape(c.ID), c.ID
	if value := c.uid(); value != "" {
		href, uid = bookURL+"/uid/"+url.PathEscape(value), value
	}
	body, err := c.body(uid)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	return carddav.NewRemoteResource(href, c.ETag, body)
}

// parseHref splits a card href into its book URL, its kind ("uid" or "id")
// and its key.
func (r *Remote) parseHref(href string) (book, kind, key string, err error) {
	for _, kind := range []string{"uid", "id"} {
		marker := "/contacts/" + kind + "/"
		if i := strings.LastIndex(href, marker); i > 0 && strings.HasPrefix(href, r.base+"/me/contactFolders/") {
			key, err := url.PathUnescape(href[i+len(marker):])
			if err != nil || key == "" {
				break
			}
			return href[:i+len("/contacts")], kind, key, nil
		}
	}
	return "", "", "", fmt.Errorf("%w: %s", carddav.ErrUnsafeTarget, href)
}

// lookup finds the contact at href, and returns the book URL from href.
func (r *Remote) lookup(ctx context.Context, href string) (c contact, book string, found bool, err error) {
	book, kind, key, err := r.parseHref(href)
	if err != nil {
		return contact{}, "", false, err
	}
	if kind == "uid" {
		filter := url.QueryEscape("singleValueExtendedProperties/Any(ep: ep/id eq '" + uidProperty +
			"' and ep/value eq '" + strings.ReplaceAll(key, "'", "''") + "')")
		var page msgraph.ListResponse[contact]
		if err := r.call(ctx, func(ctx context.Context) error {
			return r.graph.GetJSON(ctx, book+"?$filter="+filter+"&$expand="+expandUID(), &page)
		}); err != nil {
			return contact{}, book, false, err
		}
		if len(page.Value) > 1 {
			return contact{}, book, false, fmt.Errorf("graph contact folder has %d contacts with UID %q", len(page.Value), key)
		}
		if len(page.Value) == 0 {
			return contact{}, book, false, nil
		}
		return page.Value[0], book, true, nil
	}
	err = r.call(ctx, func(ctx context.Context) error {
		return r.graph.GetJSON(ctx, r.base+"/me/contacts/"+url.PathEscape(key)+"?$expand="+expandUID(), &c)
	})
	if errors.Is(err, msgraph.ErrNotFound) {
		return contact{}, book, false, nil
	}
	if err != nil {
		return contact{}, book, false, err
	}
	// A contact moved to another folder is absent from this book.
	return c, book, r.bookURL(c.ParentFolderID) == book, nil
}

func (r *Remote) Get(ctx context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	c, book, found, err := r.lookup(ctx, href)
	if err != nil || !found {
		return store.CardDAVRemoteResource{Href: href}, err == nil, err
	}
	resource, err := r.resource(book, c)
	if err == nil && resource.Href != href {
		return store.CardDAVRemoteResource{Href: href}, true, nil
	}
	return resource, false, err
}

// Put creates or updates a contact. Graph has no conditional create, so a
// create looks up the UID first. The gap between that read and the POST is
// open only to the same user's own writes.
func (r *Remote) Put(ctx context.Context, href string, body []byte, etag string, create bool) error {
	fields, err := contactFromVCard(body)
	if err != nil {
		return err
	}
	current, _, found, err := r.lookup(ctx, href)
	if err != nil {
		return err
	}
	if found {
		placePhones(&fields, &current)
		keepEmailNames(&fields, &current)
	} else {
		placePhones(&fields, nil)
	}
	if create {
		book, kind, uid, err := r.parseHref(href)
		if err != nil || kind != "uid" {
			return fmt.Errorf("%w: %s", carddav.ErrUnsafeTarget, href)
		}
		if found {
			return &carddav.StatusError{StatusCode: http.StatusPreconditionFailed}
		}
		fields.Properties = append([]singleValueExtendedProperty{{ID: uidProperty, Value: uid}}, fields.saved(body)...)
		return r.call(ctx, func(ctx context.Context) error {
			_, err := r.graph.SendOnce(ctx, http.MethodPost, book, fields, "")
			return err
		})
	}
	if !found {
		return &carddav.StatusError{StatusCode: http.StatusNotFound}
	}
	if current.ETag != etag {
		return &carddav.StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	fields.Properties = fields.saved(body)
	patch := func(ctx context.Context) error {
		_, err := r.graph.SendOnce(ctx, http.MethodPatch, r.base+"/me/contacts/"+url.PathEscape(current.ID), fields, etag)
		return err
	}
	// A blind repeat after an applied PATCH would fail its If-Match and look
	// like a conflict. Graph changes the ETag on every update, so an
	// unchanged ETag proves that the failed PATCH was not applied.
	err = r.call(ctx, patch)
	if err != nil && unclear(err) {
		if after, _, found, lookupErr := r.lookup(ctx, href); lookupErr == nil && found && after.ETag == etag {
			err = r.call(ctx, patch)
		}
	}
	return err
}

// Delete moves the contact to Deleted Items. Graph ignores If-Match on
// DELETE, so the ETag is compared first.
// ponytail: an Outlook edit between the comparison and the DELETE is lost
// instead of becoming a conflict; Deleted Items still holds the contact.
// Graph has no conditional delete to close the gap.
func (r *Remote) Delete(ctx context.Context, href, etag string) error {
	current, _, found, err := r.lookup(ctx, href)
	if err != nil {
		return err
	}
	if !found {
		return &carddav.StatusError{StatusCode: http.StatusNotFound}
	}
	if current.ETag != etag {
		return &carddav.StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	return r.call(ctx, func(ctx context.Context) error {
		_, err := r.graph.Send(ctx, http.MethodDelete, r.base+"/me/contacts/"+url.PathEscape(current.ID), nil, "")
		return err
	})
}

func (r *Remote) CreateHref(collectionURL, uid string) (string, error) {
	if strings.TrimSpace(uid) == "" || !strings.HasPrefix(collectionURL, r.base+"/me/contactFolders/") ||
		!strings.HasSuffix(collectionURL, "/contacts") {
		return "", carddav.ErrUnsafeTarget
	}
	return collectionURL + "/uid/" + url.PathEscape(uid), nil
}

// call sends one Graph request behind the connection's retry gate, as the
// carddav.Remote contract requires for calls that send several requests.
func (r *Remote) call(ctx context.Context, request func(context.Context) error) error {
	return carddav.GateRequest(ctx, func(ctx context.Context) error { return statusError(request(ctx)) })
}

// unclear reports a write failure that does not say whether Graph applied
// the write: no HTTP status that the service branches on.
func unclear(err error) bool {
	_, status := errors.AsType[*carddav.StatusError](err)
	return !status && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// statusError adds the CardDAV status that the service branches on. A 429
// that outlasted Graph's retries pauses the connection through the retry gate.
func statusError(err error) error {
	if throttled, ok := errors.AsType[*msgraph.ThrottledError](err); ok {
		return errors.Join(err, &carddav.StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: max(throttled.RetryAfter, time.Second)})
	}
	for sentinel, code := range map[error]int{
		msgraph.ErrNotFound:           http.StatusNotFound,
		msgraph.ErrPreconditionFailed: http.StatusPreconditionFailed,
		msgraph.ErrForbidden:          http.StatusForbidden,
		msgraph.ErrUnauthorized:       http.StatusUnauthorized,
	} {
		if errors.Is(err, sentinel) {
			return errors.Join(err, &carddav.StatusError{StatusCode: code})
		}
	}
	// Any other 4xx is a definitive rejection, for example a 400 for an
	// invalid property: Graph applied nothing.
	if status, ok := errors.AsType[*msgraph.StatusError](err); ok && status.StatusCode < http.StatusInternalServerError {
		return errors.Join(err, &carddav.StatusError{StatusCode: status.StatusCode})
	}
	return err
}
