// Package mscontacts syncs Microsoft 365 and Outlook.com contacts through
// Microsoft Graph as a carddav.Remote. Each contact folder is one address book.
package mscontacts

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
)

// GraphBaseURL is the Microsoft Graph v1.0 endpoint.
const GraphBaseURL = msgraph.GraphBaseURL

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

// idProperty holds the Graph ID of the contact that a publication owns. An
// Outlook copy carries it too, so a contact whose idProperty names another
// contact is a copy.
const idProperty = "String {6f3a1c52-8d4e-4b7a-9c1f-2e5d7a9b0c3d} Name msgvaultID"

const (
	operationTimeout = 5 * time.Minute
	operationBytes   = 256 << 20
	maxBooks         = 1000
	// pageBytes caps one Graph response, as the CardDAV client caps one DAV
	// response.
	pageBytes = 32 << 20
	// writeBytes stays under Graph's 4 MB limit for one write request.
	writeBytes = 4_000_000
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
	return newRemote(baseURL, token, 5)
}

// newRemote is NewRemote with a limit of qps Graph requests each second.
func newRemote(baseURL string, token msgraph.TokenFunc, qps float64) *Remote {
	graph := msgraph.NewClient(baseURL, token, qps)
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

// deltaItem is one entry of a delta round. Removed is set when the contact
// left the folder.
type deltaItem struct {
	ID      string    `json:"id"`
	Removed *struct{} `json:"@removed"`
}

// Pull walks the folder's delta query. Delta cannot return the UID property,
// so when anything changed, the folder is listed again as a full snapshot.
// Graph pages a listing by offset, so a change during it can shift a contact
// out of the listing. A full delta round decides which contacts the folder
// holds, and the listing only supplies their bodies.
func (r *Remote) Pull(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *carddav.Budget,
) (store.CardDAVSyncPlan, error) {
	full := book.CanonicalURL + "/delta?$select=id"
	members := map[string]bool{}
	note := func(page []deltaItem) {
		for _, item := range page {
			members[item.ID] = item.Removed == nil
		}
	}
	next, err := pages(ctx, r, cmp.Or(token, full), budget, note)
	if errors.Is(err, msgraph.ErrGone) {
		return store.CardDAVSyncPlan{}, fmt.Errorf("%w: %w", carddav.ErrInvalidSyncToken, err)
	}
	unchanged := token != "" && len(members) == 0
	if err == nil && token != "" && !unchanged {
		// An incremental round names only changes, so a full round runs too.
		clear(members)
		next, err = pages(ctx, r, full, budget, note)
	}
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	if next == "" {
		return store.CardDAVSyncPlan{}, errors.New("graph contact delta returned no delta link")
	}
	if unchanged {
		return store.CardDAVSyncPlan{NextSyncToken: next}, nil
	}
	// ponytail: any change lists the whole folder again, one request for each
	// 100 contacts. Fetch only the changed IDs if large folders need it.
	var contacts []contact
	// Pages of 100 match the CardDAV client's 100 cards for each response. A
	// page over pageBytes, from large saved vCards, is read again in smaller
	// pages; one card is at most writeBytes.
	for top := 100; ; top = max(top/8, 1) {
		contacts = nil
		// An insert or move during the listing can shift a contact already
		// read onto a later offset page. The later read is kept once.
		listed := map[string]int{}
		_, err = pages(ctx, r, book.CanonicalURL+"?$top="+strconv.Itoa(top)+"&$expand="+expandUID(), budget, func(page []contact) {
			for _, c := range page {
				if i, ok := listed[c.ID]; ok {
					contacts[i] = c
					continue
				}
				listed[c.ID] = len(contacts)
				contacts = append(contacts, c)
			}
		})
		if !errors.Is(err, msgraph.ErrTooLarge) || top == 1 {
			break
		}
	}
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	if contacts, err = r.complete(ctx, contacts, budget); err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	// A contact that the listing skipped is read by ID. One deleted or moved
	// to another folder since the delta round is absent, as in lookup.
	for _, c := range contacts {
		delete(members, c.ID)
	}
	for id, present := range members {
		if !present {
			continue
		}
		c, err := r.read(ctx, id, budget)
		if errors.Is(err, msgraph.ErrNotFound) {
			continue
		}
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		if r.bookURL(c.ParentFolderID) == book.CanonicalURL {
			contacts = append(contacts, c)
		}
	}
	plan := store.CardDAVSyncPlan{ReplaceAll: true, NextSyncToken: next}
	// An Outlook copy of a contact keeps its UID. Only the original holds the
	// UID href, as in lookup, and copies get ID hrefs.
	owners := map[string]contact{}
	for _, c := range contacts {
		if owner, ok := owners[c.uid()]; c.original() && (!ok || older(c, owner) < 0) {
			owners[c.uid()] = c
		}
	}
	var parseErr error
	for _, c := range contacts {
		resource, err := r.resource(book.CanonicalURL, c, owners[c.uid()].ID == c.ID)
		if err != nil {
			parseErr = errors.Join(parseErr, err)
			continue
		}
		plan.Upserts = append(plan.Upserts, resource)
	}
	if parseErr != nil {
		return store.CardDAVSyncPlan{}, parseErr
	}
	return plan, nil
}

// pages follows nextLink behind the retry gate and charges every response,
// retries included, to the budget, if any. It returns the final deltaLink, if
// any.
func pages[T any](ctx context.Context, r *Remote, start string, budget *carddav.Budget, fn func([]T)) (string, error) {
	return msgraph.PageThroughFunc(ctx, start, func(ctx context.Context, link string) (body []byte, err error) {
		err = r.call(ctx, func(ctx context.Context) (err error) {
			body, err = r.graph.GetRawMetered(ctx, link, pageBytes, charge(budget))
			return err
		})
		return body, err
	}, fn)
}

// charge returns the budget's meter, or nil when there is no budget.
func charge(budget *carddav.Budget) func(int64) error {
	if budget == nil {
		return nil
	}
	return budget.Consume
}

func expandUID() string {
	return url.QueryEscape("singleValueExtendedProperties($filter=id eq '" + uidProperty + "' or id eq '" + vcardProperty + "' or id eq '" + sentProperty + "' or id eq '" + idProperty + "')")
}

// resource renders a Graph contact in book as a stored CardDAV resource. A
// contact with a UID gets its UID href when byUID is set.
func (r *Remote) resource(bookURL string, c contact, byUID bool) (store.CardDAVRemoteResource, error) {
	href, uid := bookURL+"/id/"+url.PathEscape(c.ID), c.ID
	if value := c.uid(); value != "" && byUID {
		href, uid = bookURL+"/uid/"+url.PathEscape(value), value
	}
	body, err := c.body(uid)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	return carddav.NewRemoteResource(href, c.ETag, body)
}

// original reports whether c is not an Outlook copy of a published contact.
// Until its next update, an unmarked original (its mark PATCH failed or its
// create response was lost) counts as one, and so does its Outlook copy, which
// takes over its publication if it outlives it.
func (c contact) original() bool {
	id := c.property(idProperty)
	return id == "" || id == c.ID
}

// older orders contacts by creation, so an original comes before its
// Outlook copies.
func older(a, b contact) int {
	return cmp.Or(strings.Compare(a.CreatedDateTime, b.CreatedDateTime), strings.Compare(a.ID, b.ID))
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
		var found []contact
		// One contact for each page keeps any number of copies, each up to
		// writeBytes, under the page cap.
		if _, err := pages(ctx, r, book+"?$top=1&$filter="+filter+"&$expand="+expandUID(), nil, func(page []contact) {
			found = append(found, slices.DeleteFunc(page, func(c contact) bool { return !c.original() })...)
		}); err != nil {
			return contact{}, book, false, err
		}
		if found, err = r.complete(ctx, found, nil); err != nil {
			return contact{}, book, false, err
		}
		if len(found) == 0 {
			return contact{}, book, false, nil
		}
		return slices.MinFunc(found, older), book, true, nil
	}
	c, err = r.read(ctx, key, nil)
	if errors.Is(err, msgraph.ErrNotFound) {
		return contact{}, book, false, nil
	}
	if err != nil {
		return contact{}, book, false, err
	}
	// A contact moved to another folder is absent from this book.
	return c, book, r.bookURL(c.ParentFolderID) == book, nil
}

// complete rereads by ID each listed contact whose saved vCard or sent
// fields the listing left out or cut short. body refuses a contact that
// stays thin.
// A contact deleted before its reread is dropped.
func (r *Remote) complete(ctx context.Context, contacts []contact, budget *carddav.Budget) ([]contact, error) {
	kept := contacts[:0]
	for _, c := range contacts {
		if c.thin() {
			full, err := r.read(ctx, c.ID, budget)
			if errors.Is(err, msgraph.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			c = full
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// read gets one contact by its Graph ID with its extended properties, and
// charges every response, retries included, to the budget, if any.
func (r *Remote) read(ctx context.Context, id string, budget *carddav.Budget) (c contact, err error) {
	var body []byte
	err = r.call(ctx, func(ctx context.Context) (err error) {
		body, err = r.graph.GetRawMetered(ctx, r.base+"/me/contacts/"+url.PathEscape(id)+"?$expand="+expandUID(), pageBytes, charge(budget))
		return err
	})
	if err == nil {
		err = json.Unmarshal(body, &c)
	}
	return c, err
}

func (r *Remote) Get(ctx context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	c, book, found, err := r.lookup(ctx, href)
	if err != nil || !found {
		return store.CardDAVRemoteResource{Href: href}, err == nil, err
	}
	resource, err := r.resource(book, c, strings.HasPrefix(href, book+"/uid/"))
	if err == nil && resource.Href != href {
		return store.CardDAVRemoteResource{Href: href}, true, nil
	}
	return resource, false, err
}

// Put creates or updates a contact. Graph has no conditional create, so a
// create looks up the UID first. The gap between that read and the POST is
// open only to the same user's own writes.
func (r *Remote) Put(ctx context.Context, href string, body []byte, etag string, create bool) error {
	fields, dropped, err := mapVCard(body)
	if err != nil {
		return err
	}
	current, book, found, err := r.lookup(ctx, href)
	if err != nil {
		return unsent(err)
	}
	// The warning is logged once for each contact: again only when its
	// dropped lines differ from those of the vCard it last saved.
	_, before, _ := mapVCard([]byte(current.property(vcardProperty)))
	if len(dropped) > 0 && !slices.Equal(dropped, before) {
		slog.WarnContext(ctx, "Outlook holds one of each; extra lines are not published", "href", href, "dropped", dropped)
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
		if err := fitsGraph(fields); err != nil {
			return err
		}
		var created contact
		if err := r.call(ctx, func(ctx context.Context) error {
			response, err := r.graph.SendOnce(ctx, http.MethodPost, book, fields, "")
			if err == nil {
				err = json.Unmarshal(response, &created)
			}
			return err
		}); err != nil {
			return err
		}
		// Graph assigns the ID only now, so marking the original takes a
		// second write. The create stands without it, but until its next
		// update, an Outlook copy that outlives it takes over its publication.
		// displayName goes too, because Graph may recalculate it on an
		// update that leaves it out. If-Match skips the mark after an Outlook
		// edit, so the edit stands.
		mark := map[string]any{
			"displayName":                   fields.DisplayName,
			"singleValueExtendedProperties": []singleValueExtendedProperty{{ID: idProperty, Value: created.ID}},
		}
		_ = r.call(ctx, func(ctx context.Context) error {
			_, err := r.graph.SendOnce(ctx, http.MethodPatch, r.base+"/me/contacts/"+url.PathEscape(created.ID), mark, created.ETag)
			return err
		})
		return nil
	}
	if !found {
		// An update of a missing card fails its version check, as on a DAV
		// server, so the service records a conflict.
		return &carddav.StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	fields.Properties = fields.saved(body)
	if strings.HasPrefix(href, book+"/uid/") {
		// Only the UID href's owner is marked, so a copy updated through its
		// ID href stays a copy.
		fields.Properties = append(fields.Properties, singleValueExtendedProperty{ID: idProperty, Value: current.ID})
	}
	if err := fitsGraph(fields); err != nil {
		return err
	}
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
		return unsent(err)
	}
	if !found {
		return &carddav.StatusError{StatusCode: http.StatusNotFound}
	}
	if current.ETag != etag {
		return &carddav.StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	remove := func(ctx context.Context) error {
		_, err := r.graph.SendNoRetry(ctx, http.MethodDelete, r.base+"/me/contacts/"+url.PathEscape(current.ID), nil, "")
		return err
	}
	// A repeat is sent only while the ETag is unchanged, as for an update, so
	// an Outlook edit during the wait is not deleted. A 429 is not repeated
	// here: it pauses the connection, and the next attempt compares again.
	err = r.call(ctx, remove)
	if err != nil && unclear(err) {
		if after, _, found, lookupErr := r.lookup(ctx, href); lookupErr == nil && found && after.ETag == etag {
			err = r.call(ctx, remove)
		}
	}
	return err
}

func (r *Remote) CreateHref(collectionURL, uid string) (string, error) {
	if strings.TrimSpace(uid) == "" || !strings.HasPrefix(collectionURL, r.base+"/me/contactFolders/") ||
		!strings.HasSuffix(collectionURL, "/contacts") {
		return "", carddav.ErrUnsafeTarget
	}
	return collectionURL + "/uid/" + url.PathEscape(uid), nil
}

// unsent marks a failed read before a write as not sent, so the service
// plans the write again. A read that Graph answered with a status other than
// 408 keeps its status, which the service branches on; a 429 pauses the
// connection first.
func unsent(err error) error {
	if status, ok := errors.AsType[*carddav.StatusError](err); ok && status.StatusCode != http.StatusRequestTimeout {
		return err
	}
	return errors.Join(err, carddav.ErrWriteNotSent)
}

// fitsGraph refuses a write larger than Graph accepts, for example a card
// with a large photo in its saved vCard, before it is sent.
func fitsGraph(fields contact) error {
	body, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode Graph contact: %w", err)
	}
	if len(body) > writeBytes {
		return errors.Join(fmt.Errorf("graph contact write of %d bytes exceeds Graph's 4 MB limit", len(body)),
			carddav.ErrMicrosoftContactTooLarge, &carddav.StatusError{StatusCode: http.StatusRequestEntityTooLarge})
	}
	return nil
}

// call sends one Graph request behind the connection's retry gate, as the
// carddav.Remote contract requires for calls that send several requests. A
// request that the closed gate or the rate limiter held back was not sent.
func (r *Remote) call(ctx context.Context, request func(context.Context) error) error {
	sent := false
	err := carddav.GateRequest(ctx, func(ctx context.Context) error {
		sent = true
		return statusError(request(ctx))
	})
	if (!sent && err != nil) || errors.Is(err, msgraph.ErrNotSent) {
		return errors.Join(err, carddav.ErrWriteNotSent)
	}
	return err
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
	if errors.Is(err, msgraph.ErrUnauthorized) {
		// Graph refused the token, so only a new sign-in repairs the
		// connection. No HTTP status, so a pending write is kept.
		return errors.Join(err, carddav.ErrMicrosoftAuthorizationRequired)
	}
	for sentinel, code := range map[error]int{
		msgraph.ErrNotFound:           http.StatusNotFound,
		msgraph.ErrPreconditionFailed: http.StatusPreconditionFailed,
		msgraph.ErrForbidden:          http.StatusForbidden,
	} {
		if errors.Is(err, sentinel) {
			return errors.Join(err, &carddav.StatusError{StatusCode: code})
		}
	}
	if status, ok := errors.AsType[*msgraph.StatusError](err); ok && status.StatusCode == http.StatusRequestEntityTooLarge {
		return errors.Join(err, carddav.ErrMicrosoftContactTooLarge, &carddav.StatusError{StatusCode: status.StatusCode})
	}
	// Any other 4xx is a definitive rejection, for example a 400 for an
	// invalid property: Graph applied nothing.
	if status, ok := errors.AsType[*msgraph.StatusError](err); ok && status.StatusCode < http.StatusInternalServerError {
		return errors.Join(err, &carddav.StatusError{StatusCode: status.StatusCode})
	}
	return err
}
