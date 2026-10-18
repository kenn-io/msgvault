package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

// ErrIdentityOperationReceiptDenied prevents disclosure after authority or a
// previously authorized resource identity changes, even if the preview expired.
var ErrIdentityOperationReceiptDenied = errors.New("current authority does not authorize the identity receipt")

// The stored envelope is private to the ledger. Public receipts contain only
// outcome identifiers and digests; resource names and credentials are never saved.
type identityStoredReceipt struct {
	IdentityReceipt

	AuthorityFingerprint string                    `json:"authority_fingerprint"`
	Scope                []identityReceiptResource `json:"scope"`
	Coverage             identityReceiptCoverage   `json:"coverage,omitempty"`
}

// identityReceiptCoverage preserves the membership evidence admitted by the
// committed operation. A successful unlink can remove that binding from the
// live archive even though the same principal still needs to recover its
// receipt.
type identityReceiptCoverage struct {
	Members             []int64                      `json:"members,omitempty"`
	Bindings            []IdentityBindingEvidence    `json:"bindings,omitempty"`
	SourceContributions []IdentitySourceContribution `json:"source_contributions,omitempty"`
}

type identityReceiptResource struct {
	Kind        string `json:"kind"`
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

func identityReceiptCoverageFor(snapshots ...*IdentitySnapshot) identityReceiptCoverage {
	coverage := identityReceiptCoverage{}
	for _, snapshot := range snapshots {
		if snapshot == nil {
			continue
		}
		coverage.Members = append(coverage.Members, snapshot.Members...)
		coverage.Bindings = append(coverage.Bindings, snapshot.Bindings...)
		coverage.SourceContributions = append(coverage.SourceContributions, snapshot.SourceContributions...)
	}
	coverage.Members = canonicalIdentityIDs(coverage.Members)
	slices.SortFunc(coverage.Bindings, func(a, b IdentityBindingEvidence) int {
		if n := cmp.Compare(a.ParticipantID, b.ParticipantID); n != 0 {
			return n
		}
		return cmp.Compare(a.PersonID, b.PersonID)
	})
	coverage.Bindings = slices.Compact(coverage.Bindings)
	slices.SortFunc(coverage.SourceContributions, func(a, b IdentitySourceContribution) int {
		if n := cmp.Compare(a.ParticipantID, b.ParticipantID); n != 0 {
			return n
		}
		return cmp.Compare(a.SourceID, b.SourceID)
	})
	coverage.SourceContributions = slices.Compact(coverage.SourceContributions)
	return coverage
}

func mergeIdentityReceiptCoverage(snapshot *IdentitySnapshot, coverage identityReceiptCoverage) {
	snapshot.Members = canonicalIdentityIDs(append(snapshot.Members, coverage.Members...))
	snapshot.Bindings = append(snapshot.Bindings, coverage.Bindings...)
	snapshot.SourceContributions = append(snapshot.SourceContributions, coverage.SourceContributions...)
	merged := identityReceiptCoverageFor(snapshot)
	snapshot.Members = merged.Members
	snapshot.Bindings = merged.Bindings
	snapshot.SourceContributions = merged.SourceContributions
}

func mergeIdentityReceiptAuthorizationEvidence(snapshot, original *IdentitySnapshot) error {
	sources, err := mergeIdentityReceiptResourceEvidence(snapshot.Sources, original.Sources,
		func(source IdentitySourceEvidence) int64 { return source.ID },
		func(current, original IdentitySourceEvidence) bool {
			return current.Type == original.Type && current.Identifier == original.Identifier && current.OwnershipFingerprint == original.OwnershipFingerprint
		},
	)
	if err != nil {
		return err
	}
	persons, err := mergeIdentityReceiptResourceEvidence(snapshot.Persons, original.Persons,
		func(person IdentityPersonEvidence) int64 { return person.ID },
		func(current, original IdentityPersonEvidence) bool { return current.UID == original.UID },
	)
	if err != nil {
		return err
	}
	accounts, err := mergeIdentityReceiptResourceEvidence(snapshot.Accounts, original.Accounts,
		func(account IdentityAccountEvidence) int64 { return account.ID },
		func(current, original IdentityAccountEvidence) bool {
			return current.OwnershipFingerprint == original.OwnershipFingerprint
		},
	)
	if err != nil {
		return err
	}
	books, err := mergeIdentityReceiptResourceEvidence(snapshot.Books, original.Books,
		func(book IdentityBookEvidence) int64 { return book.ID },
		func(current, original IdentityBookEvidence) bool {
			return current.AccountID == original.AccountID && current.CanonicalURL == original.CanonicalURL
		},
	)
	if err != nil {
		return err
	}
	snapshot.Sources = sources
	snapshot.Persons = persons
	snapshot.Accounts = accounts
	snapshot.Books = books
	mergeIdentityReceiptCoverage(snapshot, identityReceiptCoverageFromSnapshot(original))
	return nil
}

func mergeIdentityReceiptResourceEvidence[T any](current, original []T, id func(T) int64, sameIdentity func(T, T) bool) ([]T, error) {
	byID := make(map[int64]T, len(current)+len(original))
	add := func(evidence T) error {
		resourceID := id(evidence)
		if previous, ok := byID[resourceID]; ok && !sameIdentity(previous, evidence) {
			return ErrIdentityOperationReceiptDenied
		}
		byID[resourceID] = evidence
		return nil
	}
	// Seed saved evidence first, then retain current revisions and other live
	// fields when the same stable resource identity is present in both snapshots.
	for _, evidence := range original {
		if err := add(evidence); err != nil {
			return nil, err
		}
	}
	for _, evidence := range current {
		if err := add(evidence); err != nil {
			return nil, err
		}
	}
	merged := make([]T, 0, len(byID))
	for _, evidence := range byID {
		merged = append(merged, evidence)
	}
	slices.SortFunc(merged, func(a, b T) int { return cmp.Compare(id(a), id(b)) })
	return merged, nil
}

func identityReceiptCoverageFromSnapshot(snapshot *IdentitySnapshot) identityReceiptCoverage {
	return identityReceiptCoverageFor(snapshot)
}

func identityReceiptScopeFor(snapshots ...*IdentitySnapshot) ([]identityReceiptResource, error) {
	var resources []identityReceiptResource
	add := func(kind string, id int64, value any) error {
		fingerprint, err := identityEvidenceHash(value)
		if err != nil {
			return err
		}
		resources = append(resources, identityReceiptResource{kind, id, fingerprint})
		return nil
	}
	for _, snapshot := range snapshots {
		for _, source := range snapshot.Sources {
			if err := add("source", source.ID, source); err != nil {
				return nil, err
			}
		}
		for _, person := range snapshot.Persons {
			if err := add(string(AttributeObjectPerson), person.ID, struct {
				ID  int64
				UID string
			}{person.ID, person.UID}); err != nil {
				return nil, err
			}
		}
		owners := map[int64]string{}
		for _, account := range snapshot.Accounts {
			owners[account.ID] = account.OwnershipFingerprint
		}
		for _, book := range snapshot.Books {
			identity := IdentityGrantAddressBook{AccountID: book.AccountID, BookID: book.ID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: owners[book.AccountID]}
			if err := add("book", book.ID, identity); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(resources, func(a, b identityReceiptResource) int {
		if n := cmp.Compare(a.Kind, b.Kind); n != 0 {
			return n
		}
		if n := cmp.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		return cmp.Compare(a.Fingerprint, b.Fingerprint)
	})
	return slices.Compact(resources), nil
}

// identityReceiptScopeTx resolves the originally admitted resource IDs in the
// caller's native snapshot. Current UID, source ownership and book/account
// identities must still equal their saved digests. Revision changes are allowed.
func (s *Store) identityReceiptScopeTx(
	ctx context.Context,
	tx *loggedTx,
	expected []identityReceiptResource,
	coverage identityReceiptCoverage,
) (*IdentitySnapshot, error) {
	snapshot := &IdentitySnapshot{}
	var people, books, sources []int64
	for _, resource := range expected {
		switch resource.Kind {
		case string(AttributeObjectPerson):
			people = append(people, resource.ID)
		case "book":
			books = append(books, resource.ID)
		case "source":
			sources = append(sources, resource.ID)
		default:
			return nil, ErrIdentityOperationReceiptDenied
		}
	}
	selection, err := s.identityGrantSelectionTx(ctx, tx, canonicalIdentityIDs(people), canonicalIdentityIDs(books))
	if err != nil {
		if errors.Is(err, ErrPersonNotFound) || errors.Is(err, ErrCardDAVAddressBookNotFound) {
			return nil, ErrIdentityOperationReceiptDenied
		}
		return nil, err
	}
	snapshot.Persons = selection.Persons
	for _, book := range selection.AddressBooks {
		snapshot.Books = append(snapshot.Books, IdentityBookEvidence{ID: book.BookID, AccountID: book.AccountID, CanonicalURL: book.CanonicalURL})
		snapshot.Accounts = append(snapshot.Accounts, IdentityAccountEvidence{ID: book.AccountID, OwnershipFingerprint: book.OwnershipFingerprint})
	}
	if err := identitySnapshotSourceIDsTx(ctx, tx, snapshot, canonicalIdentityIDs(sources)); err != nil {
		return nil, err
	}
	mergeIdentityReceiptCoverage(snapshot, coverage)
	actual, err := identityReceiptScopeFor(snapshot)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(expected, actual) {
		return nil, ErrIdentityOperationReceiptDenied
	}
	return snapshot, nil
}

// IdentityOperationReceiptAuthorizedContext checks stored and current resource
// authority in one native read snapshot. It never verifies a preview or writes.
// The owner uses the separate trusted lookup to recover after archive cleanup.
func (s *Store) IdentityOperationReceiptAuthorizedContext(ctx context.Context, principal, key, authority string, authorize func(context.Context, *IdentitySnapshot) error) (*IdentityReceipt, error) {
	if principal == "" || len(principal) > 256 || key == "" || len(key) > 256 || len(authority) != 64 || authorize == nil {
		return nil, identitycontrol.ErrInvalidRequest
	}
	var stored identityStoredReceipt
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var encoded string
		if err := tx.QueryRowContext(ctx, `SELECT receipt_json FROM identity_operation_receipts WHERE principal=? AND idempotency_key=?`, principal, key).Scan(&encoded); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIdentityOperationReceiptNotFound
			}
			return err
		}
		if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
			return err
		}
		if stored.AuthorityFingerprint != authority {
			return ErrIdentityOperationReceiptDenied
		}
		original, err := s.identityReceiptScopeTx(ctx, tx, stored.Scope, stored.Coverage)
		if err != nil {
			return err
		}
		if err := authorize(ctx, original); err != nil {
			return err
		}
		current, err := s.identityOperationSnapshotTx(ctx, tx, stored.Operation, stored.Target)
		if err != nil {
			if errors.Is(err, ErrParticipantNotFound) || errors.Is(err, ErrPersonNotFound) || errors.Is(err, ErrIdentityOperationTooLarge) {
				return ErrIdentityOperationReceiptDenied
			}
			return err
		}
		if err := mergeIdentityReceiptAuthorizationEvidence(current, original); err != nil {
			return err
		}
		return authorize(ctx, current)
	})
	if err != nil {
		return nil, err
	}
	return &stored.IdentityReceipt, nil
}
