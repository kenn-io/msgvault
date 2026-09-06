package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const MaxCardDAVConflictSnapshotBytes = 32 << 20

type CardDAVConflictStatus string

const (
	CardDAVConflictUnresolved CardDAVConflictStatus = "unresolved"
	CardDAVConflictResolved   CardDAVConflictStatus = "resolved"
)

type CardDAVConflictResolution string

const (
	CardDAVResolutionKeepLocal  CardDAVConflictResolution = "keep_local"
	CardDAVResolutionKeepRemote CardDAVConflictResolution = "keep_remote"
)

var (
	ErrCardDAVConflictNotFound   = errors.New("CardDAV conflict not found")
	ErrCardDAVConflictStale      = errors.New("CardDAV conflict is stale or already resolved")
	ErrCardDAVConflictTooLarge   = errors.New("CardDAV conflict snapshots exceed 32 MiB")
	ErrCardDAVConflictResolution = errors.New("invalid CardDAV conflict resolution")
)

type CardDAVConflict struct {
	ID                      int64
	AddressBookID           int64
	Href                    string
	BaseLocalHash           string
	LocalHash               string
	BaseRemoteHash          string
	BaseRemoteETag          string
	RemoteETag              string
	MappingRevision         int64
	LocalBody               []byte
	RemoteBody              []byte
	LocalTombstone          bool
	RemoteTombstone         bool
	PendingOperation        CardDAVMutationOperation
	ConnectionGeneration    int64
	BookSyncRevision        int64
	PreviousMappingRevision int64
	PendingStartedAt        *time.Time
	Status                  CardDAVConflictStatus
	Resolution              CardDAVConflictResolution
	ResolvedAt              *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// CardDAVConflictHeader is the compact, body-free read model used by public
// conflict lists. Sensitive mutation evidence deliberately remains absent.
type CardDAVConflictHeader struct {
	ID              int64
	AddressBookID   int64
	AddressBookName string
	Status          CardDAVConflictStatus
	LocalTombstone  bool
	RemoteTombstone bool
	UpdatedAt       time.Time
}

// CardDAVConflictDetailSource contains the internal evidence needed to build
// one bounded public projection. It must not cross the CardDAV service boundary.
type CardDAVConflictDetailSource struct {
	ID              int64
	AddressBookID   int64
	AddressBookName string
	MappingRevision int64
	LocalBody       []byte
	RemoteBody      []byte
	BaseBody        []byte
	LocalTombstone  bool
	RemoteTombstone bool
	BaseAvailable   bool
	Status          CardDAVConflictStatus
	Resolution      CardDAVConflictResolution
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ResolvedAt      *time.Time
}

type CardDAVConflictCapture struct {
	AddressBookID           int64
	Href                    string
	ExpectedMappingRevision int64
	BaseLocalHash           string
	LocalHash               string
	BaseRemoteHash          string
	BaseRemoteETag          string
	RemoteETag              string
	LocalBody               []byte
	RemoteBody              []byte
	LocalTombstone          bool
	RemoteTombstone         bool
}

type CardDAVConflictRemoteResolution struct {
	ConflictID              int64
	ExpectedMappingRevision int64
	Remote                  CardDAVRemoteResource
	RemoteTombstone         bool
}

type CardDAVConflictLocalPlan struct {
	ConflictID              int64
	ExpectedMappingRevision int64
	RemoteETag              string
	RemoteTombstone         bool
	OutgoingSemanticHash    string
}

type cardDAVConflictIdentity struct {
	AddressBookID int64
	Href          string
}

func (s *Store) getCardDAVConflictIdentityContext(
	ctx context.Context, conflictID int64,
) (cardDAVConflictIdentity, error) {
	var identity cardDAVConflictIdentity
	err := s.db.QueryRowContext(ctx, `SELECT address_book_id, href
		FROM carddav_conflicts WHERE id = ?`, conflictID).Scan(
		&identity.AddressBookID, &identity.Href)
	if errors.Is(err, sql.ErrNoRows) {
		return cardDAVConflictIdentity{}, ErrCardDAVConflictNotFound
	}
	if err != nil {
		return cardDAVConflictIdentity{}, fmt.Errorf("get CardDAV conflict identity: %w", err)
	}
	return identity, nil
}

func (s *Store) lockCardDAVConflictResolutionBookTx(
	ctx context.Context, tx *loggedTx, addressBookID int64,
) (CardDAVAddressBook, error) {
	var book CardDAVAddressBook
	err := tx.QueryRowContext(ctx, `SELECT id, account_id, canonical_url,
		is_write_target, is_subscribed, is_lookup_source, sync_revision,
		can_create, can_update, can_delete
		FROM carddav_address_books WHERE id = ?`+
		s.dialect.SelectForUpdate(), addressBookID).Scan(
		&book.ID, &book.AccountID, &book.CanonicalURL,
		&book.IsWriteTarget, &book.IsSubscribed, &book.IsLookupSource, &book.SyncRevision,
		&book.CanCreate, &book.CanUpdate, &book.CanDelete)
	if errors.Is(err, sql.ErrNoRows) {
		return CardDAVAddressBook{}, ErrCardDAVConflictStale
	}
	if err != nil {
		return CardDAVAddressBook{}, fmt.Errorf("lock CardDAV conflict book: %w", err)
	}
	return book, nil
}

func cardDAVConflictBookAllowsMutation(book CardDAVAddressBook, operation CardDAVMutationOperation) bool {
	if !book.IsSubscribed {
		return false
	}
	var capability *bool
	switch operation {
	case CardDAVMutationCreate:
		capability = book.CanCreate
	case CardDAVMutationUpdate:
		capability = book.CanUpdate
	case CardDAVMutationDelete:
		capability = book.CanDelete
	default:
		return false
	}
	// A missing privilege advertisement is not a denial. The remote mutation
	// still enforces capability through its HTTP result.
	return capability == nil || *capability
}

func (s *Store) PrepareCardDAVConflictLocalContext(
	ctx context.Context, plan CardDAVConflictLocalPlan,
) (*CardDAVPublication, error) {
	if plan.ConflictID <= 0 || plan.ExpectedMappingRevision <= 0 ||
		(!plan.RemoteTombstone && (strings.TrimSpace(plan.RemoteETag) == "" || plan.RemoteETag == "*")) ||
		strings.TrimSpace(plan.OutgoingSemanticHash) == "" {
		return nil, ErrCardDAVInvalidPlan
	}
	var prepared *CardDAVPublication
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		conflict, err := getCardDAVConflictFrom(ctx, tx, plan.ConflictID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if conflict.Status != CardDAVConflictUnresolved || conflict.MappingRevision != plan.ExpectedMappingRevision ||
			conflict.LocalTombstone || len(conflict.LocalBody) == 0 {
			return ErrCardDAVConflictStale
		}
		mapping, err := s.findCardDAVResourceTx(ctx, tx, conflict.AddressBookID, conflict.Href)
		if err != nil || mapping.PersonID == nil {
			return ErrCardDAVConflictStale
		}
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, *mapping.PersonID)
		if err != nil {
			return err
		}
		if snapshot.Fingerprint != conflict.LocalHash {
			return ErrCardDAVConflictStale
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			return err
		}
		book, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, conflict.AddressBookID)
		if err != nil {
			return err
		}
		operation := CardDAVMutationUpdate
		remoteETag := plan.RemoteETag
		if plan.RemoteTombstone {
			operation = CardDAVMutationCreate
			remoteETag = ""
		}
		if !cardDAVConflictBookAllowsMutation(book, operation) {
			return ErrCardDAVNoWriteTarget
		}
		publication, publicationErr := getCardDAVPublicationFrom(ctx, tx, *mapping.PersonID,
			s.dialect.SelectForUpdate())
		if publicationErr != nil && !errors.Is(publicationErr, ErrCardDAVPublicationNotFound) {
			return publicationErr
		}
		if publicationErr == nil && publication.PendingOperation != "" {
			if publication.AddressBookID != conflict.AddressBookID || publication.Href != conflict.Href ||
				publication.PreviousMappingRevision != conflict.MappingRevision ||
				publication.MappingRevision != mapping.MappingRevision {
				return ErrCardDAVConflictStale
			}
			publication.RecoveryOnly = true
			publication.ResolutionConflictID = conflict.ID
			prepared = publication
			return nil
		}
		if mapping.MappingRevision != plan.ExpectedMappingRevision {
			return ErrCardDAVConflictStale
		}
		nextMappingRevision := mapping.MappingRevision + 1
		result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND mapping_revision = ?`, nextMappingRevision, mapping.ID, mapping.MappingRevision)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		mutationRevision := int64(1)
		if publication != nil {
			mutationRevision = publication.MutationRevision + 1
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, `INSERT INTO carddav_publications (
			person_id, desired, address_book_id, href, pending_operation,
			outgoing_body, outgoing_semantic_hash, local_hash, remote_etag,
			connection_generation, book_sync_revision, mapping_revision,
			previous_mapping_revision, create_recovery_used, mutation_revision,
			pending_started_at, updated_at
		) VALUES (?, TRUE, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, FALSE, ?, ?, `+s.dialect.Now()+`)
		ON CONFLICT(person_id) DO UPDATE SET
			desired = TRUE, address_book_id = excluded.address_book_id, href = excluded.href,
			pending_operation = excluded.pending_operation, outgoing_body = excluded.outgoing_body,
			outgoing_semantic_hash = excluded.outgoing_semantic_hash,
			local_hash = excluded.local_hash, remote_etag = excluded.remote_etag,
			connection_generation = excluded.connection_generation,
			book_sync_revision = excluded.book_sync_revision,
			mapping_revision = excluded.mapping_revision,
			previous_mapping_revision = excluded.previous_mapping_revision,
			create_recovery_used = FALSE, mutation_revision = excluded.mutation_revision,
			pending_started_at = excluded.pending_started_at, updated_at = `+s.dialect.Now(),
			*mapping.PersonID, conflict.AddressBookID, conflict.Href, operation,
			conflict.LocalBody, plan.OutgoingSemanticHash, conflict.LocalHash, remoteETag,
			generation, book.SyncRevision, nextMappingRevision, mapping.MappingRevision,
			mutationRevision, timeValue(&now))
		if err != nil {
			return fmt.Errorf("prepare keep-local CardDAV conflict mutation: %w", err)
		}
		prepared, err = getCardDAVPublicationFrom(ctx, tx, *mapping.PersonID, "")
		if err == nil {
			prepared.ResolutionConflictID = conflict.ID
		}
		return err
	})
	return prepared, err
}

func (s *Store) PrepareCardDAVConflictLocalTombstoneContext(
	ctx context.Context, conflictID, expectedMappingRevision int64,
	remote CardDAVRemoteResource,
) (*CardDAVPublication, error) {
	if conflictID <= 0 || expectedMappingRevision <= 0 || strings.TrimSpace(remote.Href) == "" ||
		strings.TrimSpace(remote.RemoteETag) == "" || remote.RemoteETag == "*" ||
		len(remote.RemoteBody) == 0 || strings.TrimSpace(remote.SemanticHash) == "" {
		return nil, ErrCardDAVInvalidPlan
	}
	if len(remote.RemoteBody) > MaxCardDAVConflictSnapshotBytes {
		return nil, ErrCardDAVConflictTooLarge
	}
	identity, err := s.getCardDAVConflictIdentityContext(ctx, conflictID)
	if err != nil {
		return nil, err
	}
	if s.cardDAVTombstonePrepareSnapshotHook != nil {
		s.cardDAVTombstonePrepareSnapshotHook()
	}
	var prepared *CardDAVPublication
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrCardDAVNoWriteTarget
			}
			return err
		}
		book, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, identity.AddressBookID)
		if err != nil {
			return err
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		mapping, err := s.findCardDAVResourceTx(ctx, tx, identity.AddressBookID, identity.Href)
		if errors.Is(err, ErrCardDAVResourceNotFound) {
			return ErrCardDAVConflictStale
		}
		if err != nil {
			return err
		}
		conflict, err := getCardDAVConflictFrom(ctx, tx, conflictID, s.dialect.SelectForUpdate())
		if errors.Is(err, ErrCardDAVConflictNotFound) {
			return ErrCardDAVConflictStale
		}
		if err != nil {
			return err
		}
		if conflict.Status != CardDAVConflictUnresolved || !conflict.LocalTombstone ||
			conflict.RemoteTombstone || conflict.AddressBookID != identity.AddressBookID ||
			conflict.Href != identity.Href || remote.Href != identity.Href ||
			conflict.MappingRevision != expectedMappingRevision ||
			mapping.AddressBookID != conflict.AddressBookID || mapping.Href != conflict.Href ||
			mapping.MappingRevision != expectedMappingRevision {
			return ErrCardDAVConflictStale
		}
		if err := s.validateCardDAVConflictLocalTombstoneMappingTx(ctx, tx, conflict, mapping); err != nil {
			return err
		}
		if conflict.PendingOperation != "" {
			if _, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict); err != nil {
				return err
			}
			prepared = cardDAVPublicationFromConflict(conflict)
			prepared.RecoveryOnly = true
			return nil
		}
		if !cardDAVConflictBookAllowsMutation(book, CardDAVMutationDelete) {
			return ErrCardDAVNoWriteTarget
		}
		nextRevision := mapping.MappingRevision + 1
		result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND mapping_revision = ?`, nextRevision, mapping.ID, mapping.MappingRevision)
		if err != nil {
			return fmt.Errorf("advance keep-local tombstone mapping fence: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		now := time.Now().UTC()
		row := tx.QueryRowContext(ctx, `UPDATE carddav_conflicts SET
			remote_etag = ?, remote_body = ?, remote_tombstone = FALSE,
			mapping_revision = ?, pending_operation = ?, connection_generation = ?,
			book_sync_revision = ?, previous_mapping_revision = ?, pending_started_at = ?,
			updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND status = 'unresolved' AND mapping_revision = ?
			RETURNING `+cardDAVConflictColumns,
			remote.RemoteETag, remote.RemoteBody, nextRevision, CardDAVMutationDelete,
			generation, book.SyncRevision, mapping.MappingRevision, timeValue(&now),
			conflict.ID, conflict.MappingRevision)
		conflict, err = scanCardDAVConflict(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardDAVConflictStale
		}
		if err != nil {
			return fmt.Errorf("persist keep-local tombstone intent: %w", err)
		}
		prepared = cardDAVPublicationFromConflict(conflict)
		return nil
	})
	return prepared, err
}

func (s *Store) RefreshCardDAVConflictMutationFenceContext(
	ctx context.Context, conflictID int64,
) (*CardDAVPublication, error) {
	var refreshed *CardDAVPublication
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		conflict, err := getCardDAVConflictFrom(ctx, tx, conflictID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		mapping, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict)
		if err != nil {
			return err
		}
		var generation, bookRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`).Scan(&generation); err != nil {
			return err
		}
		if generation != conflict.ConnectionGeneration {
			return ErrCardDAVStalePlan
		}
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision FROM carddav_address_books
			WHERE id = ?`, conflict.AddressBookID).Scan(&bookRevision); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `UPDATE carddav_conflicts SET
			book_sync_revision = ?, mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND status = 'unresolved' AND pending_operation = ?
			RETURNING `+cardDAVConflictColumns,
			bookRevision, mapping.MappingRevision, conflict.ID, CardDAVMutationDelete)
		conflict, err = scanCardDAVConflict(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardDAVConflictStale
		}
		if err != nil {
			return err
		}
		refreshed = cardDAVPublicationFromConflict(conflict)
		refreshed.RecoveryOnly = true
		return nil
	})
	return refreshed, err
}

func (s *Store) CommitCardDAVConflictLocalTombstoneContext(
	ctx context.Context, input CardDAVCanonicalMutation,
) error {
	pending := input.Publication
	identity, err := s.getCardDAVConflictIdentityContext(ctx, pending.ResolutionConflictID)
	if err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT connection_generation FROM carddav_accounts
			WHERE id = 1`+s.dialect.SelectForUpdate()).Scan(&generation); err != nil {
			return err
		}
		book, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, identity.AddressBookID)
		if err != nil {
			return err
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		mapping, err := s.findCardDAVResourceTx(ctx, tx, identity.AddressBookID, identity.Href)
		if err != nil {
			return ErrCardDAVConflictStale
		}
		conflict, err := getCardDAVConflictFrom(ctx, tx, pending.ResolutionConflictID,
			s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if conflict.AddressBookID != identity.AddressBookID || conflict.Href != identity.Href ||
			mapping.AddressBookID != conflict.AddressBookID || mapping.Href != conflict.Href {
			return ErrCardDAVConflictStale
		}
		validatedMapping, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict)
		if err != nil {
			return err
		}
		if pending.MappingRevision != conflict.MappingRevision ||
			pending.ConnectionGeneration != conflict.ConnectionGeneration ||
			pending.BookSyncRevision != conflict.BookSyncRevision || !input.Tombstone {
			return ErrCardDAVPublicationMismatch
		}
		if generation != conflict.ConnectionGeneration || book.SyncRevision != conflict.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		_, err = s.completeCardDAVConflictLocalTombstoneTx(ctx, tx, conflict, validatedMapping)
		return err
	})
}

func (s *Store) ResetCardDAVConflictLocalTombstoneContext(
	ctx context.Context, conflictID int64, remote CardDAVRemoteResource,
) (*CardDAVConflict, error) {
	if conflictID <= 0 || remote.Href == "" || remote.RemoteETag == "" ||
		len(remote.RemoteBody) == 0 || remote.SemanticHash == "" {
		return nil, ErrCardDAVInvalidPlan
	}
	if len(remote.RemoteBody) > MaxCardDAVConflictSnapshotBytes {
		return nil, ErrCardDAVConflictTooLarge
	}
	var reset *CardDAVConflict
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		conflict, err := getCardDAVConflictFrom(ctx, tx, conflictID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		mapping, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict)
		if err != nil || remote.Href != conflict.Href {
			return ErrCardDAVConflictStale
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND mapping_revision = ?`,
			conflict.PreviousMappingRevision, mapping.ID, conflict.MappingRevision)
		if err != nil {
			return fmt.Errorf("restore keep-local tombstone mapping fence: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		row := tx.QueryRowContext(ctx, `UPDATE carddav_conflicts SET
			remote_etag = ?, remote_body = ?, remote_tombstone = FALSE,
			mapping_revision = previous_mapping_revision, pending_operation = NULL,
			connection_generation = NULL, book_sync_revision = NULL,
			previous_mapping_revision = NULL, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+` WHERE id = ? AND status = 'unresolved'
			RETURNING `+cardDAVConflictColumns,
			remote.RemoteETag, remote.RemoteBody, conflict.ID)
		reset, err = scanCardDAVConflict(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCardDAVConflictStale
		}
		return err
	})
	return reset, err
}

// RollbackCardDAVConflictMutationContext clears a definitively rejected
// keep-local tombstone and restores its mapping fence without changing the
// canonical remote snapshot retained by the unresolved conflict.
func (s *Store) RollbackCardDAVConflictMutationContext(
	ctx context.Context, pending *CardDAVPublication,
) error {
	if pending == nil || pending.ResolutionConflictID <= 0 ||
		pending.PendingOperation != CardDAVMutationDelete {
		return ErrCardDAVInvalidPlan
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		conflict, err := getCardDAVConflictFrom(ctx, tx, pending.ResolutionConflictID,
			s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		mapping, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict)
		if err != nil || conflict.MappingRevision != pending.MappingRevision ||
			conflict.PreviousMappingRevision != pending.PreviousMappingRevision {
			return ErrCardDAVConflictStale
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND mapping_revision = ?`,
			conflict.PreviousMappingRevision, mapping.ID, conflict.MappingRevision)
		if err != nil {
			return fmt.Errorf("restore rejected keep-local tombstone mapping fence: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		result, err = tx.ExecContext(ctx, `UPDATE carddav_conflicts SET
			mapping_revision = previous_mapping_revision, pending_operation = NULL,
			connection_generation = NULL, book_sync_revision = NULL,
			previous_mapping_revision = NULL, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND status = 'unresolved' AND pending_operation = ?`,
			conflict.ID, CardDAVMutationDelete)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		return nil
	})
}

func (s *Store) validatePendingCardDAVConflictTombstoneTx(
	ctx context.Context, tx *loggedTx, conflict *CardDAVConflict,
) (*CardDAVResource, error) {
	if conflict == nil || conflict.Status != CardDAVConflictUnresolved ||
		!conflict.LocalTombstone || conflict.PendingOperation != CardDAVMutationDelete ||
		conflict.ConnectionGeneration <= 0 || conflict.BookSyncRevision < 0 ||
		conflict.PreviousMappingRevision <= 0 || conflict.MappingRevision <= conflict.PreviousMappingRevision {
		return nil, ErrCardDAVConflictStale
	}
	mapping, err := s.findCardDAVResourceTx(ctx, tx, conflict.AddressBookID, conflict.Href)
	if err != nil || mapping.MappingRevision != conflict.MappingRevision {
		return nil, ErrCardDAVConflictStale
	}
	if err := s.validateCardDAVConflictLocalTombstoneMappingTx(ctx, tx, conflict, mapping); err != nil {
		return nil, err
	}
	return mapping, nil
}

func (s *Store) validateCardDAVConflictLocalTombstoneMappingTx(
	ctx context.Context, tx *loggedTx, conflict *CardDAVConflict, mapping *CardDAVResource,
) error {
	if conflict == nil || mapping == nil || mapping.AddressBookID != conflict.AddressBookID ||
		mapping.Href != conflict.Href {
		return ErrCardDAVConflictStale
	}
	if mapping.PersonID == nil {
		return nil
	}
	publication, err := getCardDAVPublicationFrom(ctx, tx, *mapping.PersonID,
		s.dialect.SelectForUpdate())
	if errors.Is(err, ErrCardDAVPublicationNotFound) {
		return ErrCardDAVConflictStale
	}
	if err != nil {
		return err
	}
	if publication.Desired || publication.PendingOperation != "" ||
		publication.AddressBookID != conflict.AddressBookID || publication.Href != conflict.Href {
		return ErrCardDAVConflictStale
	}
	return nil
}

func (s *Store) completeCardDAVConflictLocalTombstoneTx(
	ctx context.Context, tx *loggedTx, conflict *CardDAVConflict, mapping *CardDAVResource,
) (*CardDAVConflict, error) {
	retainPerson := mapping.PersonID != nil
	removed, err := s.removeCardDAVResourceWithPersonRetentionTx(
		ctx, tx, conflict.AddressBookID, conflict.Href, retainPerson)
	if err != nil {
		return nil, err
	}
	if !removed {
		return nil, ErrCardDAVConflictStale
	}
	if retainPerson {
		result, err := tx.ExecContext(ctx, `DELETE FROM carddav_publications
			WHERE person_id = ? AND desired = FALSE AND address_book_id = ? AND href = ?`,
			*mapping.PersonID, conflict.AddressBookID, conflict.Href)
		if err != nil {
			return nil, fmt.Errorf("clear resolved CardDAV unpublish intent: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, ErrCardDAVConflictStale
		}
	}
	return resolveCardDAVConflictAuditTx(ctx, tx, s.dialect,
		conflict.ID, CardDAVResolutionKeepLocal)
}

func (s *Store) completePendingCardDAVConflictTombstoneFromPullTx(
	ctx context.Context, tx *loggedTx, book CardDAVAddressBook, connectionGeneration int64,
	capture CardDAVConflictCapture,
) (bool, error) {
	conflict, err := scanCardDAVConflict(tx.QueryRowContext(ctx,
		`SELECT `+cardDAVConflictColumns+` FROM carddav_conflicts
		 WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`+
			s.dialect.SelectForUpdate(), book.ID, capture.Href))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock pulled CardDAV tombstone conflict: %w", err)
	}
	if conflict.PendingOperation == "" {
		return false, nil
	}
	mapping, err := s.validatePendingCardDAVConflictTombstoneTx(ctx, tx, conflict)
	if err != nil {
		return false, err
	}
	if !capture.LocalTombstone || !capture.RemoteTombstone ||
		capture.AddressBookID != conflict.AddressBookID || capture.Href != conflict.Href ||
		capture.ExpectedMappingRevision != mapping.MappingRevision ||
		capture.BaseLocalHash != mapping.LocalHash || capture.LocalHash != mapping.LocalHash ||
		capture.BaseRemoteHash != mapping.RemoteSemanticHash ||
		capture.BaseRemoteETag != mapping.RemoteETag ||
		conflict.ConnectionGeneration != connectionGeneration ||
		conflict.BookSyncRevision != book.SyncRevision {
		return false, ErrCardDAVConflictStale
	}
	if _, err := s.completeCardDAVConflictLocalTombstoneTx(ctx, tx, conflict, mapping); err != nil {
		return false, err
	}
	return true, nil
}

func cardDAVPublicationFromConflict(conflict *CardDAVConflict) *CardDAVPublication {
	return &CardDAVPublication{
		Desired: false, AddressBookID: conflict.AddressBookID, Href: conflict.Href,
		PendingOperation: conflict.PendingOperation, RemoteETag: conflict.RemoteETag,
		ConnectionGeneration: conflict.ConnectionGeneration, BookSyncRevision: conflict.BookSyncRevision,
		MappingRevision: conflict.MappingRevision, PreviousMappingRevision: conflict.PreviousMappingRevision,
		PendingStartedAt: conflict.PendingStartedAt, ResolutionConflictID: conflict.ID,
	}
}

func (s *Store) RecordCardDAVPublicationConflictContext(
	ctx context.Context, pending CardDAVPublication, capture CardDAVConflictCapture,
) (*CardDAVConflict, error) {
	return s.recordCardDAVPublicationConflictContext(ctx, pending, capture, false)
}

func (s *Store) RecordCardDAVPublicationConflictRetainingIntentContext(
	ctx context.Context, pending CardDAVPublication, capture CardDAVConflictCapture,
) (*CardDAVConflict, error) {
	return s.recordCardDAVPublicationConflictContext(ctx, pending, capture, true)
}

func (s *Store) recordCardDAVPublicationConflictContext(
	ctx context.Context, pending CardDAVPublication, capture CardDAVConflictCapture,
	retainOversizeIntent bool,
) (*CardDAVConflict, error) {
	if err := validateCardDAVConflictCapture(capture); err != nil {
		if errors.Is(err, ErrCardDAVConflictTooLarge) {
			rollbackErr := s.rollbackOversizedCardDAVPublicationConflictContext(
				ctx, pending, retainOversizeIntent)
			if rollbackErr != nil {
				return nil, errors.Join(err, rollbackErr)
			}
		}
		return nil, err
	}
	var conflict *CardDAVConflict
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := getCardDAVPublicationFrom(ctx, tx, pending.PersonID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if current.MutationRevision != pending.MutationRevision ||
			current.PendingOperation != pending.PendingOperation ||
			current.AddressBookID != capture.AddressBookID || current.Href != capture.Href ||
			current.MappingRevision != capture.ExpectedMappingRevision {
			return ErrCardDAVConflictStale
		}
		conflict, err = s.recordCardDAVConflictTx(ctx, tx, capture, cardDAVConflictRecordOptions{
			allowIntentTombstone: true,
		})
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET
			pending_operation = NULL, outgoing_body = NULL,
			outgoing_semantic_hash = NULL, local_hash = NULL, remote_etag = NULL,
			connection_generation = NULL, book_sync_revision = NULL,
			mapping_revision = NULL, previous_mapping_revision = NULL,
			create_recovery_used = FALSE, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND mutation_revision = ?`, current.PersonID, current.MutationRevision)
		if err != nil {
			return fmt.Errorf("clear conflicted CardDAV publication intent: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		return nil
	})
	return conflict, err
}

func (s *Store) rollbackOversizedCardDAVPublicationConflictContext(
	ctx context.Context, pending CardDAVPublication, retainIntent bool,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		current, err := getCardDAVPublicationFrom(ctx, tx, pending.PersonID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if current.MutationRevision != pending.MutationRevision ||
			current.PendingOperation != pending.PendingOperation ||
			current.AddressBookID != pending.AddressBookID || current.Href != pending.Href ||
			current.MappingRevision != pending.MappingRevision ||
			current.PreviousMappingRevision != pending.PreviousMappingRevision {
			return ErrCardDAVConflictStale
		}
		resource, err := findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID,
			current.PersonID, s.dialect.SelectForUpdate())
		if err != nil || resource.Href != current.Href || resource.MappingRevision != current.MappingRevision {
			return ErrCardDAVConflictStale
		}
		if current.PendingOperation == CardDAVMutationCreate {
			if current.MappingRevision <= 0 || current.PreviousMappingRevision != current.MappingRevision ||
				resource.RemoteSemanticHash == current.OutgoingSemanticHash {
				return ErrCardDAVConflictStale
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidates
				WHERE (left_kind = ? AND left_id = ?) OR (right_kind = ? AND right_id = ?)`,
				IdentityMatchCardDAVResource, resource.ID, IdentityMatchCardDAVResource, resource.ID); err != nil {
				return fmt.Errorf("delete oversized CardDAV create collision candidates: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vcard_resource_envelopes
				WHERE source_ref = ? AND source_resource_uid = ?`,
				fmt.Sprintf("carddav:%d", current.AddressBookID), current.Href); err != nil {
				return fmt.Errorf("delete oversized CardDAV create collision envelope: %w", err)
			}
			result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
				local_hash = remote_semantic_hash, mapping_status = ?,
				mapping_revision = mapping_revision + 1, governance = ?,
				person_id = NULL, person_revision_at_bind = NULL,
				updated_at = `+s.dialect.Now()+`
				WHERE id = ? AND person_id = ? AND mapping_revision = ?`,
				CardDAVMappingUnbound, CardDAVGovernanceNone, resource.ID,
				current.PersonID, current.MappingRevision)
			if err != nil {
				return fmt.Errorf("demote oversized CardDAV create collision: %w", err)
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return ErrCardDAVConflictStale
			}
			result, err = tx.ExecContext(ctx, `DELETE FROM carddav_publications
				WHERE person_id = ? AND mutation_revision = ? AND pending_operation = ?`,
				current.PersonID, current.MutationRevision, CardDAVMutationCreate)
			if err != nil {
				return fmt.Errorf("cancel oversized CardDAV create publication: %w", err)
			}
			if affected, _ := result.RowsAffected(); affected != 1 {
				return ErrCardDAVConflictStale
			}
			return nil
		}
		if current.PreviousMappingRevision <= 0 {
			return ErrCardDAVConflictStale
		}
		result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
			mapping_revision = ?, updated_at = `+s.dialect.Now()+`
			WHERE id = ? AND mapping_revision = ?`,
			current.PreviousMappingRevision, resource.ID, current.MappingRevision)
		if err != nil {
			return fmt.Errorf("restore oversized CardDAV conflict mapping fence: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		if retainIntent {
			result, err = tx.ExecContext(ctx, `UPDATE carddav_publications SET
				mapping_revision = ?, previous_mapping_revision = ?,
				updated_at = `+s.dialect.Now()+`
				WHERE person_id = ? AND mutation_revision = ? AND pending_operation = ?`,
				current.PreviousMappingRevision, current.PreviousMappingRevision,
				current.PersonID, current.MutationRevision, current.PendingOperation)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE carddav_publications SET
				pending_operation = NULL, outgoing_body = NULL,
				outgoing_semantic_hash = NULL, local_hash = NULL, remote_etag = NULL,
				connection_generation = NULL, book_sync_revision = NULL,
				mapping_revision = NULL, previous_mapping_revision = NULL,
				create_recovery_used = FALSE, pending_started_at = NULL,
				updated_at = `+s.dialect.Now()+`
				WHERE person_id = ? AND mutation_revision = ? AND pending_operation = ?`,
				current.PersonID, current.MutationRevision, current.PendingOperation)
		}
		if err != nil {
			return fmt.Errorf("restore oversized CardDAV publication intent: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return ErrCardDAVConflictStale
		}
		return nil
	})
}

func validateCardDAVConflictCapture(capture CardDAVConflictCapture) error {
	if capture.AddressBookID <= 0 || strings.TrimSpace(capture.Href) == "" ||
		capture.ExpectedMappingRevision <= 0 || strings.TrimSpace(capture.BaseLocalHash) == "" ||
		strings.TrimSpace(capture.LocalHash) == "" ||
		strings.TrimSpace(capture.BaseRemoteHash) == "" || strings.TrimSpace(capture.BaseRemoteETag) == "" {
		return ErrCardDAVInvalidPlan
	}
	if (!capture.LocalTombstone && len(capture.LocalBody) == 0) ||
		(!capture.RemoteTombstone && len(capture.RemoteBody) == 0) {
		return ErrCardDAVInvalidPlan
	}
	size := 0
	if !capture.LocalTombstone {
		size += len(capture.LocalBody)
	}
	if !capture.RemoteTombstone {
		size += len(capture.RemoteBody)
	}
	if size > MaxCardDAVConflictSnapshotBytes {
		return ErrCardDAVConflictTooLarge
	}
	return nil
}

func (s *Store) RecordCardDAVConflictContext(
	ctx context.Context, capture CardDAVConflictCapture,
) (*CardDAVConflict, error) {
	if err := validateCardDAVConflictCapture(capture); err != nil {
		return nil, err
	}
	var conflict *CardDAVConflict
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		conflict, err = s.recordCardDAVConflictTx(ctx, tx, capture, cardDAVConflictRecordOptions{})
		return err
	})
	return conflict, err
}

type cardDAVConflictRecordOptions struct {
	allowIntentTombstone           bool
	supersedePendingIntent         bool
	preserveExistingLocalTombstone bool
}

func (s *Store) recordCardDAVConflictTx(
	ctx context.Context, tx *loggedTx, capture CardDAVConflictCapture,
	options cardDAVConflictRecordOptions,
) (*CardDAVConflict, error) {
	var mappingID, revision int64
	var baseLocalHash, baseRemoteHash, baseRemoteETag string
	var personID sql.NullInt64
	var mappingStatus CardDAVMappingStatus
	if err := tx.QueryRowContext(ctx, `SELECT id, mapping_revision, local_hash,
		remote_semantic_hash, remote_etag, person_id, mapping_status FROM carddav_resources
		WHERE address_book_id = ? AND href = ?`+s.dialect.SelectForUpdate(),
		capture.AddressBookID, capture.Href).Scan(&mappingID, &revision, &baseLocalHash,
		&baseRemoteHash, &baseRemoteETag, &personID, &mappingStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCardDAVConflictStale
		}
		return nil, fmt.Errorf("lock CardDAV conflict mapping: %w", err)
	}
	if revision != capture.ExpectedMappingRevision || baseLocalHash != capture.BaseLocalHash ||
		baseRemoteHash != capture.BaseRemoteHash || baseRemoteETag != capture.BaseRemoteETag ||
		mappingStatus != CardDAVMappingMapped {
		return nil, ErrCardDAVConflictStale
	}
	currentLocalHash := baseLocalHash
	localTombstone := !personID.Valid
	if personID.Valid {
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, personID.Int64)
		if err != nil {
			return nil, err
		}
		currentLocalHash = snapshot.Fingerprint
	}
	preservedLocalTombstone := false
	if options.preserveExistingLocalTombstone && capture.LocalTombstone && !localTombstone {
		var existing bool
		err := tx.QueryRowContext(ctx, `SELECT local_tombstone FROM carddav_conflicts
			WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`,
			capture.AddressBookID, capture.Href).Scan(&existing)
		preservedLocalTombstone = err == nil && existing
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("load existing CardDAV tombstone intent: %w", err)
		}
	}
	if preservedLocalTombstone {
		currentLocalHash = baseLocalHash
	}
	if (!options.allowIntentTombstone && !preservedLocalTombstone && localTombstone != capture.LocalTombstone) ||
		currentLocalHash != capture.LocalHash {
		return nil, ErrCardDAVConflictStale
	}
	nextRevision := revision + 1
	result, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET
		mapping_revision = ?, updated_at = `+s.dialect.Now()+`
		WHERE id = ? AND mapping_revision = ?`, nextRevision, mappingID, revision)
	if err != nil {
		return nil, fmt.Errorf("advance CardDAV conflict mapping fence: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrCardDAVConflictStale
	}
	row := tx.QueryRowContext(ctx, `INSERT INTO carddav_conflicts (
		address_book_id, href, base_local_hash, local_hash, base_remote_hash,
		base_remote_etag, remote_etag, mapping_revision, local_body,
		remote_body, local_tombstone, remote_tombstone
	) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)
	ON CONFLICT(address_book_id, href) WHERE status = 'unresolved' DO UPDATE SET
		base_local_hash = excluded.base_local_hash,
		local_hash = excluded.local_hash,
		base_remote_hash = excluded.base_remote_hash,
		base_remote_etag = excluded.base_remote_etag,
		remote_etag = excluded.remote_etag,
		mapping_revision = excluded.mapping_revision,
		local_body = excluded.local_body,
		remote_body = excluded.remote_body,
		local_tombstone = excluded.local_tombstone,
		remote_tombstone = excluded.remote_tombstone,
		updated_at = `+s.dialect.Now()+`
	RETURNING `+cardDAVConflictColumns,
		capture.AddressBookID, capture.Href, capture.BaseLocalHash, capture.LocalHash,
		capture.BaseRemoteHash, capture.BaseRemoteETag, capture.RemoteETag,
		nextRevision, nullableConflictBody(capture.LocalBody, capture.LocalTombstone),
		nullableConflictBody(capture.RemoteBody, capture.RemoteTombstone),
		capture.LocalTombstone, capture.RemoteTombstone)
	conflict, err := scanCardDAVConflict(row)
	if err != nil {
		return nil, fmt.Errorf("record CardDAV conflict: %w", err)
	}
	if options.supersedePendingIntent && conflict.PendingOperation != "" {
		conflict, err = scanCardDAVConflict(tx.QueryRowContext(ctx, `UPDATE carddav_conflicts SET
			pending_operation = NULL, connection_generation = NULL, book_sync_revision = NULL,
			previous_mapping_revision = NULL, pending_started_at = NULL,
			updated_at = `+s.dialect.Now()+` WHERE id = ? AND status = 'unresolved'
			RETURNING `+cardDAVConflictColumns, conflict.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCardDAVConflictStale
		}
		if err != nil {
			return nil, fmt.Errorf("supersede refreshed CardDAV conflict intent: %w", err)
		}
	}
	return conflict, nil
}

func nullableConflictBody(body []byte, tombstone bool) any {
	if tombstone {
		return nil
	}
	return body
}

func (s *Store) GetCardDAVConflictContext(ctx context.Context, id int64) (*CardDAVConflict, error) {
	conflict, err := scanCardDAVConflict(s.db.QueryRowContext(ctx,
		`SELECT `+cardDAVConflictColumns+` FROM carddav_conflicts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVConflictNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV conflict: %w", err)
	}
	return conflict, nil
}

func (s *Store) GetUnresolvedCardDAVConflictForMappingContext(
	ctx context.Context, addressBookID int64, href string,
) (*CardDAVConflict, error) {
	conflict, err := scanCardDAVConflict(s.db.QueryRowContext(ctx,
		`SELECT `+cardDAVConflictColumns+` FROM carddav_conflicts
		 WHERE address_book_id = ? AND href = ? AND status = 'unresolved'`,
		addressBookID, href))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVConflictNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get unresolved CardDAV conflict: %w", err)
	}
	return conflict, nil
}

func (s *Store) ListCardDAVConflictsContext(
	ctx context.Context, unresolvedOnly bool,
) ([]CardDAVConflict, error) {
	query := `SELECT ` + cardDAVConflictColumns + ` FROM carddav_conflicts`
	if unresolvedOnly {
		query += ` WHERE status = 'unresolved'`
	}
	query += ` ORDER BY updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV conflicts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	conflicts := []CardDAVConflict{}
	for rows.Next() {
		conflict, err := scanCardDAVConflict(rows)
		if err != nil {
			return nil, fmt.Errorf("scan CardDAV conflict list: %w", err)
		}
		conflicts = append(conflicts, *conflict)
	}
	return conflicts, rows.Err()
}

func (s *Store) ListCardDAVConflictHeadersContext(ctx context.Context) ([]CardDAVConflictHeader, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.address_book_id, b.display_name,
		c.status, c.local_tombstone, c.remote_tombstone, c.updated_at
		FROM carddav_conflicts c
		JOIN carddav_address_books b ON b.id = c.address_book_id
		WHERE c.status = 'unresolved'
		ORDER BY c.updated_at DESC, c.id`)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV conflict headers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []CardDAVConflictHeader{}
	for rows.Next() {
		var header CardDAVConflictHeader
		if err := rows.Scan(&header.ID, &header.AddressBookID, &header.AddressBookName,
			&header.Status, &header.LocalTombstone, &header.RemoteTombstone, &header.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan CardDAV conflict header: %w", err)
		}
		result = append(result, header)
	}
	return result, rows.Err()
}

func (s *Store) GetCardDAVConflictDetailSourceContext(
	ctx context.Context, id int64,
) (*CardDAVConflictDetailSource, error) {
	var source CardDAVConflictDetailSource
	var resolution sql.NullString
	var resolvedAt sql.NullTime
	var baseBody []byte
	err := s.db.QueryRowContext(ctx, `SELECT c.id, c.address_book_id, b.display_name,
		c.mapping_revision, c.local_body, c.remote_body,
		c.local_tombstone, c.remote_tombstone, c.status, c.resolution,
		c.created_at, c.updated_at, c.resolved_at, r.remote_body
		FROM carddav_conflicts c
		JOIN carddav_address_books b ON b.id = c.address_book_id
		LEFT JOIN carddav_resources r ON r.address_book_id = c.address_book_id
			AND r.href = c.href
			AND r.mapping_revision = c.mapping_revision
			AND r.remote_semantic_hash = c.base_remote_hash
			AND r.remote_etag = c.base_remote_etag
		WHERE c.id = ?`, id).Scan(
		&source.ID, &source.AddressBookID, &source.AddressBookName,
		&source.MappingRevision, &source.LocalBody, &source.RemoteBody,
		&source.LocalTombstone, &source.RemoteTombstone, &source.Status, &resolution,
		&source.CreatedAt, &source.UpdatedAt, &resolvedAt, &baseBody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVConflictNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get CardDAV conflict detail source: %w", err)
	}
	source.Resolution = CardDAVConflictResolution(resolution.String)
	if resolvedAt.Valid {
		value := resolvedAt.Time.UTC()
		source.ResolvedAt = &value
	}
	if baseBody != nil {
		source.BaseAvailable = true
		source.BaseBody = append([]byte(nil), baseBody...)
	}
	source.LocalBody = append([]byte(nil), source.LocalBody...)
	source.RemoteBody = append([]byte(nil), source.RemoteBody...)
	return &source, nil
}

func (s *Store) ResolveCardDAVConflictRemoteContext(
	ctx context.Context, input CardDAVConflictRemoteResolution,
) (*CardDAVConflict, error) {
	if input.ConflictID <= 0 || input.ExpectedMappingRevision <= 0 {
		return nil, ErrCardDAVInvalidPlan
	}
	identity, err := s.getCardDAVConflictIdentityContext(ctx, input.ConflictID)
	if err != nil {
		return nil, err
	}
	if s.cardDAVConflictResolveSnapshotHook != nil {
		s.cardDAVConflictResolveSnapshotHook()
	}
	var resolved *CardDAVConflict
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		book, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, identity.AddressBookID)
		if err != nil {
			return err
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		mapping, err := s.findCardDAVResourceTx(ctx, tx, identity.AddressBookID, identity.Href)
		if err != nil || mapping.MappingRevision != input.ExpectedMappingRevision {
			return ErrCardDAVConflictStale
		}
		conflict, err := getCardDAVConflictFrom(ctx, tx, input.ConflictID, s.dialect.SelectForUpdate())
		if err != nil {
			if errors.Is(err, ErrCardDAVConflictNotFound) {
				return ErrCardDAVConflictStale
			}
			return err
		}
		if conflict.Status != CardDAVConflictUnresolved ||
			conflict.AddressBookID != identity.AddressBookID || conflict.Href != identity.Href ||
			conflict.MappingRevision != input.ExpectedMappingRevision ||
			mapping.AddressBookID != conflict.AddressBookID || mapping.Href != conflict.Href {
			return ErrCardDAVConflictStale
		}
		publicationPersonID := mapping.PersonID
		if input.RemoteTombstone != conflict.RemoteTombstone {
			return ErrCardDAVConflictStale
		}
		if input.RemoteTombstone {
			removed, err := s.removeCardDAVResourceTx(ctx, tx, conflict.AddressBookID, conflict.Href)
			if err != nil {
				return err
			}
			if !removed {
				return ErrCardDAVConflictStale
			}
			if publicationPersonID != nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM carddav_publications WHERE person_id = ?`,
					*publicationPersonID); err != nil {
					return fmt.Errorf("cancel retained CardDAV remote tombstone publication: %w", err)
				}
			}
		} else {
			if input.Remote.Href != conflict.Href || input.Remote.RemoteETag != conflict.RemoteETag ||
				!bytes.Equal(input.Remote.RemoteBody, conflict.RemoteBody) || input.Remote.SemanticHash == "" {
				return ErrCardDAVConflictStale
			}
			remoteOwnsDisplay := false
			if mapping.PersonID != nil {
				remoteOwnsDisplay, err = s.rebaseCardDAVImportedProjectionTx(
					ctx, tx, book.ID, *mapping.PersonID, input.Remote,
				)
				if err != nil {
					return err
				}
			}
			_, changed, err := s.applyCardDAVResourceTx(ctx, tx, book, input.Remote, false)
			if err != nil {
				return err
			}
			if !changed {
				return ErrCardDAVConflictStale
			}
			if mapping.PersonID != nil && mapping.Governance == CardDAVGovernanceRemote {
				if err := s.refreshCardDAVImportedPersonBindBaselineTx(
					ctx, tx, mapping.ID, *mapping.PersonID, remoteOwnsDisplay,
				); err != nil {
					return err
				}
			}
			if publicationPersonID != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE carddav_publications SET
					desired = TRUE, pending_operation = NULL, outgoing_body = NULL,
					outgoing_semantic_hash = NULL, local_hash = NULL, remote_etag = NULL,
					connection_generation = NULL, book_sync_revision = NULL,
					mapping_revision = NULL, previous_mapping_revision = NULL,
					create_recovery_used = FALSE, pending_started_at = NULL,
					updated_at = `+s.dialect.Now()+` WHERE person_id = ?`,
					*publicationPersonID); err != nil {
					return fmt.Errorf("retain CardDAV remote publication choice: %w", err)
				}
			}
		}
		resolved, err = resolveCardDAVConflictAuditTx(ctx, tx, s.dialect, conflict.ID,
			CardDAVResolutionKeepRemote)
		return err
	})
	return resolved, err
}

func (s *Store) ResolveCardDAVConflictLocalTombstoneContext(
	ctx context.Context, conflictID, expectedMappingRevision int64,
) (*CardDAVConflict, error) {
	if conflictID <= 0 || expectedMappingRevision <= 0 {
		return nil, ErrCardDAVInvalidPlan
	}
	identity, err := s.getCardDAVConflictIdentityContext(ctx, conflictID)
	if err != nil {
		return nil, err
	}
	var resolved *CardDAVConflict
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := s.lockCardDAVConflictResolutionBookTx(ctx, tx, identity.AddressBookID); err != nil {
			return err
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		mapping, err := s.findCardDAVResourceTx(ctx, tx, identity.AddressBookID, identity.Href)
		if err != nil || mapping.MappingRevision != expectedMappingRevision {
			return ErrCardDAVConflictStale
		}
		conflict, err := getCardDAVConflictFrom(ctx, tx, conflictID, s.dialect.SelectForUpdate())
		if err != nil {
			if errors.Is(err, ErrCardDAVConflictNotFound) {
				return ErrCardDAVConflictStale
			}
			return err
		}
		if conflict.Status != CardDAVConflictUnresolved || !conflict.LocalTombstone ||
			conflict.AddressBookID != identity.AddressBookID || conflict.Href != identity.Href ||
			conflict.MappingRevision != expectedMappingRevision ||
			mapping.AddressBookID != conflict.AddressBookID || mapping.Href != conflict.Href {
			return ErrCardDAVConflictStale
		}
		if err := s.validateCardDAVConflictLocalTombstoneMappingTx(ctx, tx, conflict, mapping); err != nil {
			return err
		}
		resolved, err = s.completeCardDAVConflictLocalTombstoneTx(ctx, tx, conflict, mapping)
		return err
	})
	return resolved, err
}

func resolveCardDAVConflictAuditTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, id int64,
	resolution CardDAVConflictResolution,
) (*CardDAVConflict, error) {
	if resolution != CardDAVResolutionKeepLocal && resolution != CardDAVResolutionKeepRemote {
		return nil, ErrCardDAVConflictResolution
	}
	conflict, err := scanCardDAVConflict(tx.QueryRowContext(ctx, `UPDATE carddav_conflicts SET
		status = 'resolved', resolution = ?, resolved_at = `+dialect.Now()+`,
		pending_operation = NULL, connection_generation = NULL, book_sync_revision = NULL,
		previous_mapping_revision = NULL, pending_started_at = NULL,
		updated_at = `+dialect.Now()+`
		WHERE id = ? AND status = 'unresolved' RETURNING `+cardDAVConflictColumns,
		resolution, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVConflictStale
	}
	if err != nil {
		return nil, fmt.Errorf("resolve CardDAV conflict audit: %w", err)
	}
	return conflict, nil
}

func (s *Store) SweepResolvedCardDAVConflictsContext(
	ctx context.Context, now time.Time,
) (int64, error) {
	cutoff := now.UTC().Add(-30 * 24 * time.Hour)
	query := `DELETE FROM carddav_conflicts
		WHERE status = 'resolved' AND resolved_at < ?`
	parameter := any(cutoff)
	if s.dialect.DriverName() != postgresDriverName {
		query = `DELETE FROM carddav_conflicts
			WHERE status = 'resolved' AND datetime(resolved_at) < datetime(?)`
		parameter = cutoff.Format(time.RFC3339Nano)
	}
	result, err := s.db.ExecContext(ctx, query, parameter)
	if err != nil {
		return 0, fmt.Errorf("sweep resolved CardDAV conflicts: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count swept CardDAV conflicts: %w", err)
	}
	return removed, nil
}

func getCardDAVConflictFrom(
	ctx context.Context, queryer contextRowQuerier, id int64, suffix string,
) (*CardDAVConflict, error) {
	conflict, err := scanCardDAVConflict(queryer.QueryRowContext(ctx,
		`SELECT `+cardDAVConflictColumns+` FROM carddav_conflicts WHERE id = ?`+suffix, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCardDAVConflictNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load CardDAV conflict: %w", err)
	}
	return conflict, nil
}

const cardDAVConflictColumns = `id, address_book_id, href, base_local_hash, local_hash,
	base_remote_hash, base_remote_etag, remote_etag, mapping_revision,
	local_body, remote_body, local_tombstone, remote_tombstone, pending_operation,
	connection_generation, book_sync_revision, previous_mapping_revision, pending_started_at, status,
	resolution, resolved_at, created_at, updated_at`

func scanCardDAVConflict(row scanner) (*CardDAVConflict, error) {
	var conflict CardDAVConflict
	var remoteETag, resolution sql.NullString
	var pendingOperation sql.NullString
	var connectionGeneration, bookSyncRevision, previousMappingRevision sql.NullInt64
	var localBody, remoteBody []byte
	var pendingStartedAt, resolvedAt sql.NullTime
	if err := row.Scan(&conflict.ID, &conflict.AddressBookID, &conflict.Href,
		&conflict.BaseLocalHash, &conflict.LocalHash, &conflict.BaseRemoteHash, &conflict.BaseRemoteETag,
		&remoteETag, &conflict.MappingRevision, &localBody, &remoteBody,
		&conflict.LocalTombstone, &conflict.RemoteTombstone, &pendingOperation,
		&connectionGeneration, &bookSyncRevision, &previousMappingRevision, &pendingStartedAt,
		&conflict.Status,
		&resolution, &resolvedAt, &conflict.CreatedAt, &conflict.UpdatedAt); err != nil {
		return nil, err
	}
	conflict.RemoteETag = remoteETag.String
	conflict.PendingOperation = CardDAVMutationOperation(pendingOperation.String)
	conflict.ConnectionGeneration = connectionGeneration.Int64
	conflict.BookSyncRevision = bookSyncRevision.Int64
	conflict.PreviousMappingRevision = previousMappingRevision.Int64
	if pendingStartedAt.Valid {
		value := pendingStartedAt.Time.UTC()
		conflict.PendingStartedAt = &value
	}
	conflict.Resolution = CardDAVConflictResolution(resolution.String)
	conflict.LocalBody = append([]byte(nil), localBody...)
	conflict.RemoteBody = append([]byte(nil), remoteBody...)
	if resolvedAt.Valid {
		value := resolvedAt.Time.UTC()
		conflict.ResolvedAt = &value
	}
	return &conflict, nil
}
