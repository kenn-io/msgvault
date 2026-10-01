package api

import (
	"context"
	"net/http"
)

// LaneReadiness contains only structured operator facts. Provider health is not
// probed; initialized means the owning local service is installed, not that an
// upload or query is currently authorized.
type LaneReadiness struct {
	Lane            string            `json:"lane" enum:"text_search,person_search,visual_search,documents,document_vectors,people_inference,activity,media_policy"`
	Enabled         bool              `json:"enabled"`
	Configured      bool              `json:"configured"`
	Initialized     bool              `json:"initialized"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Schedule        string            `json:"schedule,omitempty"`
	CredentialState string            `json:"credential_state" enum:"available,missing,unknown,not_required"`
	ConsentState    string            `json:"consent_state" enum:"active,missing,stale,unknown,not_required"`
	ConsentPurposes map[string]string `json:"consent_purposes,omitempty"`
	PendingRestart  bool              `json:"pending_restart"`
	Blockers        []string          `json:"blockers"`
}

type LaneReadinessResponse struct {
	StoreAvailable bool            `json:"store_available"`
	PendingRestart bool            `json:"pending_restart"`
	Lanes          []LaneReadiness `json:"lanes"`
}

// LaneRuntimeSnapshot is taken under the owning runtime locks. It contains no
// errors, host paths, endpoint values or credentials.
type LaneRuntimeSnapshot struct {
	Initialized    map[string]bool
	VectorStatus   VectorStatus
	PendingRestart bool
}

type LaneReadinessReader func(context.Context, LaneRuntimeSnapshot) (LaneReadinessResponse, error)

func (s *Server) handleLaneReadiness(w http.ResponseWriter, r *http.Request) {
	if s.laneReadinessReader == nil {
		writeError(w, http.StatusServiceUnavailable, "lane_readiness_unavailable", "Lane readiness is unavailable")
		return
	}
	s.vectorMu.RLock()
	runtime := LaneRuntimeSnapshot{Initialized: map[string]bool{
		"text_search":   s.hybridEngine != nil && s.backend != nil,
		"person_search": s.personSearchEngine != nil,
		"visual_search": s.visualPolicy != nil && s.visualStatus != nil,
		"documents":     s.store != nil,
		"media_policy":  s.cfg != nil,
	}, VectorStatus: s.vectorStatus, PendingRestart: s.settingsPendingRestart.Load()}
	s.vectorMu.RUnlock()
	s.documentSearchMu.RLock()
	runtime.Initialized["document_vectors"] = s.documentSearch != nil
	s.documentSearchMu.RUnlock()
	runtime.Initialized["people_inference"] = s.personBriefGeneratorFunc() != nil
	runtime.Initialized["activity"] = s.scheduler != nil && s.scheduler.IsJobScheduled("activity-projection")
	report, err := s.laneReadinessReader(r.Context(), runtime)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "lane_readiness_unavailable", "Lane readiness is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
