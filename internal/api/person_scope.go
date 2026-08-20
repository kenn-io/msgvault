package api

import (
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

func (s *Server) writePersonScopeError(
	w http.ResponseWriter,
	reference resolver.Reference,
	err error,
	operation string,
) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_not_found", "Person not found")
	case errors.Is(err, store.ErrPersonBindingConflict):
		writeError(w, http.StatusConflict, "person_binding_conflict",
			"The identity clusters belong to different person profiles")
	case errors.Is(err, resolver.ErrEmptyPopulation):
		writeError(w, http.StatusUnprocessableEntity, "person_scope_empty",
			"Person has no resolved identities")
	case errors.Is(err, resolver.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "person_scope_unavailable",
			"Person scope resolution is unavailable")
	default:
		s.logger.Error("resolve "+operation+" person scope", "error", err,
			"reference_kind", reference.Kind, "reference_id", reference.ID)
		writeError(w, http.StatusInternalServerError, "person_scope_failed",
			"Person scope resolution failed")
	}
}
