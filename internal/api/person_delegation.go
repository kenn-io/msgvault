package api

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
)

var errPersonScopeDenied = errors.New("current principal does not authorize every affected person and address book")

// ScopedPersonEditStore admits delegated edits only when native ownership is
// checked inside the same transaction that applies the existing profile write.
type ScopedPersonEditStore interface {
	PersonEditScopeContext(ctx context.Context, personID int64) (*store.IdentityGrantSelection, error)
	PersonProfileEditScopeContext(ctx context.Context, personID int64) (*store.IdentityGrantSelection, error)
	UpdatePersonDisplayNameAuthorizedContext(ctx context.Context, personID, revision int64, name *string, authorize store.PersonEditAuthorizer) (*store.Person, error)
	ApplyPersonProfilePatchAuthorizedContext(ctx context.Context, personID, revision int64, patch store.PersonProfilePatch, authorize store.PersonEditAuthorizer) (*store.PersonProfile, error)
}

// ScopedPersonAttributeStore joins delegated value writes to native ownership
// authorization and slot compare-and-swap in the same transaction.
type ScopedPersonAttributeStore interface {
	PersonAttributeStore
	PersonProfileEditScopeContext(ctx context.Context, personID int64) (*store.IdentityGrantSelection, error)
	SetPersonAttributeValueAuthorizedContext(ctx context.Context, input store.PersonAttributeValueInput, authorize store.PersonEditAuthorizer) (*store.PersonAttributeWrite, error)
	SupersedePersonAttributeValueAuthorizedContext(ctx context.Context, input store.PersonAttributeSupersedeInput, authorize store.PersonEditAuthorizer) (*store.PersonAttributeWrite, error)
}

func (s *Server) scopedPersonAttributeAdmission(w http.ResponseWriter, r *http.Request, id, revision int64) (ScopedPersonAttributeStore, store.PersonEditAuthorizer, bool) {
	backend, ok := s.store.(ScopedPersonAttributeStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "person_scope_unavailable", "Native scoped attribute editing is unavailable")
		return nil, nil, false
	}
	checkScope := s.personEditAuthorization(r)
	if s.requestAuthentication(r).Mode != AuthModeDelegated {
		checkScope = func(context.Context, *store.IdentityGrantSelection) error {
			principal, err := identityOperationPrincipal(s.classifyAPIRequestDirect(r))
			if err != nil || principal != "owner" {
				return errPersonScopeDenied
			}
			return nil
		}
	}
	authorize := func(ctx context.Context, scope *store.IdentityGrantSelection) error {
		if scope == nil {
			return errPersonScopeDenied
		}
		if err := checkScope(ctx, scope); err != nil {
			return err
		}
		for _, person := range scope.Persons {
			if person.ID == id && person.Revision == revision {
				return nil
			}
		}
		return store.ErrPersonRevisionConflict
	}
	scope, err := backend.PersonProfileEditScopeContext(r.Context(), id)
	if err == nil {
		err = authorize(r.Context(), scope)
	}
	if err != nil {
		s.writePersonError(w, err)
		return nil, nil, false
	}
	return backend, authorize, true
}

func (s *Server) admitPersonTarget(w http.ResponseWriter, r *http.Request, id int64, write bool) bool {
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode != AuthModeDelegated {
		if _, err := identityOperationPrincipal(auth); err == nil {
			return true
		}
	} else if auth.Grant != nil && auth.Grant.HasPermission(agentgrant.PermissionPersonRead) && (!write || auth.Grant.HasPermission(agentgrant.PermissionPersonEdit)) {
		for _, person := range auth.Grant.Persons {
			if person.ID == id {
				return true
			}
		}
	}
	writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize this person")
	return false
}

func (s *Server) admitPersonRead(w http.ResponseWriter, r *http.Request, person *store.Person) bool {
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode != AuthModeDelegated {
		if _, err := identityOperationPrincipal(auth); err != nil {
			writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize this person")
			return false
		}
		return true
	}
	if person == nil || auth.Grant == nil || !auth.Grant.AllowsPerson(agentgrant.PermissionPersonRead, agentgrant.PersonRef{ID: person.ID, UID: person.VCardUID}) {
		writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize this person")
		return false
	}
	return true
}

func (s *Server) personEditAuthorization(r *http.Request) store.PersonEditAuthorizer {
	return s.personMutationAuthorization(r, agentgrant.PermissionPersonEdit)
}

func (s *Server) personMutationAuthorization(r *http.Request, permission agentgrant.Permission) store.PersonEditAuthorizer {
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode != AuthModeDelegated || auth.Grant == nil {
		return func(context.Context, *store.IdentityGrantSelection) error { return errPersonScopeDenied }
	}
	principal := auth.Grant.ID
	return func(_ context.Context, scope *store.IdentityGrantSelection) error {
		current := s.classifyAPIRequestDirect(r)
		if current.Mode != AuthModeDelegated || current.Grant == nil || current.Grant.ID != principal || scope == nil || len(scope.Persons) == 0 {
			return errPersonScopeDenied
		}
		for _, person := range scope.Persons {
			ref := agentgrant.PersonRef{ID: person.ID, UID: person.UID}
			if !current.Grant.AllowsPerson(agentgrant.PermissionPersonRead, ref) || !current.Grant.AllowsPerson(permission, ref) {
				return errPersonScopeDenied
			}
		}
		for _, book := range scope.AddressBooks {
			ref := agentgrant.AddressBookRef{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint}
			if !current.Grant.AllowsAddressBook(agentgrant.PermissionCardDAVWrite, ref) {
				return errPersonScopeDenied
			}
		}
		return nil
	}
}

func (s *Server) scopedPersonEditAdmission(w http.ResponseWriter, r *http.Request, id int64, structured bool) (ScopedPersonEditStore, store.PersonEditAuthorizer, bool) {
	backend, ok := s.store.(ScopedPersonEditStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "person_scope_unavailable", "Native scoped person editing is unavailable")
		return nil, nil, false
	}
	authorize := s.personEditAuthorization(r)
	var scope *store.IdentityGrantSelection
	var err error
	if structured {
		scope, err = backend.PersonProfileEditScopeContext(r.Context(), id)
	} else {
		scope, err = backend.PersonEditScopeContext(r.Context(), id)
	}
	if err == nil {
		err = authorize(r.Context(), scope)
	}
	if err != nil {
		s.writePersonError(w, err)
		return nil, nil, false
	}
	return backend, authorize, true
}

func (s *Server) beginScopedPersonEdit(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if s.operationGate == nil {
		return func() {}, true
	}
	release, ok := beginGateWorkBounded(r.Context(), s.operationGate, "person profile edit")
	if !ok {
		writeOperationGateBusy(w, r, s.operationGate)
	}
	return release, ok
}
