package carddav

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type ResolutionChoice string

const (
	ResolutionKeepLocal  ResolutionChoice = "keep_local"
	ResolutionKeepRemote ResolutionChoice = "keep_remote"
)

var (
	ErrInvalidResolutionChoice = errors.New("invalid CardDAV conflict resolution choice")
	ErrCardDAVConflictPending  = errors.New("CardDAV mapping has an unresolved conflict")
)

type ConflictError struct {
	ID int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: conflict %d", ErrCardDAVConflictPending, e.ID)
}

func (e *ConflictError) Unwrap() error { return ErrCardDAVConflictPending }

func (s *Service) ListConflictViews(ctx context.Context) ([]ConflictListItem, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("CardDAV service is not configured")
	}
	headers, err := s.store.ListCardDAVConflictHeadersContext(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]ConflictListItem, 0, len(headers))
	for _, header := range headers {
		localState, remoteState := ConflictSidePresent, ConflictSidePresent
		if header.LocalTombstone {
			localState = ConflictSideDeleted
		}
		if header.RemoteTombstone {
			remoteState = ConflictSideDeleted
		}
		items = append(items, ConflictListItem{
			ID: header.ID, AddressBook: publicAddressBookIdentity(header.AddressBookID, header.AddressBookName),
			Status: header.Status, LocalState: localState, RemoteState: remoteState,
			AllowedResolutions: allowedConflictResolutions(header.Status), UpdatedAt: header.UpdatedAt,
		})
	}
	return items, nil
}

func (s *Service) GetConflictView(ctx context.Context, id int64) (*ConflictDetail, error) {
	if s == nil || s.store == nil || id <= 0 {
		return nil, errors.New("CardDAV service is not configured")
	}
	source, err := s.store.GetCardDAVConflictDetailSourceContext(ctx, id)
	if err != nil {
		return nil, err
	}
	base := emptyContactSummary(ConflictSideUnavailable)
	if source.BaseAvailable {
		base = projectConflictContact(source.BaseBody, false)
	}
	return &ConflictDetail{
		ID: source.ID, AddressBook: publicAddressBookIdentity(source.AddressBookID, source.AddressBookName),
		Status: source.Status, Resolution: source.Resolution, Base: base,
		Local:              projectConflictContact(source.LocalBody, source.LocalTombstone),
		Remote:             projectConflictContact(source.RemoteBody, source.RemoteTombstone),
		AllowedResolutions: allowedConflictResolutions(source.Status),
		CreatedAt:          source.CreatedAt, UpdatedAt: source.UpdatedAt, ResolvedAt: source.ResolvedAt,
	}, nil
}

// ListConflicts retains the internal mutation-evidence read used by CardDAV's
// own reconciliation tests and workflows. Browser APIs use ListConflictViews.
func (s *Service) ListConflicts(ctx context.Context) ([]store.CardDAVConflict, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("CardDAV service is not configured")
	}
	return s.store.ListCardDAVConflictsContext(ctx, true)
}

func (s *Service) GetConflict(ctx context.Context, id int64) (*store.CardDAVConflict, error) {
	if s == nil || s.store == nil || id <= 0 {
		return nil, errors.New("CardDAV service is not configured")
	}
	return s.store.GetCardDAVConflictContext(ctx, id)
}

func allowedConflictResolutions(status store.CardDAVConflictStatus) []ResolutionChoice {
	if status == store.CardDAVConflictUnresolved {
		return []ResolutionChoice{ResolutionKeepLocal, ResolutionKeepRemote}
	}
	return []ResolutionChoice{}
}

func (s *Service) ResolveConflict(ctx context.Context, id int64, choice ResolutionChoice) error {
	if choice != ResolutionKeepLocal && choice != ResolutionKeepRemote {
		return ErrInvalidResolutionChoice
	}
	if s == nil || s.store == nil || s.client == nil || id <= 0 {
		return errors.New("CardDAV service is not configured")
	}
	conflict, err := s.store.GetCardDAVConflictContext(ctx, id)
	if err != nil {
		return err
	}
	if conflict.Status != store.CardDAVConflictUnresolved {
		return store.ErrCardDAVConflictStale
	}
	if choice == ResolutionKeepRemote {
		operationCtx, cancel := context.WithTimeout(ctx, s.client.operationTimeout)
		defer cancel()
		remote, tombstone, fetchErr := s.fetchCanonical(operationCtx, conflict.Href)
		if fetchErr != nil {
			return fetchErr
		}
		if tombstone != conflict.RemoteTombstone ||
			(!tombstone && remote.RemoteETag != conflict.RemoteETag) {
			return store.ErrCardDAVConflictStale
		}
		_, err = s.store.ResolveCardDAVConflictRemoteContext(operationCtx, store.CardDAVConflictRemoteResolution{
			ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
			Remote: remote, RemoteTombstone: tombstone,
		})
		if err != nil {
			return err
		}
		_, err = s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now())
		return err
	}
	return s.resolveConflictKeepLocal(ctx, conflict)
}

func (s *Service) prepareSyncConflicts(
	ctx context.Context, book store.CardDAVAddressBook, plan *store.CardDAVSyncPlan,
) error {
	resources, err := s.store.ListCardDAVResourcesContext(ctx, book.ID)
	if err != nil {
		return err
	}
	byHref := make(map[string]store.CardDAVResource, len(resources))
	byRemoteUID := make(map[string][]store.CardDAVResource, len(resources))
	for _, resource := range resources {
		byHref[resource.Href] = resource
		if resource.RemoteUID != "" {
			byRemoteUID[resource.RemoteUID] = append(byRemoteUID[resource.RemoteUID], resource)
		}
	}
	removed := make(map[string]bool, len(plan.RemovedHrefs))
	for _, href := range plan.RemovedHrefs {
		removed[href] = true
	}
	seen := make(map[string]bool, len(plan.Upserts))
	for _, remote := range plan.Upserts {
		seen[remote.Href] = true
	}
	changed := make(map[string]*store.CardDAVRemoteResource, len(plan.Upserts))
	claimedPreviousHrefs := make(map[string]bool)
	for index := range plan.Upserts {
		remote := &plan.Upserts[index]
		if _, exists := byHref[remote.Href]; !exists && remote.RemoteUID != "" {
			matches := byRemoteUID[remote.RemoteUID]
			if len(matches) == 1 && !claimedPreviousHrefs[matches[0].Href] {
				old := matches[0]
				eligible := removed[old.Href] || plan.ReplaceAll && !seen[old.Href]
				if eligible {
					remote.PreviousHref = old.Href
					claimedPreviousHrefs[old.Href] = true
					delete(byHref, old.Href)
					byHref[remote.Href] = old
				}
			}
		}
		changed[remote.Href] = remote
	}
	if plan.ReplaceAll {
		for href := range byHref {
			if _, exists := changed[href]; !exists {
				removed[href] = true
			}
		}
	}
	for href, remote := range changed {
		mapping, exists := byHref[href]
		if !exists {
			continue
		}
		capture, needed, err := s.prepareMappingConflict(ctx, book, mapping, remote, false)
		if err != nil {
			return err
		}
		if needed {
			if remote.PreviousHref != "" {
				capture.Href = remote.Href
			}
			plan.Conflicts = append(plan.Conflicts, capture)
		}
	}
	for href := range removed {
		mapping, exists := byHref[href]
		if !exists {
			continue
		}
		capture, needed, err := s.prepareMappingConflict(ctx, book, mapping, nil, true)
		if err != nil {
			return err
		}
		if needed {
			plan.Conflicts = append(plan.Conflicts, capture)
		}
	}
	return nil
}

func (s *Service) recordPublicationConflict(
	ctx context.Context, pending *store.CardDAVPublication,
	remote store.CardDAVRemoteResource, remoteTombstone, retainOversizeIntent bool,
) error {
	mapping, err := s.store.GetCardDAVResourceContext(ctx, pending.AddressBookID, pending.Href)
	if err != nil {
		return err
	}
	localBody := append([]byte(nil), pending.OutgoingBody...)
	localHash := pending.LocalHash
	localTombstone := pending.PendingOperation == store.CardDAVMutationDelete
	if !localTombstone && mapping.PersonID != nil {
		snapshot, err := s.store.LoadPersonVCardSnapshotContext(ctx, *mapping.PersonID)
		if err != nil {
			return err
		}
		if snapshot.Fingerprint != pending.LocalHash {
			books, err := s.store.ListCardDAVAddressBooksContext(ctx)
			if err != nil {
				return err
			}
			book, ok := findCardDAVBook(books, pending.AddressBookID)
			if !ok {
				return store.ErrCardDAVStalePlan
			}
			person, err := s.store.GetPersonContext(ctx, *mapping.PersonID)
			if err != nil {
				return err
			}
			localBody, localHash, err = s.renderPublicationCard(ctx, *person, book, mapping)
			if err != nil {
				return err
			}
		}
	}
	capture := store.CardDAVConflictCapture{
		AddressBookID: mapping.AddressBookID, Href: mapping.Href,
		ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash:           mapping.LocalHash, LocalHash: localHash,
		BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		LocalBody: localBody, LocalTombstone: localTombstone,
		RemoteETag: remote.RemoteETag, RemoteBody: remote.RemoteBody,
		RemoteTombstone: remoteTombstone,
	}
	var conflict *store.CardDAVConflict
	if retainOversizeIntent {
		conflict, err = s.store.RecordCardDAVPublicationConflictRetainingIntentContext(ctx, *pending, capture)
	} else {
		conflict, err = s.store.RecordCardDAVPublicationConflictContext(ctx, *pending, capture)
	}
	if err != nil {
		return err
	}
	return &ConflictError{ID: conflict.ID}
}

func (s *Service) prepareMappingConflict(
	ctx context.Context, book store.CardDAVAddressBook, mapping store.CardDAVResource,
	remote *store.CardDAVRemoteResource, remoteTombstone bool,
) (store.CardDAVConflictCapture, bool, error) {
	if mapping.MappingStatus != store.CardDAVMappingMapped {
		return store.CardDAVConflictCapture{}, false, nil
	}
	existingConflict, conflictErr := s.store.GetUnresolvedCardDAVConflictForMappingContext(ctx, book.ID, mapping.Href)
	unresolved := conflictErr == nil
	if conflictErr != nil && !errors.Is(conflictErr, store.ErrCardDAVConflictNotFound) {
		return store.CardDAVConflictCapture{}, false, conflictErr
	}
	localTombstone := mapping.PersonID == nil || (existingConflict != nil && existingConflict.LocalTombstone)
	localHash := mapping.LocalHash
	var localBody []byte
	if mapping.PersonID != nil && !localTombstone {
		person, err := s.store.GetPersonContext(ctx, *mapping.PersonID)
		if err != nil {
			return store.CardDAVConflictCapture{}, false, err
		}
		body, hash, err := s.renderPublicationCard(ctx, *person, book, &mapping)
		if err != nil {
			return store.CardDAVConflictCapture{}, false, err
		}
		localBody, localHash = body, hash
	}
	localChanged := localTombstone || localHash != mapping.LocalHash
	remoteChanged := remoteTombstone || remote.SemanticHash != mapping.RemoteSemanticHash
	if !unresolved && (!localChanged || !remoteChanged) {
		return store.CardDAVConflictCapture{}, false, nil
	}
	if !unresolved && localTombstone && remoteTombstone {
		return store.CardDAVConflictCapture{}, false, nil
	}
	if !unresolved && !localTombstone && !remoteTombstone {
		localSemanticHash, err := SemanticHash(localBody)
		if err != nil {
			return store.CardDAVConflictCapture{}, false, err
		}
		if localSemanticHash == remote.SemanticHash {
			remote.EquivalentLocalHash = localHash
			return store.CardDAVConflictCapture{}, false, nil
		}
	}
	capture := store.CardDAVConflictCapture{
		AddressBookID: book.ID, Href: mapping.Href,
		ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash:           mapping.LocalHash, LocalHash: localHash,
		BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		LocalBody: localBody, LocalTombstone: localTombstone,
		RemoteTombstone: remoteTombstone,
	}
	if remote != nil {
		capture.RemoteETag = remote.RemoteETag
		capture.RemoteBody = append([]byte(nil), remote.RemoteBody...)
	}
	size := 0
	if !capture.LocalTombstone {
		size += len(capture.LocalBody)
	}
	if !capture.RemoteTombstone {
		size += len(capture.RemoteBody)
	}
	if size > store.MaxCardDAVConflictSnapshotBytes {
		return store.CardDAVConflictCapture{}, false, store.ErrCardDAVConflictTooLarge
	}
	return capture, true, nil
}

func (s *Service) resolveConflictKeepLocal(
	ctx context.Context, conflict *store.CardDAVConflict,
) error {
	operationCtx, cancel := context.WithTimeout(ctx, s.client.operationTimeout)
	defer cancel()
	remote, tombstone, err := s.fetchCanonical(operationCtx, conflict.Href)
	if err != nil {
		return err
	}
	if conflict.LocalTombstone {
		if conflict.PendingOperation != "" {
			pending := &store.CardDAVPublication{
				AddressBookID: conflict.AddressBookID, Href: conflict.Href,
				PendingOperation: conflict.PendingOperation, RemoteETag: conflict.RemoteETag,
				ConnectionGeneration:    conflict.ConnectionGeneration,
				BookSyncRevision:        conflict.BookSyncRevision,
				MappingRevision:         conflict.MappingRevision,
				PreviousMappingRevision: conflict.PreviousMappingRevision,
				PendingStartedAt:        conflict.PendingStartedAt,
				RecoveryOnly:            true, ResolutionConflictID: conflict.ID,
			}
			if err := s.executeMutation(operationCtx, pending); err != nil {
				return err
			}
			_, err = s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now())
			return err
		}
		if !tombstone {
			pending, prepareErr := s.store.PrepareCardDAVConflictLocalTombstoneContext(
				operationCtx, conflict.ID, conflict.MappingRevision, remote)
			if prepareErr != nil {
				return prepareErr
			}
			if err := s.executeMutation(operationCtx, pending); err != nil {
				return err
			}
		} else {
			_, err = s.store.ResolveCardDAVConflictLocalTombstoneContext(
				operationCtx, conflict.ID, conflict.MappingRevision)
			if err != nil {
				return err
			}
		}
		_, err = s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now())
		return err
	}
	semanticHash, err := SemanticHash(conflict.LocalBody)
	if err != nil {
		return err
	}
	prepared, err := s.store.PrepareCardDAVConflictLocalContext(operationCtx,
		store.CardDAVConflictLocalPlan{
			ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision,
			RemoteETag: remote.RemoteETag, RemoteTombstone: tombstone,
			OutgoingSemanticHash: semanticHash,
		})
	if err != nil {
		return err
	}
	if err := s.executeMutation(operationCtx, prepared); err != nil {
		return err
	}
	_, err = s.store.SweepResolvedCardDAVConflictsContext(operationCtx, time.Now())
	return err
}
