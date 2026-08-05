package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
)

// SourceIdentityCatalogStore is the read-only store capability used by the
// source identity catalog. Production stores implement it directly; keeping
// it feature-local avoids expanding lightweight MessageStore test doubles.
type SourceIdentityCatalogStore interface {
	GetSourceByIDContext(ctx context.Context, sourceID int64) (*store.Source, error)
	ListAccountIdentitiesContext(ctx context.Context, sourceID int64) ([]store.AccountIdentity, error)
}

type SourceIdentityResponse struct {
	Identifier  string    `json:"identifier"`
	Signals     []string  `json:"signals" nullable:"false"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

type SourceIdentitiesResponse struct {
	SourceID   int64                    `json:"source_id"`
	Account    string                   `json:"account"`
	Identities []SourceIdentityResponse `json:"identities" nullable:"false"`
}

func (s *Server) handleSourceIdentities(w http.ResponseWriter, r *http.Request) {
	sourceID, err := strconv.ParseInt(r.PathValue("source_id"), 10, 64)
	if err != nil || sourceID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_source_id", "Source ID must be a positive integer")
		return
	}

	catalog, ok := s.store.(SourceIdentityCatalogStore)
	if s.store == nil || !ok {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}

	source, err := catalog.GetSourceByIDContext(r.Context(), sourceID)
	switch {
	case errors.Is(err, store.ErrSourceNotFound):
		writeError(w, http.StatusNotFound, "source_not_found", "Source not found")
		return
	case err != nil:
		s.logger.Error("failed to load source identity catalog", "source_id", sourceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve source identities")
		return
	}

	stored, err := catalog.ListAccountIdentitiesContext(r.Context(), sourceID)
	if err != nil {
		s.logger.Error("failed to list source identities", "source_id", sourceID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve source identities")
		return
	}

	identities := make([]SourceIdentityResponse, 0, len(stored))
	for _, identity := range stored {
		identities = append(identities, SourceIdentityResponse{
			Identifier:  identity.Address,
			Signals:     identityops.SplitSignalSet(identity.SourceSignal),
			ConfirmedAt: identity.ConfirmedAt,
		})
	}
	sort.SliceStable(identities, func(i, j int) bool {
		return identities[i].Identifier < identities[j].Identifier
	})

	writeJSON(w, http.StatusOK, SourceIdentitiesResponse{
		SourceID: source.ID, Account: source.Identifier, Identities: identities,
	})
}
