package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type personAppreciationStore interface {
	ProjectPersonAppreciationsContext(context.Context, int64, peoplesweep.ProviderProfile) (store.PersonAppreciationProjection, error)
	DismissPersonAppreciationContext(context.Context, int64, string, string, time.Time, peoplesweep.ProviderProfile) (store.PersonAppreciationState, error)
	GetPersonAppreciationStatesContext(context.Context, []string) (map[string]store.PersonAppreciationState, error)
}

// PersonAppreciation exposes only the selected peer and owner citations, not
// the other owner messages considered during generation.
type PersonAppreciation struct {
	Suggestion peoplesweep.BriefThanksSuggestion `json:"suggestion"`
	Peer       peoplesweep.EvidenceRef           `json:"peer"`
	Owner      *peoplesweep.EvidenceRef          `json:"owner,omitempty"`
}

type PersonAppreciationsResponse struct {
	Appreciations []PersonAppreciation `json:"appreciations"`
}

func (s *Server) registerPersonAppreciationRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonAppreciations", http.MethodGet, "/people/{id}/appreciations", "List supported appreciation suggestions")
	addPersonIDParameter(&list)
	list.Responses = jsonResponsesFor[PersonAppreciationsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonAppreciations)

	dismiss := rawAPIV1Operation("dismissPersonAppreciation", http.MethodPost, "/people/{id}/appreciations/{key}/dismiss", "Dismiss an appreciation suggestion")
	addPersonIDParameter(&dismiss)
	dismiss.Parameters = append(dismiss.Parameters, &huma.Param{Name: "key", In: pathKey, Required: true,
		Description: "Stable contribution key", Schema: &huma.Schema{Type: huma.TypeString}})
	dismiss.Responses = jsonResponsesFor[store.PersonAppreciationState](api)
	addErrorResponses(api, dismiss.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, dismiss, s.handleDismissPersonAppreciation)
}

func (s *Server) appreciationProfile(w http.ResponseWriter) (peoplesweep.ProviderProfile, bool) {
	profile, err := s.cfg.People.Sweep.Profile()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "appreciations_unavailable", "Current people inference policy is unavailable")
		return peoplesweep.ProviderProfile{}, false
	}
	return profile, true
}

func (s *Server) loadPersonAppreciations(w http.ResponseWriter, r *http.Request, personID int64) (store.PersonAppreciationProjection, bool) {
	archive, ok := s.store.(personAppreciationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "appreciations_unavailable", "Person appreciations are unavailable")
		return store.PersonAppreciationProjection{}, false
	}
	profile, ok := s.appreciationProfile(w)
	if !ok {
		return store.PersonAppreciationProjection{}, false
	}
	projection, err := archive.ProjectPersonAppreciationsContext(r.Context(), personID, profile)
	if err != nil {
		s.writePersonBriefError(w, err)
		return store.PersonAppreciationProjection{}, false
	}
	return projection, true
}

func (s *Server) handleListPersonAppreciations(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	projection, ok := s.loadPersonAppreciations(w, r, personID)
	if !ok {
		return
	}
	response := PersonAppreciationsResponse{Appreciations: []PersonAppreciation{}}
	for _, memory := range projection.Appreciations {
		suggestion := memory.Suggestion
		suggestion.Text = textutil.SanitizeTerminal(suggestion.Text)
		suggestion.Quote = textutil.SanitizeTerminal(suggestion.Quote)
		if suggestion.Thanked != nil {
			thanks := *suggestion.Thanked
			thanks.Quote = textutil.SanitizeTerminal(thanks.Quote)
			suggestion.Thanked = &thanks
		}
		response.Appreciations = append(response.Appreciations, PersonAppreciation{Suggestion: suggestion, Peer: memory.PeerRef, Owner: memory.OwnerRef})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDismissPersonAppreciation(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	archive, ok := s.store.(personAppreciationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "appreciations_unavailable", "Person appreciations are unavailable")
		return
	}
	profile, ok := s.appreciationProfile(w)
	if !ok {
		return
	}
	state, err := archive.DismissPersonAppreciationContext(r.Context(), personID, r.PathValue("key"), personBriefActor, s.clockNow().UTC(), profile)
	if errors.Is(err, store.ErrInvalidPersonAppreciationKey) {
		writeError(w, http.StatusBadRequest, "invalid_appreciation_key", "Invalid appreciation key")
		return
	}
	if errors.Is(err, store.ErrPersonAppreciationNotFound) || errors.Is(err, store.ErrPersonBriefNotFound) {
		writeError(w, http.StatusNotFound, "appreciation_not_found", "Eligible appreciation not found")
		return
	}
	if err != nil {
		s.writePersonBriefError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) projectCurrentBrief(w http.ResponseWriter, response *PersonBrief, projection store.PersonAppreciationProjection) bool {
	var output peoplesweep.BriefOutput
	if err := json.Unmarshal(projection.Brief.Structured, &output); err != nil {
		writeError(w, http.StatusServiceUnavailable, "brief_projection_failed", "Person brief projection is unavailable")
		return false
	}
	order := peoplesweep.BriefCitationOrder(output)
	if len(order) != len(response.Evidence) {
		writeError(w, http.StatusServiceUnavailable, "brief_projection_failed", "Person brief citations are unavailable")
		return false
	}
	inputs := make([]personfacts.EvidenceInput, len(order))
	pointers := make(map[string]store.PersonBriefEvidencePointer, len(order))
	for i, id := range order {
		inputs[i].SourceRef = id
		pointers[id] = response.Evidence[i]
	}
	current := make(map[string]peoplesweep.BriefAppreciationMemory)
	for _, memory := range projection.Appreciations {
		pointer, ok := pointers[memory.Suggestion.AnchorID]
		if ok && pointer.Supported {
			current[memory.Suggestion.Key] = memory
		}
	}
	rendered := peoplesweep.RenderedBrief{Text: projection.Brief.RenderedText}
	for _, sentence := range response.Sentences {
		rendered.Sentences = append(rendered.Sentences, peoplesweep.RenderedSentence{Kind: peoplesweep.BriefSentenceKind(sentence.Kind), Index: sentence.Index, Text: sentence.Text})
	}
	result := peoplesweep.BriefResult{Structured: projection.Brief.Structured, Evidence: inputs, Rendered: rendered}
	if err := peoplesweep.FilterBriefAppreciations(&result, current); err != nil {
		writeError(w, http.StatusServiceUnavailable, "brief_projection_failed", "Person brief projection is unavailable")
		return false
	}
	projected := projection.Brief
	projected.Structured, projected.RenderedText = result.Structured, result.Rendered.Text
	response.Structured = s.sanitizedPersonBriefStructure(projected)
	response.RenderedText = textutil.SanitizeTerminal(result.Rendered.Text)
	response.Evidence = make([]store.PersonBriefEvidencePointer, 0, len(result.Evidence))
	for _, input := range result.Evidence {
		pointer := pointers[input.SourceRef]
		pointer.Ordinal = len(response.Evidence)
		response.Evidence = append(response.Evidence, pointer)
	}
	response.Sentences = s.personBriefSentences(projected, len(response.Evidence))
	for i := range response.Sentences {
		response.Sentences[i].Text = textutil.SanitizeTerminal(response.Sentences[i].Text)
	}
	return true
}

func (s *Server) addBriefHistoryOverlay(w http.ResponseWriter, r *http.Request, response *PersonBrief, brief store.PersonBrief) bool {
	response.HistoricalSnapshot = true
	if brief.ProgramVersion != "v3" {
		return true
	}
	var boundary peoplesweep.BriefBoundary
	if err := json.Unmarshal(brief.Boundary, &boundary); err != nil || len(boundary.AppreciationMemory) > 3 {
		writeError(w, http.StatusServiceUnavailable, "brief_projection_failed", "Person brief history is unavailable")
		return false
	}
	keys := make([]string, 0, len(boundary.AppreciationMemory))
	for _, memory := range boundary.AppreciationMemory {
		keys = append(keys, memory.Suggestion.Key)
	}
	archive, ok := s.store.(personAppreciationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "appreciations_unavailable", "Person appreciations are unavailable")
		return false
	}
	states, err := archive.GetPersonAppreciationStatesContext(r.Context(), keys)
	if err != nil {
		s.writePersonBriefError(w, err)
		return false
	}
	response.AppreciationDismissalKeys = []string{}
	for _, key := range keys {
		if state, ok := states[key]; ok && state.DismissedAt != nil {
			response.AppreciationDismissalKeys = append(response.AppreciationDismissalKeys, key)
		}
	}
	return true
}
