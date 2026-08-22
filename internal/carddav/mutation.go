package carddav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
	"go.kenn.io/msgvault/internal/vcardmap"
)

type Mutation struct {
	PersonID int64
	Desired  bool
}

func (s *Service) PublishPerson(ctx context.Context, personID int64) error {
	return s.mutate(ctx, Mutation{PersonID: personID, Desired: true})
}

func (s *Service) UnpublishPerson(ctx context.Context, personID int64) error {
	return s.mutate(ctx, Mutation{PersonID: personID, Desired: false})
}

// ReconcilePublications runs in person-ID order. A person's failure does not
// prevent later people from being considered; an account-wide 429 gate stops
// the sweep immediately.
func (s *Service) ReconcilePublications(ctx context.Context) error {
	return s.reconcilePublications(ctx, nil)
}

// recoverPendingPublications resolves only ambiguous in-flight mutations.
// Sync calls it before pull so a successful remote write whose response was
// lost is not mistaken for a new remote edit. Settled desired publications
// remain in the normal post-pull reconciliation phase.
func (s *Service) recoverPendingPublications(ctx context.Context) (map[int64]bool, error) {
	ids, err := s.store.ListCardDAVPublicationPersonIDsContext(ctx)
	if err != nil {
		return nil, err
	}
	recovered := make(map[int64]bool)
	var failures []error
	for _, personID := range ids {
		publication, err := s.store.GetCardDAVPublicationContext(ctx, personID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if publication.PendingOperation == "" {
			continue
		}
		recovered[personID] = true
		err = s.mutate(ctx, Mutation{PersonID: personID, Desired: publication.Desired})
		if errors.Is(err, store.ErrCardDAVRetryAfter) || isStatus(err, http.StatusTooManyRequests) {
			return recovered, err
		}
		if errors.Is(err, ErrCardDAVConflictPending) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile CardDAV publication for person %d: %w", personID, err))
		}
	}
	return recovered, errors.Join(failures...)
}

func (s *Service) reconcilePublications(ctx context.Context, skip map[int64]bool) error {
	ids, err := s.store.ListCardDAVPublicationPersonIDsContext(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, personID := range ids {
		if skip[personID] {
			continue
		}
		publication, err := s.store.GetCardDAVPublicationContext(ctx, personID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		err = s.mutate(ctx, Mutation{PersonID: personID, Desired: publication.Desired})
		if errors.Is(err, store.ErrCardDAVRetryAfter) || isStatus(err, http.StatusTooManyRequests) {
			return err
		}
		if errors.Is(err, ErrCardDAVConflictPending) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile CardDAV publication for person %d: %w", personID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) mutate(ctx context.Context, mutation Mutation) error {
	if s == nil || s.store == nil || s.client == nil || mutation.PersonID <= 0 {
		return errors.New("CardDAV service is not configured")
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.client.operationTimeout)
	defer cancel()
	if err := s.store.CheckCardDAVRetryAfterContext(operationCtx); err != nil {
		return err
	}

	existing, publicationErr := s.store.GetCardDAVPublicationContext(operationCtx, mutation.PersonID)
	if publicationErr == nil && existing.PendingOperation != "" {
		if conflict, conflictErr := s.store.GetUnresolvedCardDAVConflictForMappingContext(
			operationCtx, existing.AddressBookID, existing.Href,
		); conflictErr == nil {
			return &ConflictError{ID: conflict.ID}
		} else if !errors.Is(conflictErr, store.ErrCardDAVConflictNotFound) {
			return conflictErr
		}
		if existing.Desired != mutation.Desired {
			return store.ErrCardDAVPublicationPending
		}
		existing.RecoveryOnly = true
		return s.executeMutation(operationCtx, existing)
	} else if publicationErr != nil && !errors.Is(publicationErr, store.ErrCardDAVPublicationNotFound) {
		return publicationErr
	}
	if !mutation.Desired && errors.Is(publicationErr, store.ErrCardDAVPublicationNotFound) {
		_, err := s.store.GetPersonContext(operationCtx, mutation.PersonID)
		return err
	}

	account, err := s.store.GetCardDAVAccountContext(operationCtx)
	if err != nil {
		return err
	}
	if account == nil {
		return store.ErrCardDAVNoWriteTarget
	}
	books, err := s.store.ListCardDAVAddressBooksContext(operationCtx)
	if err != nil {
		return err
	}
	book, ok := writeTarget(books)
	if !ok {
		return store.ErrCardDAVNoWriteTarget
	}
	person, err := s.store.GetPersonContext(operationCtx, mutation.PersonID)
	if err != nil {
		return err
	}
	resource, err := s.store.GetCardDAVResourceForPersonContext(operationCtx, book.ID, mutation.PersonID)
	if err != nil && !errors.Is(err, store.ErrCardDAVResourceNotFound) {
		return err
	}
	if errors.Is(err, store.ErrCardDAVResourceNotFound) {
		resource = nil
	}
	if resource != nil {
		if conflict, conflictErr := s.store.GetUnresolvedCardDAVConflictForMappingContext(
			operationCtx, resource.AddressBookID, resource.Href,
		); conflictErr == nil {
			return &ConflictError{ID: conflict.ID}
		} else if !errors.Is(conflictErr, store.ErrCardDAVConflictNotFound) {
			return conflictErr
		}
	}
	href, err := s.publicationHref(book.CanonicalURL, person.VCardUID)
	if err != nil {
		return err
	}
	if resource != nil {
		href = resource.Href
	}
	plan := store.CardDAVPublicationPlan{
		PersonID: mutation.PersonID, Desired: mutation.Desired,
		AddressBookID: book.ID, Href: href,
	}
	if mutation.Desired {
		body, localHash, err := s.renderPublicationCard(operationCtx, *person, book, resource)
		if err != nil {
			return err
		}
		semanticHash, err := SemanticHash(body)
		if err != nil {
			return err
		}
		plan.OutgoingBody, plan.OutgoingSemanticHash, plan.LocalHash = body, semanticHash, localHash
	}
	prepared, err := s.store.PrepareCardDAVPublicationContext(operationCtx, plan)
	if err != nil {
		return err
	}
	if prepared.Noop {
		return nil
	}
	return s.executeMutation(operationCtx, prepared)
}

func writeTarget(books []store.CardDAVAddressBook) (store.CardDAVAddressBook, bool) {
	for _, book := range books {
		if book.IsWriteTarget && book.IsSubscribed {
			return book, true
		}
	}
	return store.CardDAVAddressBook{}, false
}

func (s *Service) renderPublicationCard(
	ctx context.Context, person store.Person, book store.CardDAVAddressBook,
	resource *store.CardDAVResource,
) ([]byte, string, error) {
	snapshot, err := s.store.LoadPersonVCardSnapshotContext(ctx, person.ID)
	if err != nil {
		return nil, "", err
	}
	var envelope vcard.ResourceEnvelope
	version := publicationVersion(book.SupportedVCardVersions)
	if resource != nil {
		record, err := s.store.GetVCardResourceEnvelopeContext(ctx, fmt.Sprintf("carddav:%d", book.ID), resource.Href)
		if err != nil {
			return nil, "", err
		}
		envelope = record.ResourceEnvelope
		if envelope.RenderMetadata.StoredVersion == vcard.Version30 || envelope.RenderMetadata.StoredVersion == vcard.Version40 {
			version = envelope.RenderMetadata.StoredVersion
		}
	} else {
		href, err := s.publicationHref(book.CanonicalURL, person.VCardUID)
		if err != nil {
			return nil, "", err
		}
		fullName := person.VCardUID
		if person.DisplayName != nil && strings.TrimSpace(*person.DisplayName) != "" {
			fullName = strings.TrimSpace(*person.DisplayName)
		}
		raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + vcard.EscapeText(person.VCardUID) +
			"\r\nFN:" + vcard.EscapeText(fullName) + "\r\nEND:VCARD\r\n")
		envelope, err = vcard.ParseResourceEnvelope(raw)
		if err != nil {
			return nil, "", err
		}
		envelope.SourceRef = fmt.Sprintf("carddav:%d", book.ID)
		envelope.SourceResourceUID = href
		envelope.Href = envelope.SourceResourceUID
		envelope.CanonicalPersonUID = person.VCardUID
	}
	prepared, err := vcardmap.ProjectPersonEnvelope(*snapshot, envelope)
	if err != nil {
		return nil, "", fmt.Errorf("project person for CardDAV publication: %w", err)
	}
	body, err := prepared.RenderView(version)
	if err != nil {
		return nil, "", err
	}
	body, err = stripServerOwnedProperties(body, version)
	if err != nil {
		return nil, "", err
	}
	return body, snapshot.Fingerprint, nil
}

func (s *Service) publicationHref(collectionURL, uid string) (string, error) {
	if s == nil || s.client == nil || strings.TrimSpace(uid) == "" {
		return "", ErrUnsafeTarget
	}
	collection, err := url.Parse(collectionURL)
	if err != nil || !validHTTPURL(collection) || !sameOrigin(s.client.origin, collection) {
		return "", fmt.Errorf("CardDAV publication collection: %w", ErrUnsafeTarget)
	}
	child := *collection
	child.RawQuery = ""
	child.ForceQuery = false
	child.Fragment = ""
	escapedBase := strings.TrimSuffix(collection.EscapedPath(), "/") + "/"
	child.Path = strings.TrimSuffix(collection.Path, "/") + "/" + uid + ".vcf"
	child.RawPath = escapedBase + url.PathEscape(uid) + ".vcf"
	if !sameOrigin(collection, &child) || !sameOrigin(s.client.origin, &child) {
		return "", fmt.Errorf("CardDAV publication href: %w", ErrUnsafeTarget)
	}
	return child.String(), nil
}

func publicationVersion(advertised []string) vcard.Version {
	versions := slices.Clone(advertised)
	slices.Sort(versions)
	versions = slices.Compact(versions)
	if len(versions) == 1 && versions[0] == string(vcard.Version40) {
		return vcard.Version40
	}
	return vcard.Version30
}

func stripServerOwnedProperties(body []byte, version vcard.Version) ([]byte, error) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return nil, err
	}
	edits := make([]vcard.PropertyEdit, 0)
	for _, occurrence := range envelope.PropertyTree {
		if serverOwnedProperties[strings.ToUpper(occurrence.Property.Name)] {
			edits = append(edits, vcard.PropertyEdit{Identity: occurrence.Identity, Delete: true})
		}
	}
	if len(edits) > 0 {
		envelope, err = envelope.MergeProperties(edits)
		if err != nil {
			return nil, err
		}
	}
	return envelope.RenderView(version)
}

func (s *Service) executeMutation(ctx context.Context, pending *store.CardDAVPublication) error {
	books, err := s.store.ListCardDAVAddressBooksContext(ctx)
	if err != nil {
		return err
	}
	book, ok := findCardDAVBook(books, pending.AddressBookID)
	conflictScoped := pending.ResolutionConflictID != 0
	if !ok || !book.IsSubscribed || (!book.IsWriteTarget && !conflictScoped) {
		return store.ErrCardDAVNoWriteTarget
	}
	if pending.RecoveryOnly {
		resolutionConflictID := pending.ResolutionConflictID
		var refreshed *store.CardDAVPublication
		var err error
		if resolutionConflictID != 0 && pending.PersonID == 0 {
			refreshed, err = s.store.RefreshCardDAVConflictMutationFenceContext(ctx, resolutionConflictID)
		} else {
			refreshed, err = s.store.RefreshCardDAVPublicationFenceContext(ctx, pending.PersonID)
		}
		if err != nil {
			return err
		}
		refreshed.RecoveryOnly = true
		refreshed.ResolutionConflictID = resolutionConflictID
		pending = refreshed
		if pending.PendingOperation == store.CardDAVMutationCreate {
			return s.recoverCreate(ctx, pending)
		}
		remote, tombstone, err := s.fetchCanonical(ctx, pending.Href)
		if err != nil {
			return err
		}
		err = s.commitCardDAVCanonicalMutation(ctx, store.CardDAVCanonicalMutation{
			Publication: *pending, Remote: remote, Tombstone: tombstone,
		})
		if errors.Is(err, store.ErrCardDAVPublicationMismatch) {
			return s.captureCardDAVMutationConflict(ctx, pending, remote, tombstone, true)
		}
		return err
	}

	request := Request{URL: pending.Href, ETag: pending.RemoteETag}
	switch pending.PendingOperation {
	case store.CardDAVMutationCreate:
		request.Method, request.Body, request.Create = http.MethodPut, pending.OutgoingBody, true
	case store.CardDAVMutationUpdate:
		request.Method, request.Body = http.MethodPut, pending.OutgoingBody
	case store.CardDAVMutationDelete:
		request.Method = http.MethodDelete
	default:
		return store.ErrCardDAVInvalidPlan
	}
	_, err = s.doRequest(ctx, request)
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests {
			if pending.PersonID == 0 && pending.ResolutionConflictID != 0 {
				if rollbackErr := s.store.RollbackCardDAVConflictMutationContext(ctx, pending); rollbackErr != nil {
					return errors.Join(err, rollbackErr)
				}
				return err
			}
			gate := time.Now().Add(status.RetryAfter).UTC()
			if rollbackErr := s.store.RollbackCardDAVPublicationThrottleContext(ctx, pending, gate); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		canonicalTombstone := pending.PendingOperation == store.CardDAVMutationDelete &&
			isAbsentStatus(err)
		if canonicalTombstone {
			// The required canonical read below proves the same tombstone.
			return s.commitCanonical(ctx, pending)
		}
		if pending.PendingOperation == store.CardDAVMutationCreate && isStatus(err, http.StatusPreconditionFailed) {
			return s.commitCanonical(ctx, pending)
		}
		if isStatus(err, http.StatusPreconditionFailed) {
			remote, tombstone, fetchErr := s.fetchCanonical(ctx, pending.Href)
			if fetchErr != nil {
				return fetchErr
			}
			if pending.PendingOperation == store.CardDAVMutationDelete && tombstone {
				return s.commitCardDAVCanonicalMutation(ctx, store.CardDAVCanonicalMutation{
					Publication: *pending, Remote: remote, Tombstone: true,
				})
			}
			return s.captureCardDAVMutationConflict(ctx, pending, remote, tombstone, false)
		}
		if isDefinitiveMutationRejection(err) {
			if rollbackErr := s.rollbackDefinitiveMutation(ctx, pending); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		// Transport ambiguity and conditional failures retain the exact
		// intent. Mapped recovery never replays this request.
		return err
	}
	return s.commitCanonical(ctx, pending)
}

func (s *Service) recoverCreate(ctx context.Context, pending *store.CardDAVPublication) error {
	remote, tombstone, err := s.fetchCanonical(ctx, pending.Href)
	if err != nil {
		return err
	}
	if !tombstone {
		err := s.store.CommitCardDAVPublicationContext(ctx, store.CardDAVCanonicalMutation{
			Publication: *pending, Remote: remote,
		})
		if errors.Is(err, store.ErrCardDAVPublicationMismatch) {
			return s.captureCardDAVCreateConflict(ctx, pending, remote)
		}
		return err
	}
	// A canonical 404 makes another conditional create safe. Do not gate this
	// retry durably: a transient PUT failure or process exit would otherwise
	// strand the pending publication forever. Concurrent attempts are still
	// fenced by If-None-Match: * and the publication mutation revision.
	_, err = s.doRequest(ctx, Request{
		Method: http.MethodPut, URL: pending.Href,
		Body: pending.OutgoingBody, Create: true,
	})
	if err != nil && !isStatus(err, http.StatusPreconditionFailed) {
		if isStatus(err, http.StatusTooManyRequests) {
			if status, ok := errors.AsType[*StatusError](err); ok {
				gate := time.Now().Add(status.RetryAfter).UTC()
				_ = s.store.RollbackCardDAVPublicationThrottleContext(ctx, pending, gate)
			}
		}
		if isDefinitiveMutationRejection(err) {
			if rollbackErr := s.rollbackDefinitiveMutation(ctx, pending); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
		}
		return err
	}
	return s.commitCanonical(ctx, pending)
}

func (s *Service) rollbackDefinitiveMutation(
	ctx context.Context, pending *store.CardDAVPublication,
) error {
	if pending.PersonID == 0 && pending.ResolutionConflictID != 0 {
		return s.store.RollbackCardDAVConflictMutationContext(ctx, pending)
	}
	return s.store.RollbackCardDAVPublicationContext(ctx, pending)
}

func isDefinitiveMutationRejection(err error) bool {
	var status *StatusError
	return errors.As(err, &status) && status.StatusCode >= http.StatusBadRequest &&
		status.StatusCode < http.StatusInternalServerError &&
		status.StatusCode != http.StatusRequestTimeout &&
		status.StatusCode != http.StatusTooManyRequests &&
		status.StatusCode != http.StatusPreconditionFailed
}

func (s *Service) commitCanonical(ctx context.Context, pending *store.CardDAVPublication) error {
	remote, tombstone, err := s.fetchCanonical(ctx, pending.Href)
	if err != nil {
		return err
	}
	err = s.commitCardDAVCanonicalMutation(ctx, store.CardDAVCanonicalMutation{
		Publication: *pending, Remote: remote, Tombstone: tombstone,
	})
	if errors.Is(err, store.ErrCardDAVPublicationMismatch) {
		if pending.PendingOperation == store.CardDAVMutationCreate && !tombstone {
			return s.captureCardDAVCreateConflict(ctx, pending, remote)
		}
		return s.captureCardDAVMutationConflict(ctx, pending, remote, tombstone, true)
	}
	return err
}

func (s *Service) captureCardDAVCreateConflict(
	ctx context.Context, pending *store.CardDAVPublication, remote store.CardDAVRemoteResource,
) error {
	if pending.MappingRevision > 0 {
		return s.recordPublicationConflict(ctx, pending, remote, false, true)
	}
	fenced, err := s.store.FenceCardDAVCreateCollisionContext(ctx, *pending, remote)
	if err != nil {
		return err
	}
	return s.recordPublicationConflict(ctx, fenced, remote, false, true)
}

func (s *Service) commitCardDAVCanonicalMutation(
	ctx context.Context, input store.CardDAVCanonicalMutation,
) error {
	if input.Publication.ResolutionConflictID != 0 && input.Publication.PersonID == 0 {
		return s.store.CommitCardDAVConflictLocalTombstoneContext(ctx, input)
	}
	return s.store.CommitCardDAVPublicationContext(ctx, input)
}

func (s *Service) captureCardDAVMutationConflict(
	ctx context.Context, pending *store.CardDAVPublication,
	remote store.CardDAVRemoteResource, tombstone, retainOversizeIntent bool,
) error {
	if pending.ResolutionConflictID == 0 || pending.PersonID != 0 {
		return s.recordPublicationConflict(ctx, pending, remote, tombstone, retainOversizeIntent)
	}
	if tombstone {
		return s.commitCardDAVCanonicalMutation(ctx, store.CardDAVCanonicalMutation{
			Publication: *pending, Remote: remote, Tombstone: true,
		})
	}
	conflict, err := s.store.ResetCardDAVConflictLocalTombstoneContext(
		ctx, pending.ResolutionConflictID, remote)
	if err != nil {
		return err
	}
	return &ConflictError{ID: conflict.ID}
}

func (s *Service) fetchCanonical(ctx context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	response, err := s.doRequest(ctx, Request{Method: http.MethodGet, URL: href})
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

func (s *Service) doRequest(ctx context.Context, request Request) (*Response, error) {
	if err := s.store.CheckCardDAVRetryAfterContext(ctx); err != nil {
		return nil, err
	}
	response, err := s.client.Do(ctx, request)
	var status *StatusError
	if errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests {
		gate := time.Now().Add(status.RetryAfter).UTC()
		if gateErr := s.store.SetCardDAVRetryAfterContext(ctx, gate); gateErr != nil {
			return response, errors.Join(err, gateErr)
		}
	}
	return response, err
}

func isStatus(err error, code int) bool {
	var status *StatusError
	return errors.As(err, &status) && status.StatusCode == code
}

func isAbsentStatus(err error) bool {
	return isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusGone)
}

func isAbsentStatusCode(code int) bool {
	return code == http.StatusNotFound || code == http.StatusGone
}
