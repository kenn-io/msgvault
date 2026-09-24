package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchworker"
	"go.kenn.io/msgvault/internal/store"
)

const apiPersonMatchConsentActor = "local_operator"

type PersonMatchScoringStatus struct {
	Enabled                          bool                    `json:"enabled"`
	ModelID                          string                  `json:"model_id"`
	MinimumProbability               float64                 `json:"minimum_probability"`
	BatchSize                        int                     `json:"batch_size"`
	Disclosure                       *personmatch.Disclosure `json:"disclosure,omitempty"`
	DisclosureFingerprint            string                  `json:"disclosure_fingerprint,omitempty"`
	CredentialAvailable              bool                    `json:"credential_available"`
	ConsentActive                    bool                    `json:"consent_active"`
	DryRunReady                      bool                    `json:"dry_run_ready"`
	LiveAutomaticAcceptanceAvailable bool                    `json:"live_automatic_acceptance_available"`
	Blocker                          string                  `json:"blocker,omitempty"`
	LiveBlocker                      string                  `json:"live_blocker"`
}

type PersonMatchConsentDecisionRequest struct {
	DisclosureFingerprint string `json:"disclosure_fingerprint"`
}

type PersonMatchConsentDecisionResponse struct {
	DisclosureFingerprint string `json:"disclosure_fingerprint"`
	ConsentActive         bool   `json:"consent_active"`
	Changed               bool   `json:"changed"`
}

type PersonMatchDryRunRequest struct {
	DryRun bool `json:"dry_run"`
	Limit  int  `json:"limit,omitempty"`
}

type personMatchScoringRunRequest struct {
	DryRun *bool `json:"dry_run"`
	Limit  int   `json:"limit,omitempty"`
}

type PersonMatchDryRunResponse struct {
	Results   []personmatchworker.DryResult `json:"results"`
	Processed int                           `json:"processed"`
}

type PersonMatchJudgmentHistoryResponse struct {
	Judgments    []store.IdentityMatchJudgment `json:"judgments"`
	Limit        int                           `json:"limit"`
	CandidateID  int64                         `json:"candidate_id"`
	NextBeforeID *int64                        `json:"next_before_id,omitempty"`
}

type personMatchConsentStore interface {
	GrantPersonMatchConsentContext(ctx context.Context, disclosure personmatch.Disclosure, actor string) (*store.PersonMatchConsent, bool, error)
	RevokePersonMatchConsentContext(ctx context.Context, fingerprint string, actor string) (bool, error)
	HasPersonMatchConsentContext(ctx context.Context, fingerprint string) (bool, error)
}

type personMatchRunStore interface {
	personMatchConsentStore
	personmatchworker.Store
	ListIdentityMatchJudgmentsContext(ctx context.Context, candidateID int64, limit int, beforeID ...int64) ([]store.IdentityMatchJudgment, error)
}

func (s *Server) registerPersonMatchScoringRoutes(api huma.API) {
	status := rawAPIV1Operation("getPersonMatchScoringStatus", http.MethodGet,
		"/identity/scoring/status", "Get identity scoring configuration and consent status")
	status.Responses = jsonResponsesFor[PersonMatchScoringStatus](api)
	addErrorResponses(api, status.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, status, s.handlePersonMatchScoringStatus)
	for _, action := range []string{"consent", "revoke"} {
		summary := "Grant identity scoring consent"
		if action == "revoke" {
			summary = "Withdraw identity scoring consent"
		}
		op := rawAPIV1Operation("personMatchScoring"+strings.ToUpper(action[:1])+action[1:], http.MethodPost,
			"/identity/scoring/"+action, summary)
		op.RequestBody = jsonRequestBodyFor[PersonMatchConsentDecisionRequest](api)
		op.RequestBody.Required = true
		op.Responses = jsonResponsesFor[PersonMatchConsentDecisionResponse](api)
		addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
		if action == "consent" {
			registerRawHumaRoute(api, op, s.handlePersonMatchScoringConsent)
		} else {
			registerRawHumaRoute(api, op, s.handlePersonMatchScoringRevoke)
		}
	}
	run := rawAPIV1Operation("runPersonMatchScoring", http.MethodPost,
		"/identity/scoring/run", "Run a bounded dry scoring batch")
	run.RequestBody = jsonRequestBodyFor[PersonMatchDryRunRequest](api)
	run.RequestBody.Required = true
	run.Responses = jsonResponsesFor[PersonMatchDryRunResponse](api)
	addErrorResponses(api, run.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, run, s.handlePersonMatchScoringRun)
	history := rawAPIV1Operation("listPersonMatchJudgments", http.MethodGet,
		"/identity/scoring/history", "List redacted identity scoring judgments")
	history.Parameters = append(history.Parameters,
		queryIntegerParam("candidate_id", "Optional candidate ID; zero lists all"),
		queryIntegerParam("limit", "Maximum judgments"),
		queryIntegerParam("before_id", "Older judgments with ID below this cursor"))
	history.Responses = jsonResponsesFor[PersonMatchJudgmentHistoryResponse](api)
	addErrorResponses(api, history.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, history, s.handlePersonMatchScoringHistory)
}

func (s *Server) personMatchScoringStore(w http.ResponseWriter) (personMatchConsentStore, bool) {
	st, ok := s.store.(personMatchConsentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Identity scoring store is unavailable")
	}
	return st, ok
}

func (s *Server) personMatchStatus(ctx context.Context, st personMatchConsentStore) (PersonMatchScoringStatus, error) {
	status := PersonMatchScoringStatus{LiveAutomaticAcceptanceAvailable: false, LiveBlocker: "automatic_match_evaluation_required"}
	if s.cfg == nil {
		status.Blocker = "scoring_disabled"
		return status, nil
	}
	cfg := s.cfg.People.IdentityMerge
	cfg.ApplyDefaults()
	status.Enabled = cfg.Enabled
	status.ModelID = cfg.ModelID
	status.MinimumProbability = cfg.MinimumProbability
	status.BatchSize = cfg.BatchSize
	if !cfg.Enabled {
		status.Blocker = "scoring_disabled"
		return status, nil
	}
	disclosure, disclosureErr := cfg.Disclosure()
	if disclosureErr != nil {
		status.Blocker = "invalid_config"
		return status, nil //nolint:nilerr // Status reports invalid optional configuration as a blocker.
	}
	status.Disclosure = &disclosure
	fingerprint, err := disclosure.Fingerprint()
	if err != nil {
		return status, err
	}
	status.DisclosureFingerprint = fingerprint
	_, credentialErr := cfg.CredentialFromEnvironment()
	status.CredentialAvailable = credentialErr == nil
	status.ConsentActive, err = st.HasPersonMatchConsentContext(ctx, status.DisclosureFingerprint)
	if err != nil {
		return status, err
	}
	switch {
	case !status.CredentialAvailable:
		status.Blocker = "credential_unavailable"
	case !status.ConsentActive:
		status.Blocker = "consent_required"
	default:
		status.DryRunReady = true
	}
	return status, nil
}

func (s *Server) handlePersonMatchScoringStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.personMatchScoringStore(w)
	if !ok {
		return
	}
	status, err := s.personMatchStatus(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "scoring_status_unavailable", "Identity scoring status is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handlePersonMatchScoringConsent(w http.ResponseWriter, r *http.Request) {
	s.handlePersonMatchScoringConsentDecision(w, r, true)
}
func (s *Server) handlePersonMatchScoringRevoke(w http.ResponseWriter, r *http.Request) {
	s.handlePersonMatchScoringConsentDecision(w, r, false)
}

func (s *Server) handlePersonMatchScoringConsentDecision(w http.ResponseWriter, r *http.Request, grant bool) {
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.personMatchScoringStore(w)
	if !ok {
		return
	}
	var body PersonMatchConsentDecisionRequest
	if !decodeIdentityMatchJSON(w, r, &body) {
		return
	}
	var changed bool
	var err error
	if grant {
		status, statusErr := s.personMatchStatus(r.Context(), st)
		if statusErr != nil {
			writeError(w, http.StatusServiceUnavailable, "scoring_status_unavailable", "Identity scoring status is unavailable")
			return
		}
		if !status.Enabled || status.Disclosure == nil {
			writeError(w, http.StatusConflict, "scoring_disabled", "Enable valid identity scoring configuration before recording consent")
			return
		}
		if body.DisclosureFingerprint == "" || body.DisclosureFingerprint != status.DisclosureFingerprint {
			writeError(w, http.StatusConflict, "disclosure_changed", "Read the current disclosure fingerprint before deciding")
			return
		}
		_, changed, err = st.GrantPersonMatchConsentContext(r.Context(), *status.Disclosure, apiPersonMatchConsentActor)
	} else {
		if !validPersonMatchDisclosureFingerprint(body.DisclosureFingerprint) {
			writeError(w, http.StatusBadRequest, "invalid_disclosure_fingerprint", "A valid disclosure fingerprint is required")
			return
		}
		changed, err = st.RevokePersonMatchConsentContext(r.Context(), body.DisclosureFingerprint, apiPersonMatchConsentActor)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "consent_unavailable", "Identity scoring consent could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, PersonMatchConsentDecisionResponse{
		DisclosureFingerprint: body.DisclosureFingerprint, ConsentActive: grant, Changed: changed,
	})
}

func validPersonMatchDisclosureFingerprint(fingerprint string) bool {
	if len(fingerprint) != 64 {
		return false
	}
	_, err := hex.DecodeString(fingerprint)
	return err == nil && strings.ToLower(fingerprint) == fingerprint
}

func (s *Server) personMatchRunStore(w http.ResponseWriter) (personMatchRunStore, bool) {
	st, ok := s.store.(personMatchRunStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Identity scoring journal is unavailable")
	}
	return st, ok
}

func (s *Server) handlePersonMatchScoringRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body personMatchScoringRunRequest
	if !decodeIdentityMatchJSON(w, r, &body) {
		return
	}
	if body.DryRun == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "dry_run is required")
		return
	}
	if !*body.DryRun {
		writeError(w, http.StatusConflict, "automatic_match_evaluation_required", "Live automatic acceptance has not passed its evaluation gate")
		return
	}
	st, ok := s.personMatchRunStore(w)
	if !ok {
		return
	}
	status, err := s.personMatchStatus(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "scoring_status_unavailable", "Identity scoring status is unavailable")
		return
	}
	if !status.DryRunReady {
		writeError(w, http.StatusConflict, status.Blocker, "Identity scoring is not ready for a dry run")
		return
	}
	if body.Limit == 0 {
		body.Limit = s.cfg.People.IdentityMerge.BatchSize
	}
	if body.Limit < 1 {
		writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be at least 1")
		return
	}
	if body.Limit > s.cfg.People.IdentityMerge.BatchSize {
		writeError(w, http.StatusBadRequest, "invalid_limit", "Limit exceeds configured batch size")
		return
	}
	worker := personmatchworker.Worker{Store: st, Config: s.cfg.People.IdentityMerge, Endpoint: s.personMatchScoringEndpoint}
	results, err := worker.RunDry(r.Context(), body.Limit)
	if err != nil {
		writePersonMatchScoringRunError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PersonMatchDryRunResponse{Results: results, Processed: len(results)})
}

func writePersonMatchScoringRunError(w http.ResponseWriter, err error) {
	if errors.Is(err, personmatchworker.ErrConsentRequired) {
		writeError(w, http.StatusConflict, "consent_required", "Identity scoring consent is required")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "scoring_run_failed", "Identity scoring dry run failed")
}

func (s *Server) handlePersonMatchScoringHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.personMatchRunStore(w)
	if !ok {
		return
	}
	candidateID, _, err := queryInt(r, "candidate_id")
	if err != nil || candidateID < 0 {
		writeError(w, http.StatusBadRequest, "invalid_candidate_id", "Candidate ID must be nonnegative")
		return
	}
	limit, present, err := queryInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be an integer")
		return
	}
	if !present {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be 1–100")
		return
	}
	beforeID, _, err := queryInt(r, "before_id")
	if err != nil || beforeID < 0 {
		writeError(w, http.StatusBadRequest, "invalid_before_id", "Before ID must be nonnegative")
		return
	}
	judgments, err := st.ListIdentityMatchJudgmentsContext(r.Context(), int64(candidateID), limit, int64(beforeID))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "history_unavailable", "Identity scoring history is unavailable")
		return
	}
	response := PersonMatchJudgmentHistoryResponse{Judgments: judgments, Limit: limit, CandidateID: int64(candidateID)}
	if len(judgments) == limit {
		response.NextBeforeID = &judgments[len(judgments)-1].ID
	}
	writeJSON(w, http.StatusOK, response)
}
