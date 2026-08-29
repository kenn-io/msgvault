package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vcard"
)

type CardDAVMappingStatus string

const (
	CardDAVMappingMapped    CardDAVMappingStatus = "mapped"
	CardDAVMappingUnbound   CardDAVMappingStatus = "unbound"
	CardDAVMappingAmbiguous CardDAVMappingStatus = "ambiguous"
)

type CardDAVGovernance string

const (
	CardDAVGovernanceRemote CardDAVGovernance = "remote"
	CardDAVGovernanceLocal  CardDAVGovernance = "local"
	CardDAVGovernanceNone   CardDAVGovernance = "none"
)

var (
	ErrCardDAVResourceNotFound = errors.New("CardDAV resource not found")
	ErrCardDAVStalePlan        = errors.New("CardDAV sync plan is stale")
	ErrCardDAVInvalidPlan      = errors.New("invalid CardDAV sync plan")
)

type CardDAVResource struct {
	ID                   int64
	AddressBookID        int64
	Href                 string
	RemoteUID            string
	RemoteETag           string
	RemoteBody           []byte
	RemoteSemanticHash   string
	LocalHash            string
	MappingStatus        CardDAVMappingStatus
	MappingRevision      int64
	Governance           CardDAVGovernance
	PersonID             *int64
	PersonRevisionAtBind *int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// CardDAVRemoteResource is one complete network result. Contact values are
// already decoded from the same body and are used only for safe person
// binding; RemoteBody remains the authoritative lossless payload.
type CardDAVRemoteResource struct {
	Href                string
	PreviousHref        string
	RemoteUID           string
	RemoteETag          string
	RemoteBody          []byte
	SemanticHash        string
	DisplayName         string
	DisplayNameIdentity VCardIdentity
	Emails              []string
	EmailIdentities     []VCardIdentity
	Phones              []string
	PhoneIdentities     []VCardIdentity
	// EquivalentLocalHash is sync-plan evidence that the current local
	// projection and this remote body have the same CardDAV semantic hash.
	EquivalentLocalHash string
}

type CardDAVSyncPlan struct {
	AddressBookID          int64
	ConnectionGeneration   int64
	SyncRevision           int64
	ReplaceAll             bool
	NextSyncToken          string
	Upserts                []CardDAVRemoteResource
	RemovedHrefs           []string
	Conflicts              []CardDAVConflictCapture
	CompletesFullReconcile bool
}

type CardDAVApplyResult struct {
	Created int
	Updated int
	Removed int
}

// ApplyCardDAVSyncPlanContext applies a fully fetched network plan in one
// transaction. Both account and book fences are checked before ledger or
// person state is read, and the network client is intentionally absent from
// this API.
func (s *Store) ApplyCardDAVSyncPlanContext(
	ctx context.Context, plan CardDAVSyncPlan,
) (*CardDAVApplyResult, error) {
	if err := validateCardDAVSyncPlan(plan); err != nil {
		return nil, err
	}
	result := &CardDAVApplyResult{}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation
			FROM carddav_accounts WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVStalePlan
			}
			return fmt.Errorf("lock CardDAV account sync fence: %w", err)
		}
		if generation != plan.ConnectionGeneration {
			return ErrCardDAVStalePlan
		}
		var book CardDAVAddressBook
		if err := tx.QueryRowContext(ctx, `SELECT id, account_id, canonical_url,
			is_subscribed, is_lookup_source, sync_token, sync_revision
			FROM carddav_address_books WHERE id = ?`+s.dialect.SelectForUpdate(),
			plan.AddressBookID,
		).Scan(&book.ID, &book.AccountID, &book.CanonicalURL,
			&book.IsSubscribed, &book.IsLookupSource, &book.SyncToken, &book.SyncRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVStalePlan
			}
			return fmt.Errorf("lock CardDAV address book sync fence: %w", err)
		}
		if book.SyncRevision != plan.SyncRevision ||
			(!book.IsSubscribed && !book.IsLookupSource) {
			return ErrCardDAVStalePlan
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}

		seen := make(map[string]bool, len(plan.Upserts))
		conflicts := make(map[string]CardDAVConflictCapture, len(plan.Conflicts))
		for _, capture := range plan.Conflicts {
			conflicts[capture.Href] = capture
		}
		for _, input := range plan.Upserts {
			var moveHashes cardDAVHrefMoveHashes
			if input.PreviousHref != "" {
				var moveErr error
				moveHashes, moveErr = s.moveCardDAVResourceHrefTx(ctx, tx, book.ID, input)
				if moveErr != nil {
					return moveErr
				}
				if input.EquivalentLocalHash != "" && moveHashes.hasPerson {
					if input.EquivalentLocalHash != moveHashes.currentBefore {
						return ErrCardDAVStalePlan
					}
					input.EquivalentLocalHash = moveHashes.currentAfter
				}
			}
			seen[input.Href] = true
			capture, supplied := conflicts[input.Href]
			if supplied && input.PreviousHref != "" && moveHashes.hasPerson {
				if capture.BaseLocalHash != moveHashes.baseBefore {
					return ErrCardDAVStalePlan
				}
				capture.BaseLocalHash = moveHashes.baseAfter
				if capture.LocalTombstone {
					if capture.LocalHash != moveHashes.baseBefore {
						return ErrCardDAVStalePlan
					}
					capture.LocalHash = moveHashes.baseAfter
				} else {
					if capture.LocalHash != moveHashes.currentBefore {
						return ErrCardDAVStalePlan
					}
					capture.LocalHash = moveHashes.currentAfter
				}
			}
			needsConflict, err := s.cardDAVResourceNeedsConflictTx(ctx, tx, book.ID, input.Href,
				input.SemanticHash, input.EquivalentLocalHash)
			if err != nil {
				return err
			}
			if supplied != needsConflict {
				return ErrCardDAVStalePlan
			}
			if supplied {
				if capture.AddressBookID != book.ID || capture.RemoteTombstone ||
					capture.RemoteETag != input.RemoteETag || !bytes.Equal(capture.RemoteBody, input.RemoteBody) {
					return ErrCardDAVInvalidPlan
				}
				if _, err := s.recordCardDAVConflictTx(ctx, tx, capture, cardDAVConflictRecordOptions{
					supersedePendingIntent:         true,
					preserveExistingLocalTombstone: true,
				}); err != nil {
					return err
				}
				delete(conflicts, input.Href)
				continue
			}
			created, changed, err := s.applyCardDAVResourceTx(
				ctx, tx, book, input, plan.CompletesFullReconcile,
			)
			if err != nil {
				return err
			}
			if created {
				result.Created++
			} else if changed {
				result.Updated++
			}
		}

		removed := make(map[string]bool, len(plan.RemovedHrefs))
		for _, href := range plan.RemovedHrefs {
			removed[href] = true
		}
		if plan.ReplaceAll {
			rows, err := tx.QueryContext(ctx, `SELECT href FROM carddav_resources
				WHERE address_book_id = ? ORDER BY href`, book.ID)
			if err != nil {
				return fmt.Errorf("list CardDAV snapshot tombstones: %w", err)
			}
			for rows.Next() {
				var href string
				if err := rows.Scan(&href); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scan CardDAV snapshot tombstone: %w", err)
				}
				if !seen[href] {
					removed[href] = true
				}
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close CardDAV snapshot tombstones: %w", err)
			}
		}
		for href := range removed {
			capture, supplied := conflicts[href]
			needsConflict, err := s.cardDAVResourceNeedsConflictTx(ctx, tx, book.ID, href, "", "")
			if err != nil {
				return err
			}
			if supplied != needsConflict {
				return ErrCardDAVStalePlan
			}
			if supplied {
				if capture.AddressBookID != book.ID || !capture.RemoteTombstone {
					return ErrCardDAVInvalidPlan
				}
				completed, err := s.completePendingCardDAVConflictTombstoneFromPullTx(
					ctx, tx, book, generation, capture)
				if err != nil {
					return err
				}
				if completed {
					result.Removed++
					delete(conflicts, href)
					continue
				}
				if _, err := s.recordCardDAVConflictTx(ctx, tx, capture, cardDAVConflictRecordOptions{}); err != nil {
					return err
				}
				delete(conflicts, href)
				continue
			}
			wasRemoved, err := s.removeCardDAVResourceTx(ctx, tx, book.ID, href)
			if err != nil {
				return err
			}
			if wasRemoved {
				result.Removed++
			}
		}
		if len(conflicts) != 0 {
			return ErrCardDAVInvalidPlan
		}

		updated, err := tx.ExecContext(ctx, `UPDATE carddav_address_books SET
			sync_token = ?, sync_revision = sync_revision + 1,
			needs_full_reconcile = CASE WHEN ? THEN FALSE ELSE needs_full_reconcile END,
			updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND sync_revision = ?`,
			plan.NextSyncToken, plan.CompletesFullReconcile, book.ID, plan.SyncRevision)
		if err != nil {
			return fmt.Errorf("advance CardDAV address book sync fence: %w", err)
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("count CardDAV address book fence update: %w", err)
		}
		if affected != 1 {
			return ErrCardDAVStalePlan
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func validateCardDAVSyncPlan(plan CardDAVSyncPlan) error {
	if plan.AddressBookID <= 0 || plan.ConnectionGeneration <= 0 || plan.SyncRevision <= 0 {
		return ErrCardDAVInvalidPlan
	}
	if plan.CompletesFullReconcile && !plan.ReplaceAll {
		return ErrCardDAVInvalidPlan
	}
	seen := make(map[string]bool, len(plan.Upserts))
	for _, resource := range plan.Upserts {
		if strings.TrimSpace(resource.Href) == "" || strings.TrimSpace(resource.RemoteETag) == "" ||
			len(resource.RemoteBody) == 0 || strings.TrimSpace(resource.SemanticHash) == "" {
			return ErrCardDAVInvalidPlan
		}
		if seen[resource.Href] {
			return fmt.Errorf("%w: duplicate href", ErrCardDAVInvalidPlan)
		}
		if resource.PreviousHref != "" && (resource.PreviousHref == resource.Href ||
			strings.TrimSpace(resource.RemoteUID) == "") {
			return ErrCardDAVInvalidPlan
		}
		seen[resource.Href] = true
	}
	for _, href := range plan.RemovedHrefs {
		if strings.TrimSpace(href) == "" || seen[href] {
			return fmt.Errorf("%w: contradictory removal", ErrCardDAVInvalidPlan)
		}
	}
	conflicts := make(map[string]bool, len(plan.Conflicts))
	for _, capture := range plan.Conflicts {
		if err := validateCardDAVConflictCapture(capture); err != nil {
			return err
		}
		if capture.AddressBookID != plan.AddressBookID || conflicts[capture.Href] {
			return ErrCardDAVInvalidPlan
		}
		conflicts[capture.Href] = true
	}
	return nil
}

type cardDAVHrefMoveHashes struct {
	hasPerson     bool
	baseBefore    string
	baseAfter     string
	currentBefore string
	currentAfter  string
}

func (s *Store) moveCardDAVResourceHrefTx(
	ctx context.Context, tx *loggedTx, bookID int64, input CardDAVRemoteResource,
) (cardDAVHrefMoveHashes, error) {
	hashes := cardDAVHrefMoveHashes{}
	resource, err := s.findCardDAVResourceTx(ctx, tx, bookID, input.PreviousHref)
	if err != nil || resource.RemoteUID != input.RemoteUID {
		return hashes, ErrCardDAVStalePlan
	}
	if _, err := s.findCardDAVResourceTx(ctx, tx, bookID, input.Href); err == nil {
		return hashes, ErrCardDAVStalePlan
	} else if !errors.Is(err, ErrCardDAVResourceNotFound) {
		return hashes, err
	}
	if resource.PersonID != nil {
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *resource.PersonID)
		if err != nil {
			return hashes, err
		}
		hashes = cardDAVHrefMoveHashes{
			hasPerson: true, baseBefore: resource.LocalHash, baseAfter: resource.LocalHash,
			currentBefore: snapshot.Fingerprint, currentAfter: snapshot.Fingerprint,
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET href = ?, updated_at = `+
		s.dialect.Now()+` WHERE id = ? AND href = ?`, input.Href, resource.ID, input.PreviousHref)
	if err != nil {
		return hashes, fmt.Errorf("move CardDAV resource href: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return hashes, ErrCardDAVStalePlan
	}
	sourceRef := fmt.Sprintf("carddav:%d", bookID)
	if err := s.rewriteVCardSourceResourceProvenanceTx(
		ctx, tx, sourceRef, input.PreviousHref, input.Href,
	); err != nil {
		return hashes, err
	}
	if resource.PersonID != nil {
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *resource.PersonID)
		if err != nil {
			return hashes, err
		}
		hashes.currentAfter = snapshot.Fingerprint
		if hashes.currentBefore == hashes.baseBefore {
			hashes.baseAfter = snapshot.Fingerprint
			if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources
				SET local_hash = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
				snapshot.Fingerprint, resource.ID); err != nil {
				return hashes, fmt.Errorf("rebase moved CardDAV resource local hash: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE vcard_resource_envelopes SET
		source_resource_uid = ?, href = ?, revision = revision + 1,
		updated_at = `+s.dialect.Now()+`
		WHERE source_ref = ? AND source_resource_uid = ?`,
		input.Href, input.Href, sourceRef, input.PreviousHref); err != nil {
		return hashes, fmt.Errorf("move CardDAV resource envelope href: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET href = ?, updated_at = `+
		s.dialect.Now()+` WHERE address_book_id = ? AND href = ?`,
		input.Href, bookID, input.PreviousHref); err != nil {
		return hashes, fmt.Errorf("move CardDAV publication href: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE carddav_conflicts SET href = ?, updated_at = `+
		s.dialect.Now()+` WHERE address_book_id = ? AND href = ?`,
		input.Href, bookID, input.PreviousHref); err != nil {
		return hashes, fmt.Errorf("move CardDAV conflict href: %w", err)
	}
	return hashes, nil
}

func (s *Store) cardDAVResourceNeedsConflictTx(
	ctx context.Context, tx *loggedTx, bookID int64, href, remoteHash, equivalentLocalHash string,
) (bool, error) {
	resource, err := s.findCardDAVResourceTx(ctx, tx, bookID, href)
	if errors.Is(err, ErrCardDAVResourceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM carddav_conflicts
		WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`, bookID, href).Scan(&unresolved); err != nil {
		return false, fmt.Errorf("check unresolved CardDAV mapping conflict: %w", err)
	}
	if unresolved > 0 {
		return true, nil
	}
	if resource.MappingStatus != CardDAVMappingMapped {
		return false, nil
	}
	localChanged := resource.PersonID == nil
	localHash := ""
	if resource.PersonID != nil {
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *resource.PersonID)
		if err != nil {
			return false, err
		}
		localHash = snapshot.Fingerprint
		localChanged = localHash != resource.LocalHash
	}
	remoteChanged := remoteHash == "" || remoteHash != resource.RemoteSemanticHash
	if localChanged && remoteChanged {
		if resource.PersonID == nil && remoteHash == "" {
			return false, nil
		}
		if equivalentLocalHash != "" && localHash == equivalentLocalHash {
			return false, nil
		}
	}
	return localChanged && remoteChanged, nil
}

func (s *Store) applyCardDAVResourceTx(
	ctx context.Context, tx *loggedTx, book CardDAVAddressBook,
	input CardDAVRemoteResource, reconcileBinding bool,
) (bool, bool, error) {
	current, err := s.findCardDAVResourceTx(ctx, tx, book.ID, input.Href)
	created := errors.Is(err, ErrCardDAVResourceNotFound)
	if err != nil && !created {
		return false, false, err
	}
	unchangedRemote := !created && current.RemoteETag == input.RemoteETag &&
		bytes.Equal(current.RemoteBody, input.RemoteBody)
	bindingNeedsReconcile := !created && (current.MappingStatus == CardDAVMappingUnbound ||
		current.MappingStatus == CardDAVMappingAmbiguous)
	if unchangedRemote && (!reconcileBinding || !bindingNeedsReconcile) {
		return false, false, nil
	}

	var personID *int64
	personRevision := (*int64)(nil)
	status := CardDAVMappingUnbound
	governance := CardDAVGovernanceNone
	var ambiguous []cardDAVPersonMatch
	projectionRebased := false
	remoteOwnsDisplay := false
	preserveLocalTombstone := !created && current.MappingStatus == CardDAVMappingMapped &&
		current.PersonID == nil && current.RemoteSemanticHash == input.SemanticHash
	if preserveLocalTombstone {
		status, governance = current.MappingStatus, current.Governance
	} else if !created && current.PersonID != nil {
		personID = current.PersonID
		personRevision = current.PersonRevisionAtBind
		status, governance = current.MappingStatus, current.Governance
	} else {
		resourceID := int64(0)
		if !created {
			resourceID = current.ID
		}
		personID, ambiguous, err = s.resolveCardDAVPersonTx(ctx, tx, resourceID, input)
		if err != nil {
			return false, false, err
		}
		switch {
		case personID != nil:
			status, governance = CardDAVMappingMapped, CardDAVGovernanceLocal
		case len(ambiguous) > 0:
			status, governance = CardDAVMappingAmbiguous, CardDAVGovernanceNone
		case book.IsSubscribed:
			personID, personRevision, err = s.createCardDAVImportedPersonTx(ctx, tx, book.ID, input)
			if err != nil {
				return false, false, err
			}
			status, governance = CardDAVMappingMapped, CardDAVGovernanceRemote
		}
	}
	semanticRemoteChanged := !created && current.RemoteSemanticHash != input.SemanticHash
	if semanticRemoteChanged && current.Governance == CardDAVGovernanceRemote &&
		current.MappingStatus == CardDAVMappingMapped && personID != nil &&
		current.PersonRevisionAtBind != nil {
		hasUserOwnedState, err := s.personHasUserOwnedStateTx(
			ctx, tx, *personID, *current.PersonRevisionAtBind,
		)
		if err != nil {
			return false, false, err
		}
		if !hasUserOwnedState {
			remoteOwnsDisplay, err = s.rebaseCardDAVImportedProjectionTx(
				ctx, tx, book.ID, *personID, input,
			)
			if err != nil {
				return false, false, err
			}
			projectionRebased = true
			personRevision = nil
		}
	}
	if personID != nil && personRevision == nil {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`, *personID).Scan(&revision); err != nil {
			return false, false, fmt.Errorf("load CardDAV-bound person revision: %w", err)
		}
		personRevision = &revision
	}
	localHash := input.SemanticHash
	if !created {
		localHash = current.LocalHash
	}
	if personID != nil {
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *personID)
		if err != nil {
			return false, false, fmt.Errorf("hash CardDAV-bound local person: %w", err)
		}
		localHash = snapshot.Fingerprint
	}

	var resourceID int64
	if created {
		err = tx.QueryRowContext(ctx, `INSERT INTO carddav_resources (
			address_book_id, href, remote_uid, remote_etag, remote_body,
			remote_semantic_hash, local_hash, mapping_status, mapping_revision,
			governance, person_id, person_revision_at_bind
		) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, 1, ?, ?, ?) RETURNING id`,
			book.ID, input.Href, input.RemoteUID, input.RemoteETag, input.RemoteBody,
			input.SemanticHash, localHash, status, governance,
			nullableVCardInt64(personID), nullableVCardInt64(personRevision),
		).Scan(&resourceID)
	} else {
		resourceID = current.ID
		_, err = tx.ExecContext(ctx, `UPDATE carddav_resources SET
			remote_uid = NULLIF(?, ''), remote_etag = ?, remote_body = ?,
			remote_semantic_hash = ?, local_hash = ?, mapping_status = ?,
			mapping_revision = mapping_revision + 1, governance = ?, person_id = ?,
			person_revision_at_bind = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ?`, input.RemoteUID, input.RemoteETag, input.RemoteBody,
			input.SemanticHash, localHash, status, governance,
			nullableVCardInt64(personID), nullableVCardInt64(personRevision), resourceID)
	}
	if err != nil {
		return false, false, fmt.Errorf("save CardDAV resource ledger: %w", err)
	}
	sourceRef := fmt.Sprintf("carddav:%d", book.ID)
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates
		WHERE source = ? AND source_ref = ?
		  AND state IN (?, ?)
		  AND decided_at IS NULL
		  AND ((left_kind = ? AND left_id = ?)
		    OR (right_kind = ? AND right_id = ?))`,
		ProvenanceCardDAVImport, sourceRef,
		IdentityMatchStateCandidate, IdentityMatchStateConflict,
		IdentityMatchCardDAVResource, resourceID,
		IdentityMatchCardDAVResource, resourceID,
	); err != nil {
		return false, false, fmt.Errorf("reconcile CardDAV identity candidates: %w", err)
	}
	if personID != nil {
		if err := s.putCardDAVEnvelopeTx(ctx, tx, book.ID, *personID, input); err != nil {
			return false, false, err
		}
	}
	if projectionRebased {
		if err := s.refreshCardDAVImportedPersonBindBaselineTx(
			ctx, tx, resourceID, *personID, remoteOwnsDisplay,
		); err != nil {
			return false, false, err
		}
	}
	for _, match := range ambiguous {
		normalized := match.NormalizedValue
		_, _, err := s.upsertIdentityMatchCandidateTx(ctx, tx, IdentityMatchCandidateInput{
			LeftKind: IdentityMatchCardDAVResource, LeftID: resourceID,
			RightKind: IdentityMatchPerson, RightID: match.PersonID,
			Basis: match.Basis, NormalizedValue: &normalized,
			State: IdentityMatchStateConflict, Source: ProvenanceCardDAVImport,
			SourceRef: &sourceRef,
		}, IdentityMatchCardDAVResource, resourceID, IdentityMatchPerson,
			match.PersonID, nil, false)
		if err != nil {
			return false, false, err
		}
	}
	return created, true, nil
}

// acceptCardDAVIdentityMatchCandidateContext records the explicit review
// decision and binds the resource to the chosen person in one transaction.
// Competing generated candidates are rejected as part of the same decision;
// subsequent reconciliation preserves every accepted or rejected row.
func (s *Store) acceptCardDAVIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64, decidedBy string, notes *string,
) (*IdentityMatchCandidate, int64, error) {
	if decidedBy != string(ProvenanceUser) {
		return nil, 0, ErrIdentityMatchNotAcceptable
	}
	var accepted *IdentityMatchCandidate
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		candidate, err := getIdentityMatchCandidateTx(ctx, tx, candidateID)
		if err != nil {
			return err
		}
		if candidate.LeftKind != IdentityMatchCardDAVResource ||
			candidate.RightKind != IdentityMatchPerson {
			return ErrIdentityMatchEndpointUnsupported
		}
		resource, err := scanCardDAVResource(tx.QueryRowContext(ctx,
			cardDAVResourceSelect+` WHERE id = ?`+s.dialect.SelectForUpdate(),
			candidate.LeftID,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrIdentityMatchEndpointNotFound
		}
		if err != nil {
			return fmt.Errorf("lock CardDAV candidate resource: %w", err)
		}
		if resource.PersonID != nil && *resource.PersonID != candidate.RightID {
			return ErrIdentityMatchAlreadyApplied
		}
		var personRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`+
			s.dialect.SelectForUpdate(), candidate.RightID).Scan(&personRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIdentityMatchEndpointNotFound
			}
			return fmt.Errorf("lock CardDAV candidate person: %w", err)
		}
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, candidate.RightID)
		if err != nil {
			return fmt.Errorf("hash accepted CardDAV candidate person: %w", err)
		}
		if resource.PersonID == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
				mapping_status = ?, mapping_revision = mapping_revision + 1,
				governance = ?, person_id = ?, person_revision_at_bind = ?,
				local_hash = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`,
				CardDAVMappingMapped, CardDAVGovernanceLocal, candidate.RightID,
				personRevision, snapshot.Fingerprint, resource.ID,
			); err != nil {
				return fmt.Errorf("bind accepted CardDAV identity candidate: %w", err)
			}
			if err := s.putCardDAVEnvelopeTx(ctx, tx, resource.AddressBookID,
				candidate.RightID, CardDAVRemoteResource{
					Href: resource.Href, RemoteBody: resource.RemoteBody,
				}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
			state = ?, decided_by = ?, decided_at = `+s.dialect.Now()+`, notes = ?,
			pre_conflict_state = NULL, application_pending = FALSE,
			updated_at = `+s.dialect.Now()+` WHERE id = ?`,
			IdentityMatchStateAccepted, decidedBy, stringValue(notes), candidate.ID,
		); err != nil {
			return fmt.Errorf("accept CardDAV identity candidate: %w", err)
		}
		peerNote := "another CardDAV identity candidate was accepted"
		if _, err := tx.ExecContext(ctx, `UPDATE identity_match_candidates SET
			state = ?, decided_by = ?, decided_at = `+s.dialect.Now()+`, notes = ?,
			pre_conflict_state = NULL, application_pending = FALSE,
			updated_at = `+s.dialect.Now()+`
			WHERE id <> ? AND source = ? AND state IN (?, ?)
			  AND left_kind = ? AND left_id = ? AND right_kind = ?`,
			IdentityMatchStateRejected, decidedBy, peerNote, candidate.ID,
			ProvenanceCardDAVImport, IdentityMatchStateCandidate,
			IdentityMatchStateConflict, IdentityMatchCardDAVResource,
			resource.ID, IdentityMatchPerson,
		); err != nil {
			return fmt.Errorf("reject competing CardDAV identity candidates: %w", err)
		}
		accepted, err = getIdentityMatchCandidateTx(ctx, tx, candidate.ID)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	revision, err := readIdentityRevisionContext(ctx, s.db)
	if err != nil {
		return nil, 0, err
	}
	return accepted, revision, nil
}

type cardDAVPersonMatch struct {
	PersonID        int64
	Basis           IdentityMatchBasis
	NormalizedValue string
}

func (s *Store) resolveCardDAVPersonTx(
	ctx context.Context, tx *loggedTx, resourceID int64, input CardDAVRemoteResource,
) (*int64, []cardDAVPersonMatch, error) {
	rejected := map[int64]struct{}{}
	if resourceID != 0 {
		rows, err := tx.QueryContext(ctx, `SELECT right_id, state FROM identity_match_candidates
			WHERE left_kind = ? AND left_id = ? AND right_kind = ?
			  AND state IN (?, ?) ORDER BY right_id`,
			IdentityMatchCardDAVResource, resourceID, IdentityMatchPerson,
			IdentityMatchStateAccepted, IdentityMatchStateRejected)
		if err != nil {
			return nil, nil, fmt.Errorf("load reviewed CardDAV identity candidates: %w", err)
		}
		var accepted []int64
		for rows.Next() {
			var personID int64
			var state IdentityMatchState
			if err := rows.Scan(&personID, &state); err != nil {
				_ = rows.Close()
				return nil, nil, fmt.Errorf("scan reviewed CardDAV identity candidate: %w", err)
			}
			if state == IdentityMatchStateAccepted {
				accepted = append(accepted, personID)
			} else {
				rejected[personID] = struct{}{}
			}
		}
		if err := rows.Close(); err != nil {
			return nil, nil, fmt.Errorf("close reviewed CardDAV identity candidates: %w", err)
		}
		if len(accepted) > 1 {
			return nil, nil, errors.New("CardDAV resource has multiple accepted identity candidates")
		}
		if len(accepted) == 1 {
			return &accepted[0], nil, nil
		}
	}
	if uid := strings.TrimSpace(input.RemoteUID); uid != "" {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid = ?`, uid).Scan(&id)
		if err == nil {
			if _, wasRejected := rejected[id]; !wasRejected {
				return &id, nil, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("match CardDAV canonical UID: %w", err)
		}
		var surviving sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT surviving_person_id
			FROM person_uid_aliases WHERE retired_uid = ?`, uid).Scan(&surviving)
		if err == nil && surviving.Valid {
			id = surviving.Int64
			if _, wasRejected := rejected[id]; !wasRejected {
				return &id, nil, nil
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("match CardDAV retired UID: %w", err)
		}
	}

	matches := map[int64]cardDAVPersonMatch{}
	for _, candidate := range []struct {
		kind   ContactAddressKind
		basis  IdentityMatchBasis
		values []string
	}{{ContactAddressEmail, IdentityMatchEmail, input.Emails},
		{ContactAddressPhone, IdentityMatchPhone, input.Phones}} {
		for _, raw := range candidate.values {
			normalized, err := NormalizeServiceValue(nil, candidate.kind, raw)
			if err != nil {
				continue
			}
			rows, err := tx.QueryContext(ctx, `SELECT person_id FROM person_contact_points
				WHERE address_kind = ? AND service_id IS NULL
				  AND normalized_value = ? AND active_until IS NULL AND superseded_at IS NULL
				ORDER BY person_id`, candidate.kind, normalized)
			if err != nil {
				return nil, nil, fmt.Errorf("match CardDAV contact point: %w", err)
			}
			for rows.Next() {
				var personID int64
				if err := rows.Scan(&personID); err != nil {
					_ = rows.Close()
					return nil, nil, fmt.Errorf("scan CardDAV contact match: %w", err)
				}
				if _, wasRejected := rejected[personID]; !wasRejected {
					matches[personID] = cardDAVPersonMatch{PersonID: personID, Basis: candidate.basis, NormalizedValue: normalized}
				}
			}
			if err := rows.Close(); err != nil {
				return nil, nil, fmt.Errorf("close CardDAV contact matches: %w", err)
			}
		}
	}
	if len(matches) == 1 {
		for id := range matches {
			return &id, nil, nil
		}
	}
	ambiguous := make([]cardDAVPersonMatch, 0, len(matches))
	for _, match := range matches {
		ambiguous = append(ambiguous, match)
	}
	slices.SortFunc(ambiguous, func(left, right cardDAVPersonMatch) int {
		return int(left.PersonID - right.PersonID)
	})
	return nil, ambiguous, nil
}

func (s *Store) createCardDAVImportedPersonTx(
	ctx context.Context, tx *loggedTx, bookID int64, input CardDAVRemoteResource,
) (*int64, *int64, error) {
	uid, err := newVCardUID()
	if err != nil {
		return nil, nil, err
	}
	var personID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, NULLIF(?, '')) RETURNING id`, uid, input.DisplayName).Scan(&personID); err != nil {
		return nil, nil, fmt.Errorf("create CardDAV imported person: %w", err)
	}
	if err := s.addCardDAVImportedProjectionTx(ctx, tx, bookID, personID, input); err != nil {
		return nil, nil, err
	}
	revision := int64(1)
	return &personID, &revision, nil
}

func (s *Store) addCardDAVImportedProjectionTx(
	ctx context.Context, tx *loggedTx, bookID, personID int64, input CardDAVRemoteResource,
) error {
	sourceRef := fmt.Sprintf("carddav:%d", bookID)
	baseEnvelope := ValueEnvelopeInput{
		Source: ProvenanceCardDAVImport, SourceRef: &sourceRef,
		SourceResourceUID: &input.Href,
	}
	if strings.TrimSpace(input.DisplayName) != "" {
		envelope := baseEnvelope
		envelope.VCard = input.DisplayNameIdentity
		if _, err := s.addPersonNameTx(ctx, tx, personID, PersonNameInput{
			NameKind: PersonNameFormatted, Formatted: &input.DisplayName,
			OriginalValue: input.DisplayName, Envelope: envelope,
		}); err != nil {
			return err
		}
	}
	for _, point := range []struct {
		kind       ContactAddressKind
		values     []string
		identities []VCardIdentity
	}{
		{ContactAddressEmail, input.Emails, input.EmailIdentities},
		{ContactAddressPhone, input.Phones, input.PhoneIdentities},
	} {
		for index, value := range point.values {
			envelope := baseEnvelope
			if index < len(point.identities) {
				envelope.VCard = point.identities[index]
			}
			if _, err := s.addPersonContactPointTx(ctx, tx, personID, PersonContactPointInput{
				AddressKind: point.kind, OriginalValue: value, Envelope: envelope,
			}); err != nil {
				if point.kind == ContactAddressPhone && errors.Is(err, ErrNormalizationRejected) {
					continue
				}
				return err
			}
		}
	}
	return nil
}

// rebaseCardDAVImportedProjectionTx replaces only the fields owned by this
// CardDAV resource. Explicit user rows survive untouched. The scalar display
// label is replaced only while it still equals the prior imported FN; a local
// rename therefore remains user-owned even as the imported formatted name
// advances underneath it.
func (s *Store) rebaseCardDAVImportedProjectionTx(
	ctx context.Context, tx *loggedTx, bookID, personID int64, input CardDAVRemoteResource,
) (bool, error) {
	sourceRef := fmt.Sprintf("carddav:%d", bookID)
	var currentDisplay, priorImportedDisplay sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE id = ?`+
		s.dialect.SelectForUpdate(), personID).Scan(&currentDisplay); err != nil {
		return false, fmt.Errorf("lock CardDAV projection person: %w", err)
	}
	err := tx.QueryRowContext(ctx, `SELECT formatted FROM person_names
		WHERE person_id = ? AND source = ? AND source_ref = ? AND source_resource_uid = ?
		  AND name_kind = ? AND active_until IS NULL AND superseded_at IS NULL
		ORDER BY id LIMIT 1`, personID, ProvenanceCardDAVImport, sourceRef, input.Href,
		PersonNameFormatted).Scan(&priorImportedDisplay)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("load prior CardDAV display projection: %w", err)
	}
	remoteOwnsDisplay := !currentDisplay.Valid ||
		(priorImportedDisplay.Valid && currentDisplay.String == priorImportedDisplay.String)

	for _, table := range []string{"person_names", "person_contact_points"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET
			superseded_at = `+s.dialect.Now()+`, updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND source = ? AND source_ref = ? AND source_resource_uid = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			personID, ProvenanceCardDAVImport, sourceRef, input.Href); err != nil {
			return false, fmt.Errorf("supersede %s CardDAV projection: %w", table, err)
		}
	}
	if err := s.addCardDAVImportedProjectionTx(ctx, tx, bookID, personID, input); err != nil {
		return false, err
	}
	displayChanged := false
	if remoteOwnsDisplay {
		result, err := tx.ExecContext(ctx, `UPDATE persons SET display_name = NULLIF(?, '')
			WHERE id = ? AND display_name IS DISTINCT FROM NULLIF(?, '')`,
			strings.TrimSpace(input.DisplayName), personID, strings.TrimSpace(input.DisplayName))
		if err != nil {
			return false, fmt.Errorf("rebase CardDAV display label: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("count rebased CardDAV display label: %w", err)
		}
		displayChanged = affected > 0
	}
	if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
		return false, err
	}
	if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, personID); err != nil {
		return false, err
	}
	if displayChanged {
		if err := s.bumpDisplayNameCounterpartVCardProjectionsTx(ctx, tx, personID); err != nil {
			return false, err
		}
	}
	return remoteOwnsDisplay, nil
}

// retireCardDAVImportedProjectionTx removes one resource's current semantic
// projection while retaining its history. The scalar display label is cleared
// only when it still matches the imported formatted name being retired.
func (s *Store) retireCardDAVImportedProjectionTx(
	ctx context.Context, tx *loggedTx, bookID, personID int64, href string,
) error {
	sourceRef := fmt.Sprintf("carddav:%d", bookID)
	var currentDisplay, importedDisplay sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE id = ?`+
		s.dialect.SelectForUpdate(), personID).Scan(&currentDisplay); err != nil {
		return fmt.Errorf("lock retired CardDAV projection person: %w", err)
	}
	err := tx.QueryRowContext(ctx, `SELECT formatted FROM person_names
		WHERE person_id = ? AND source = ? AND source_ref = ? AND source_resource_uid = ?
		  AND name_kind = ? AND active_until IS NULL AND superseded_at IS NULL
		ORDER BY id LIMIT 1`, personID, ProvenanceCardDAVImport, sourceRef, href,
		PersonNameFormatted).Scan(&importedDisplay)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load retired CardDAV display projection: %w", err)
	}
	remoteOwnsDisplay := currentDisplay.Valid && importedDisplay.Valid &&
		currentDisplay.String == importedDisplay.String

	for _, table := range []string{"person_names", "person_contact_points"} {
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET
			superseded_at = `+s.dialect.Now()+`, updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND source = ? AND source_ref = ? AND source_resource_uid = ?
			  AND active_until IS NULL AND superseded_at IS NULL`,
			personID, ProvenanceCardDAVImport, sourceRef, href); err != nil {
			return fmt.Errorf("retire %s CardDAV projection: %w", table, err)
		}
	}
	if remoteOwnsDisplay {
		if _, err := tx.ExecContext(ctx, `UPDATE persons SET display_name = NULL
			WHERE id = ? AND display_name = ?`, personID, importedDisplay.String); err != nil {
			return fmt.Errorf("clear retired CardDAV display label: %w", err)
		}
	}
	if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
		return err
	}
	if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, personID); err != nil {
		return err
	}
	if remoteOwnsDisplay {
		if err := s.bumpDisplayNameCounterpartVCardProjectionsTx(ctx, tx, personID); err != nil {
			return err
		}
	}
	return nil
}

// refreshCardDAVImportedPersonBindBaselineTx records the revision produced by
// a remote-owned rebase only when the rebase left no user-owned state behind.
// A locally changed display label has no separate provenance row, so the
// caller supplies whether the imported display still owned that scalar.
func (s *Store) refreshCardDAVImportedPersonBindBaselineTx(
	ctx context.Context, tx *loggedTx, resourceID, personID int64, remoteOwnsDisplay bool,
) error {
	if !remoteOwnsDisplay {
		return nil
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`+
		s.dialect.SelectForUpdate(), personID).Scan(&revision); err != nil {
		return fmt.Errorf("load rebased CardDAV person revision: %w", err)
	}
	hasUserOwnedState, err := s.personHasUserOwnedStateTx(ctx, tx, personID, revision)
	if err != nil {
		return err
	}
	if hasUserOwnedState {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE carddav_resources
		SET person_revision_at_bind = ?
		WHERE id = ? AND person_id = ? AND governance = ?`,
		revision, resourceID, personID, CardDAVGovernanceRemote)
	if err != nil {
		return fmt.Errorf("refresh rebased CardDAV person revision: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count refreshed CardDAV person revision: %w", err)
	}
	if affected != 1 {
		return ErrCardDAVConflictStale
	}
	return nil
}

func (s *Store) putCardDAVEnvelopeTx(
	ctx context.Context, tx *loggedTx, bookID, personID int64,
	input CardDAVRemoteResource,
) error {
	envelope, err := vcard.ParseResourceEnvelope(input.RemoteBody)
	if err != nil {
		return fmt.Errorf("parse CardDAV resource envelope: %w", err)
	}
	envelope.SourceRef = fmt.Sprintf("carddav:%d", bookID)
	envelope.SourceResourceUID = input.Href
	envelope.Href = input.Href
	canonicalUID, err := vcardCanonicalUIDTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	envelope.CanonicalPersonUID = canonicalUID
	prepared, err := prepareVCardEnvelope(envelope)
	if err != nil {
		return err
	}
	current, err := s.findVCardResourceEnvelopeTx(ctx, tx, envelope.SourceRef, input.Href)
	if errors.Is(err, ErrVCardResourceNotFound) {
		_, err = s.insertVCardResourceEnvelopeTx(ctx, tx,
			VCardResourceEnvelopeInput{PersonID: personID}, prepared)
		return err
	}
	if err != nil {
		return err
	}
	expected := current.Revision
	_, err = s.updateVCardResourceEnvelopeTx(ctx, tx, VCardResourceEnvelopeInput{
		PersonID: personID, ExpectedRevision: &expected,
	}, prepared, current)
	return err
}

func (s *Store) removeCardDAVResourceTx(
	ctx context.Context, tx *loggedTx, bookID int64, href string,
) (bool, error) {
	return s.removeCardDAVResourceWithPersonRetentionTx(ctx, tx, bookID, href, false)
}

func (s *Store) removeCardDAVResourceWithPersonRetentionTx(
	ctx context.Context, tx *loggedTx, bookID int64, href string, retainPerson bool,
) (bool, error) {
	return s.removeCardDAVResourceWithModeTx(
		ctx, tx, bookID, href, retainPerson, cardDAVRemovalPreserveProjection,
	)
}

type cardDAVResourceRemovalMode uint8

const (
	cardDAVRemovalPreserveProjection cardDAVResourceRemovalMode = iota
	cardDAVRemovalRetireProjection
)

func (s *Store) removeCardDAVResourceWithModeTx(
	ctx context.Context, tx *loggedTx, bookID int64, href string, retainPerson bool,
	mode cardDAVResourceRemovalMode,
) (bool, error) {
	resource, err := s.findCardDAVResourceTx(ctx, tx, bookID, href)
	if errors.Is(err, ErrCardDAVResourceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cleanupImportedPerson := false
	personHasUserOwnedState := false
	if !retainPerson && resource.PersonID != nil && resource.PersonRevisionAtBind != nil {
		cleanupImportedPerson, err = s.personHasCardDAVImportedProjectionTx(
			ctx, tx, *resource.PersonID,
		)
		if err != nil {
			return false, err
		}
	}
	if cleanupImportedPerson {
		personHasUserOwnedState, err = s.personHasUserOwnedStateTx(
			ctx, tx, *resource.PersonID, *resource.PersonRevisionAtBind,
		)
		if err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates
		WHERE left_kind = ? AND left_id = ?`, IdentityMatchCardDAVResource, resource.ID); err != nil {
		return false, fmt.Errorf("delete CardDAV identity candidates: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vcard_resource_envelopes
		WHERE source_ref = ? AND source_resource_uid = ?`, fmt.Sprintf("carddav:%d", bookID), href); err != nil {
		return false, fmt.Errorf("delete CardDAV resource envelope: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_resources WHERE id = ?`, resource.ID); err != nil {
		return false, fmt.Errorf("delete CardDAV resource ledger row: %w", err)
	}
	if err := s.transferCardDAVImportedPersonCleanupBaselineTx(
		ctx, tx, resource, personHasUserOwnedState,
	); err != nil {
		return false, err
	}
	if mode == cardDAVRemovalRetireProjection && personHasUserOwnedState {
		if err := s.retireCardDAVImportedProjectionTx(
			ctx, tx, bookID, *resource.PersonID, href,
		); err != nil {
			return false, err
		}
	}
	if !retainPerson {
		if err := s.deleteUntouchedCardDAVImportedPersonTx(
			ctx, tx, resource, cleanupImportedPerson, personHasUserOwnedState,
		); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *Store) demoteCardDAVBookResourcesTx(ctx context.Context, tx *loggedTx, bookID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT href FROM carddav_resources
		WHERE address_book_id = ? ORDER BY href`, bookID)
	if err != nil {
		return fmt.Errorf("list CardDAV resources for demotion: %w", err)
	}
	var hrefs []string
	for rows.Next() {
		var href string
		if err := rows.Scan(&href); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan CardDAV resource for demotion: %w", err)
		}
		hrefs = append(hrefs, href)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate CardDAV resources for demotion: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close CardDAV resources for demotion: %w", err)
	}
	for _, href := range hrefs {
		resource, err := s.findCardDAVResourceTx(ctx, tx, bookID, href)
		if err != nil {
			return err
		}
		cleanupImportedPerson := false
		personHasUserOwnedState := false
		if resource.PersonID != nil && resource.PersonRevisionAtBind != nil {
			cleanupImportedPerson, err = s.personHasCardDAVImportedProjectionTx(
				ctx, tx, *resource.PersonID,
			)
			if err != nil {
				return err
			}
		}
		if cleanupImportedPerson {
			personHasUserOwnedState, err = s.personHasUserOwnedStateTx(
				ctx, tx, *resource.PersonID, *resource.PersonRevisionAtBind,
			)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates
			WHERE left_kind = ? AND left_id = ? AND source = ?
			  AND state IN (?, ?) AND decided_at IS NULL`,
			IdentityMatchCardDAVResource, resource.ID, ProvenanceCardDAVImport,
			IdentityMatchStateCandidate, IdentityMatchStateConflict); err != nil {
			return fmt.Errorf("delete demoted CardDAV identity candidates: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM vcard_resource_envelopes
			WHERE source_ref = ? AND source_resource_uid = ?`, fmt.Sprintf("carddav:%d", bookID), href); err != nil {
			return fmt.Errorf("delete demoted CardDAV resource envelope: %w", err)
		}
		demotedStatus := CardDAVMappingUnbound
		if resource.MappingStatus == CardDAVMappingMapped && resource.PersonID == nil {
			demotedStatus = CardDAVMappingMapped
		}
		if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_status = ?, mapping_revision = mapping_revision + 1,
			governance = ?, person_id = NULL, person_revision_at_bind = NULL,
			updated_at = `+s.dialect.Now()+` WHERE id = ?`,
			demotedStatus, CardDAVGovernanceNone, resource.ID); err != nil {
			return fmt.Errorf("demote CardDAV resource: %w", err)
		}
		if err := s.transferCardDAVImportedPersonCleanupBaselineTx(
			ctx, tx, resource, personHasUserOwnedState,
		); err != nil {
			return err
		}
		if err := s.deleteUntouchedCardDAVImportedPersonTx(
			ctx, tx, resource, cleanupImportedPerson, personHasUserOwnedState,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) dropCardDAVBookResourcesTx(ctx context.Context, tx *loggedTx, bookID int64) error {
	return s.dropCardDAVBookResourcesWithModeTx(
		ctx, tx, bookID, cardDAVRemovalPreserveProjection,
	)
}

func (s *Store) dropCardDAVBookResourcesForIdentityChangeTx(
	ctx context.Context, tx *loggedTx, bookID int64,
) error {
	return s.dropCardDAVBookResourcesWithModeTx(
		ctx, tx, bookID, cardDAVRemovalRetireProjection,
	)
}

func (s *Store) dropCardDAVBookResourcesWithModeTx(
	ctx context.Context, tx *loggedTx, bookID int64, mode cardDAVResourceRemovalMode,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT href FROM carddav_resources
		WHERE address_book_id = ? ORDER BY href`, bookID)
	if err != nil {
		return fmt.Errorf("list ignored CardDAV resources: %w", err)
	}
	var hrefs []string
	for rows.Next() {
		var href string
		if err := rows.Scan(&href); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan ignored CardDAV resource: %w", err)
		}
		hrefs = append(hrefs, href)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate ignored CardDAV resources: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close ignored CardDAV resources: %w", err)
	}
	for _, href := range hrefs {
		if _, err := s.removeCardDAVResourceWithModeTx(
			ctx, tx, bookID, href, false, mode,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) deleteUntouchedCardDAVImportedPersonTx(
	ctx context.Context, tx *loggedTx, resource *CardDAVResource,
	cleanupImportedPerson, personHasUserOwnedState bool,
) error {
	if resource.PersonID == nil || resource.PersonRevisionAtBind == nil ||
		!cleanupImportedPerson || personHasUserOwnedState {
		return nil
	}
	var revision, otherMappings int64
	err := tx.QueryRowContext(ctx, `SELECT revision,
		(SELECT COUNT(*) FROM carddav_resources WHERE person_id = persons.id)
		FROM persons WHERE id = ?`, *resource.PersonID).Scan(&revision, &otherMappings)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check CardDAV imported person tombstone: %w", err)
	}
	if revision != *resource.PersonRevisionAtBind || otherMappings != 0 {
		return nil
	}
	if err := s.deleteIdentityMatchCandidatesForPersonTx(ctx, tx, *resource.PersonID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM persons WHERE id = ? AND revision = ?`,
		*resource.PersonID, revision); err != nil {
		return fmt.Errorf("delete untouched CardDAV imported person: %w", err)
	}
	return nil
}

func (s *Store) personHasCardDAVImportedProjectionTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (bool, error) {
	var imported bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_names WHERE person_id = ? AND source = ?
		UNION ALL
		SELECT 1 FROM person_contact_points WHERE person_id = ? AND source = ?
	)`, personID, ProvenanceCardDAVImport, personID, ProvenanceCardDAVImport).Scan(&imported)
	if err != nil {
		return false, fmt.Errorf("check CardDAV imported person projection: %w", err)
	}
	return imported, nil
}

// When a remotely governed resource disappears before another mapping for the
// same imported person, carry its cleanup baseline forward. A nil baseline is
// deliberately sticky once user-owned state is observed, so removing the last
// duplicate cannot later erase that state using a newer local bind revision.
func (s *Store) transferCardDAVImportedPersonCleanupBaselineTx(
	ctx context.Context, tx *loggedTx, resource *CardDAVResource, personHasUserOwnedState bool,
) error {
	if resource.PersonID == nil || resource.Governance != CardDAVGovernanceRemote ||
		resource.PersonRevisionAtBind == nil {
		return nil
	}
	var baseline any = *resource.PersonRevisionAtBind
	if personHasUserOwnedState {
		baseline = nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources
		SET person_revision_at_bind = ?, updated_at = `+s.dialect.Now()+`
		WHERE person_id = ?`, baseline, *resource.PersonID); err != nil {
		return fmt.Errorf("transfer CardDAV imported person cleanup baseline: %w", err)
	}
	return nil
}

func (s *Store) GetCardDAVResourceContext(
	ctx context.Context, bookID int64, href string,
) (*CardDAVResource, error) {
	resource, err := scanCardDAVResource(s.db.QueryRowContext(ctx,
		cardDAVResourceSelect+` WHERE address_book_id = ? AND href = ?`, bookID, href))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVResourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV resource: %w", err)
	}
	return resource, nil
}

// GetCardDAVResourceForPersonContext returns the person's mapping in one
// address book. Task 6 will tighten role transitions; publication already
// scopes this lookup to the configured write target.
func (s *Store) GetCardDAVResourceForPersonContext(
	ctx context.Context, bookID, personID int64,
) (*CardDAVResource, error) {
	return findCardDAVResourceForPersonTx(ctx, s.db, bookID, personID, "")
}

func (s *Store) ListCardDAVResourcesContext(
	ctx context.Context, bookID int64,
) ([]CardDAVResource, error) {
	rows, err := s.db.QueryContext(ctx, cardDAVResourceSelect+`
		WHERE address_book_id = ? ORDER BY href`, bookID)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV resources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	resources := []CardDAVResource{}
	for rows.Next() {
		resource, err := scanCardDAVResource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan CardDAV resource list: %w", err)
		}
		resources = append(resources, *resource)
	}
	return resources, rows.Err()
}

func (s *Store) findCardDAVResourceTx(
	ctx context.Context, tx *loggedTx, bookID int64, href string,
) (*CardDAVResource, error) {
	resource, err := scanCardDAVResource(tx.QueryRowContext(ctx,
		cardDAVResourceSelect+` WHERE address_book_id = ? AND href = ?`+
			s.dialect.SelectForUpdate(), bookID, href))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVResourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find CardDAV resource: %w", err)
	}
	return resource, nil
}

const cardDAVResourceSelect = `SELECT id, address_book_id, href, remote_uid,
	remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status,
	mapping_revision, governance, person_id, person_revision_at_bind,
	created_at, updated_at FROM carddav_resources`

func scanCardDAVResource(row scanner) (*CardDAVResource, error) {
	var resource CardDAVResource
	var uid sql.NullString
	var personID, personRevision sql.NullInt64
	if err := row.Scan(&resource.ID, &resource.AddressBookID, &resource.Href, &uid,
		&resource.RemoteETag, &resource.RemoteBody, &resource.RemoteSemanticHash,
		&resource.LocalHash, &resource.MappingStatus, &resource.MappingRevision,
		&resource.Governance, &personID, &personRevision,
		&resource.CreatedAt, &resource.UpdatedAt); err != nil {
		return nil, err
	}
	resource.RemoteUID = uid.String
	if personID.Valid {
		resource.PersonID = &personID.Int64
	}
	if personRevision.Valid {
		resource.PersonRevisionAtBind = &personRevision.Int64
	}
	resource.RemoteBody = append([]byte(nil), resource.RemoteBody...)
	return &resource, nil
}
