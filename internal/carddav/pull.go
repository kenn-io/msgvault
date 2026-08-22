package carddav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

var (
	ErrTruncatedSnapshot  = errors.New("CardDAV snapshot was truncated")
	ErrSyncTokenCycle     = errors.New("CardDAV sync token continuation cycle")
	ErrIncompleteMultiget = errors.New("CardDAV multiget response was incomplete")
)

const (
	maxSyncPages   = 100
	multigetBatch  = 100
	maxSyncMembers = 50_000
)

type Service struct {
	store  *store.Store
	client *Client
}

func NewService(st *store.Store, client *Client) *Service {
	return &Service{store: st, client: client}
}

type SyncOptions struct {
	Full bool
}

type SyncResult struct {
	Books   int `json:"books"`
	Created int `json:"created"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
}

// Sync fetches complete network plans before entering the store's fenced
// apply transaction. A stale plan is re-fetched once; a second stale result is
// returned rather than retried blindly.
func (s *Service) Sync(ctx context.Context, options SyncOptions) (SyncResult, error) {
	if s == nil || s.store == nil || s.client == nil {
		return SyncResult{}, errors.New("CardDAV service is not configured")
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.client.operationTimeout)
	defer cancel()
	if err := s.store.CheckCardDAVRetryAfterContext(operationCtx); err != nil {
		return SyncResult{}, err
	}
	// Resolve ambiguous publication outcomes before interpreting remote changes.
	// Otherwise a successful write whose response timed out can be misclassified
	// as an edit/edit conflict by the pull that follows.
	recovered, err := s.recoverPendingPublications(operationCtx)
	if err != nil {
		return SyncResult{}, err
	}
	var failures []error
	budget := &operationBudget{remaining: s.client.operationBytes}
	var total SyncResult
	books, err := s.store.ListCardDAVAddressBooksContext(operationCtx)
	if err != nil {
		return total, err
	}
	for _, initial := range books {
		if !initial.IsSubscribed && !initial.IsLookupSource {
			continue
		}
		applied, err := s.syncBook(operationCtx, initial.ID, options, budget)
		if err != nil {
			bookErr := fmt.Errorf("sync CardDAV address book %d: %w", initial.ID, err)
			if isGlobalSyncFailure(operationCtx, err) {
				return total, errors.Join(append(failures, bookErr)...)
			}
			failures = append(failures, bookErr)
			continue
		}
		if applied == nil {
			continue
		}
		total.Books++
		total.Created += applied.Created
		total.Updated += applied.Updated
		total.Removed += applied.Removed
	}
	if err := s.reconcilePublications(operationCtx, recovered); err != nil {
		failures = append(failures, err)
		if isGlobalSyncFailure(operationCtx, err) {
			return total, errors.Join(failures...)
		}
	}
	if _, err := s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now()); err != nil {
		failures = append(failures, err)
	}
	return total, errors.Join(failures...)
}

func isGlobalSyncFailure(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrOperationLimit) || errors.Is(err, store.ErrCardDAVRetryAfter) {
		return true
	}
	var status *StatusError
	return errors.As(err, &status) &&
		(status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusTooManyRequests)
}

func (s *Service) syncBook(
	ctx context.Context, bookID int64, options SyncOptions, budget *operationBudget,
) (*store.CardDAVApplyResult, error) {
	state := &bookSyncState{}
	for attempt := range 2 {
		account, err := s.store.GetCardDAVAccountContext(ctx)
		if err != nil {
			return nil, err
		}
		if account == nil {
			return nil, store.ErrCardDAVStalePlan
		}
		books, err := s.store.ListCardDAVAddressBooksContext(ctx)
		if err != nil {
			return nil, err
		}
		book, ok := findCardDAVBook(books, bookID)
		if !ok {
			return nil, store.ErrCardDAVStalePlan
		}
		if !book.IsSubscribed && !book.IsLookupSource {
			return nil, nil //nolint:nilnil // A deliberately ignored book produces no apply result and no error.
		}
		plan, err := s.fetchBookPlan(ctx, *account, book, options, budget, state)
		if err != nil {
			return nil, err
		}
		if err := s.prepareSyncConflicts(ctx, book, &plan); err != nil {
			return nil, err
		}
		applied, err := s.store.ApplyCardDAVSyncPlanContext(ctx, plan)
		if err == nil {
			return applied, nil
		}
		if !errors.Is(err, store.ErrCardDAVStalePlan) || attempt == 1 {
			return nil, err
		}
	}
	return nil, store.ErrCardDAVStalePlan
}

type bookSyncState struct {
	invalidTokenReconciled bool
}

func findCardDAVBook(books []store.CardDAVAddressBook, id int64) (store.CardDAVAddressBook, bool) {
	for _, book := range books {
		if book.ID == id {
			return book, true
		}
	}
	return store.CardDAVAddressBook{}, false
}

func (s *Service) fetchBookPlan(
	ctx context.Context, account store.CardDAVAccount, book store.CardDAVAddressBook,
	options SyncOptions, budget *operationBudget, state *bookSyncState,
) (store.CardDAVSyncPlan, error) {
	base := store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision,
	}
	token := book.SyncToken
	if options.Full || book.NeedsFullReconcile {
		token = ""
	}
	if book.SupportsSyncCollection {
		plan, err := s.fetchSyncCollection(ctx, book, token, budget)
		if err == nil {
			plan.AddressBookID = base.AddressBookID
			plan.ConnectionGeneration = base.ConnectionGeneration
			plan.SyncRevision = base.SyncRevision
			plan.CompletesFullReconcile = options.Full || book.NeedsFullReconcile
			return plan, nil
		}
		var status *StatusError
		switch {
		case errors.As(err, &status) && status.Precondition == "valid-sync-token" &&
			token != "" && !state.invalidTokenReconciled:
			state.invalidTokenReconciled = true
			plan, retryErr := s.fetchSyncCollection(ctx, book, "", budget)
			if retryErr != nil {
				return store.CardDAVSyncPlan{}, retryErr
			}
			plan.AddressBookID = base.AddressBookID
			plan.ConnectionGeneration = base.ConnectionGeneration
			plan.SyncRevision = base.SyncRevision
			plan.CompletesFullReconcile = options.Full || book.NeedsFullReconcile
			return plan, nil
		case errors.As(err, &status) && (status.StatusCode == http.StatusMethodNotAllowed || status.StatusCode == http.StatusNotImplemented):
			// Capability advertisements are hints. A standards-compliant snapshot
			// is the bounded downgrade when sync-collection is unavailable.
		default:
			return store.CardDAVSyncPlan{}, err
		}
	}
	plan, err := s.fetchSnapshot(ctx, book, budget)
	if err != nil {
		return store.CardDAVSyncPlan{}, err
	}
	plan.AddressBookID = base.AddressBookID
	plan.ConnectionGeneration = base.ConnectionGeneration
	plan.SyncRevision = base.SyncRevision
	plan.CompletesFullReconcile = options.Full || book.NeedsFullReconcile
	return plan, nil
}

func (s *Service) do(ctx context.Context, request Request, budget *operationBudget) (*Response, error) {
	response, err := s.doRequest(ctx, request)
	if response != nil {
		if budgetErr := budget.consume(response); budgetErr != nil {
			return nil, budgetErr
		}
	}
	return response, err
}

func (s *Service) fetchSyncCollection(
	ctx context.Context, book store.CardDAVAddressBook, token string, budget *operationBudget,
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
		depth := 1
		response, err := s.do(ctx, Request{Method: "REPORT", URL: book.CanonicalURL, Depth: &depth, Body: body}, budget)
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
		changed, removed, continuation, truncated, err := s.parseSyncPage(ctx, collection, multiStatus)
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
	resources := make([]store.CardDAVRemoteResource, 0, len(changed))
	if book.SupportsMultiget {
		for offset := 0; offset < len(changed); offset += multigetBatch {
			end := min(offset+multigetBatch, len(changed))
			cards, missing, err := s.fetchMultiget(ctx, collection, changed[offset:end], budget)
			if isStatus(err, http.StatusMethodNotAllowed) || isStatus(err, http.StatusNotImplemented) {
				cards, missing, err = s.fetchMembersIndividually(ctx, collection, changed[offset:], budget)
				if err != nil {
					return store.CardDAVSyncPlan{}, err
				}
				resources = append(resources, cards...)
				removed = append(removed, missing...)
				break
			}
			if err != nil {
				return store.CardDAVSyncPlan{}, err
			}
			for index := range cards {
				resolved, err := url.Parse(cards[index].Href)
				if err != nil {
					return store.CardDAVSyncPlan{}, ErrIncompleteMultiget
				}
				cards[index].Href = canonicalDAVURLIdentity(resolved)
			}
			resources = append(resources, cards...)
			removed = append(removed, missing...)
		}
	} else {
		cards, missing, err := s.fetchMembersIndividually(ctx, collection, changed, budget)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		resources = append(resources, cards...)
		removed = append(removed, missing...)
	}
	return store.CardDAVSyncPlan{
		ReplaceAll: token == "", NextSyncToken: nextToken,
		Upserts: resources, RemovedHrefs: removed,
	}, nil
}

func (s *Service) fetchMembersIndividually(
	ctx context.Context, collection *url.URL, hrefs []string, budget *operationBudget,
) ([]store.CardDAVRemoteResource, []string, error) {
	resources := make([]store.CardDAVRemoteResource, 0, len(hrefs))
	missing := make([]string, 0)
	for _, href := range hrefs {
		response, err := s.do(ctx, Request{Method: http.MethodGet, URL: href}, budget)
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
		if _, err := s.resolveMemberHref(ctx, collection, effective.String(), false); err != nil {
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

func (s *Service) parseSyncPage(
	ctx context.Context, collection *url.URL, multiStatus MultiStatus,
) ([]string, []string, string, bool, error) {
	if multiStatus.SyncToken == "" {
		return nil, nil, "", false, errors.New("CardDAV sync response lacks a usable sync token")
	}
	changed, removed := []string{}, []string{}
	seen := map[string]bool{}
	truncated := false
	for _, davResponse := range multiStatus.Responses {
		resolved, err := s.resolveMemberHref(ctx, collection, davResponse.Href, true)
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

func (s *Service) fetchMultiget(
	ctx context.Context, collection *url.URL, hrefs []string, budget *operationBudget,
) ([]store.CardDAVRemoteResource, []string, error) {
	body, err := AddressbookMultigetBody([]PropertyName{GetETagProperty, AddressDataProperty}, hrefs)
	if err != nil {
		return nil, nil, err
	}
	depth := 0
	response, err := s.do(ctx, Request{Method: "REPORT", URL: collection.String(), Depth: &depth, Body: body}, budget)
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
		resolved, err := s.resolveMemberHref(ctx, collection, davResponse.Href, false)
		if err != nil {
			return nil, nil, err
		}
		identity := canonicalDAVURLIdentity(resolved)
		href := resolved.String()
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

func (s *Service) fetchSnapshot(
	ctx context.Context, book store.CardDAVAddressBook, budget *operationBudget,
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
	response, err := s.do(ctx, Request{Method: "REPORT", URL: book.CanonicalURL, Depth: &depth, Body: body}, budget)
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
		resolved, err := s.resolveMemberHref(ctx, collection, davResponse.Href, true)
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

func (s *Service) resolveMemberHref(
	ctx context.Context, collection *url.URL, href string, allowCollection bool,
) (*url.URL, error) {
	resolved, err := s.client.ValidateChildHref(ctx, collection, href)
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

func parseRemoteResource(href, etag string, body []byte) (store.CardDAVRemoteResource, error) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, fmt.Errorf("parse remote CardDAV vCard: %w", err)
	}
	semanticHash, err := SemanticHash(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	resource := store.CardDAVRemoteResource{
		Href: href, RemoteETag: etag, RemoteBody: append([]byte(nil), body...),
		SemanticHash: semanticHash,
	}
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		identity := cardDAVVCardIdentity(occurrence)
		switch strings.ToUpper(property.Name) {
		case "UID":
			if resource.RemoteUID == "" {
				resource.RemoteUID = strings.TrimSpace(property.RawValue)
			}
		case "FN":
			if resource.DisplayName == "" {
				value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
				if err != nil {
					return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV FN: %w", err)
				}
				resource.DisplayName = strings.TrimSpace(value)
				resource.DisplayNameIdentity = identity
			}
		case "EMAIL":
			value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
			if err != nil {
				return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV EMAIL: %w", err)
			}
			value = strings.TrimSpace(trimPrefixFold(value, "mailto:"))
			if value != "" {
				resource.Emails = append(resource.Emails, value)
				resource.EmailIdentities = append(resource.EmailIdentities, identity)
			}
		case "TEL":
			value, err := cardDAVPropertyValue(envelope.RenderMetadata.StoredVersion, property)
			if err != nil {
				return store.CardDAVRemoteResource{}, fmt.Errorf("decode CardDAV TEL: %w", err)
			}
			value = strings.TrimSpace(trimPrefixFold(value, "tel:"))
			if value != "" {
				resource.Phones = append(resource.Phones, value)
				resource.PhoneIdentities = append(resource.PhoneIdentities, identity)
			}
		}
	}
	return resource, nil
}

func cardDAVPropertyValue(version vcard.Version, property vcard.Property) (string, error) {
	valueType := ""
	for _, parameter := range property.ParametersNamed("VALUE") {
		if len(parameter.Values) > 0 {
			valueType = strings.ToLower(strings.TrimSpace(parameter.Values[0].Decoded))
			break
		}
	}
	name := strings.ToUpper(property.Name)
	isText := valueType == "text" || valueType == "" &&
		(name != "TEL" || version != vcard.Version40)
	if !isText {
		return property.RawValue, nil
	}
	return vcard.UnescapeText(property.RawValue)
}

func cardDAVVCardIdentity(occurrence vcard.PropertyOccurrence) store.VCardIdentity {
	identity := store.VCardIdentity{
		Property: strings.ToUpper(occurrence.Property.Name),
		PropID:   occurrence.Identity.PropID,
		PID:      append([]string(nil), occurrence.Identity.PID...),
		AltID:    occurrence.Identity.AltID,
	}
	if occurrence.Identity.Group != "" {
		group := occurrence.Identity.Group
		identity.Group = &group
	}
	return identity
}

func trimPrefixFold(value, prefix string) string {
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return value[len(prefix):]
	}
	return value
}

// SyncCollectionBody builds the RFC 6578 level-1 incremental request.
func SyncCollectionBody(token string) ([]byte, error) {
	var body bytes.Buffer
	encoder := xml.NewEncoder(&body)
	root := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "sync-collection"}}
	if err := encoder.EncodeToken(root); err != nil {
		return nil, fmt.Errorf("encode CardDAV sync root: %w", err)
	}
	fields := []struct {
		name, text string
	}{{"sync-token", token}, {"sync-level", "1"}}
	for _, value := range fields {
		start := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: value.name}}
		if err := encoder.EncodeToken(start); err != nil {
			return nil, fmt.Errorf("encode CardDAV sync field %s: %w", value.name, err)
		}
		if err := encoder.EncodeToken(xml.CharData(value.text)); err != nil {
			return nil, fmt.Errorf("encode CardDAV sync value %s: %w", value.name, err)
		}
		if err := encoder.EncodeToken(start.End()); err != nil {
			return nil, fmt.Errorf("close CardDAV sync field %s: %w", value.name, err)
		}
	}
	prop := xml.StartElement{Name: xml.Name{Space: davNamespace, Local: "prop"}}
	if err := encoder.EncodeToken(prop); err != nil {
		return nil, fmt.Errorf("encode CardDAV sync properties: %w", err)
	}
	if err := encodeEmptyElement(encoder, xml.Name{Space: davNamespace, Local: "getetag"}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(prop.End()); err != nil {
		return nil, fmt.Errorf("close CardDAV sync properties: %w", err)
	}
	if err := encoder.EncodeToken(root.End()); err != nil {
		return nil, fmt.Errorf("close CardDAV sync root: %w", err)
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush CardDAV sync XML: %w", err)
	}
	return body.Bytes(), nil
}
