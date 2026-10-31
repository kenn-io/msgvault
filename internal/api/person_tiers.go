package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

// PersonTierStore reads the tier preview and explanations. Both only read.
type PersonTierStore interface {
	PreviewPersonTiersContext(ctx context.Context, options store.PersonTierOptions) (*store.PersonTierPreview, error)
	ExplainPersonTierContext(ctx context.Context, personID int64, options store.PersonTierOptions) (*store.PersonTierExplanation, error)
}

var _ PersonTierStore = (*store.Store)(nil)

func (s *Server) registerPersonTierRoutes(api huma.API) {
	preview := rawAPIV1Operation("previewPersonTiers", http.MethodGet,
		"/people/tiers", "List saved people in tier 1 or 2")
	preview.Responses = jsonResponsesFor[store.PersonTierPreview](api)
	addErrorResponses(api, preview.Responses, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, preview, s.handlePreviewPersonTiers)

	explain := rawAPIV1Operation("explainPersonTier", http.MethodGet,
		"/people/{id}/tier", "Show every count behind one person's tier")
	addPersonIDParameter(&explain)
	explain.Responses = jsonResponsesFor[store.PersonTierExplanation](api)
	addErrorResponses(api, explain.Responses, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, explain, s.handleExplainPersonTier)
}

func (s *Server) handlePreviewPersonTiers(w http.ResponseWriter, r *http.Request) {
	tiers, options, ok := s.personTierRequest(w)
	if !ok {
		return
	}
	preview, err := tiers.PreviewPersonTiersContext(r.Context(), options)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleExplainPersonTier(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	tiers, options, ok := s.personTierRequest(w)
	if !ok {
		return
	}
	explanation, err := tiers.ExplainPersonTierContext(r.Context(), personID, options)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, explanation)
}

// personTierRequest reads [people.tiers] and [activity] from the config file
// as it is now, the same settings a proxied "person tiers" command loads, so
// the HTTP and CLI answers agree after the owner edits thresholds.
func (s *Server) personTierRequest(w http.ResponseWriter) (PersonTierStore, store.PersonTierOptions, bool) {
	tiers, ok := s.store.(PersonTierStore)
	if !ok || s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "person_tiers_unavailable", "Person tiers are unavailable")
		return nil, store.PersonTierOptions{}, false
	}
	// A lenient load records a misspelled [people.tiers] key instead of
	// failing; refuse it rather than answer with the default it hides.
	cfg, err := s.currentPersonTierConfig()
	if err == nil {
		err = cfg.PeopleTiersError()
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "person_tiers_config_invalid", err.Error())
		return nil, store.PersonTierOptions{}, false
	}
	return tiers, store.PersonTierOptions{
		Config:            cfg.People.Tiers,
		ActivityTimezone:  cfg.Activity.Timezone,
		ActivityMaxDirect: cfg.Activity.MaxDirectCounterparts,
	}, true
}

func (s *Server) currentPersonTierConfig() (*config.Config, error) {
	snapshot, err := config.ReadConfigFile(s.cfg.ConfigFilePath())
	if err != nil {
		return nil, fmt.Errorf("read config for person tiers: %w", err)
	}
	if !snapshot.Exists {
		return s.cfg, nil
	}
	loaded, err := config.LoadClientConfigFile(snapshot, s.cfg.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("load config for person tiers: %w", err)
	}
	return loaded, nil
}

func (s *Server) writePersonTierError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrPersonTierRebuilding):
		writeError(w, http.StatusConflict, "activity_projection_rebuilding", err.Error())
	case errors.Is(err, persontiers.ErrInvalidConfig):
		writeError(w, http.StatusServiceUnavailable, "person_tiers_config_invalid", err.Error())
	default:
		s.writePersonError(w, err)
	}
}
