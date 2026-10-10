package api

import (
	"context"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
)

// ScopedPersonMergeStore admits delegated merges and receipt replay only when
// the native transaction resolves every affected person and address book.
type ScopedPersonMergeStore interface {
	MergePersonsAuthorizedContext(ctx context.Context, request store.PersonMergeRequest, authorize store.PersonEditAuthorizer) (*store.PersonMergeResult, error)
}

func (s *Server) scopedPersonMergeAdmission(w http.ResponseWriter, r *http.Request, survivorID, absorbedID int64) (ScopedPersonMergeStore, store.PersonEditAuthorizer, string, bool) {
	auth := s.classifyAPIRequestDirect(r)
	if auth.Mode != AuthModeDelegated || auth.Grant == nil || !auth.Grant.HasPermission(agentgrant.PermissionPersonRead) || !auth.Grant.HasPermission(agentgrant.PermissionPersonMerge) {
		writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize this person merge")
		return nil, nil, "", false
	}
	for _, id := range []int64{survivorID, absorbedID} {
		if !slices.ContainsFunc(auth.Grant.Persons, func(person agentgrant.PersonRef) bool { return person.ID == id }) {
			writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize both merge targets")
			return nil, nil, "", false
		}
	}
	backend, ok := s.store.(ScopedPersonMergeStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "person_scope_unavailable", "Native scoped person merging is unavailable")
		return nil, nil, "", false
	}
	return backend, s.personMutationAuthorization(r, agentgrant.PermissionPersonMerge), "agent:" + auth.Grant.ID, true
}
