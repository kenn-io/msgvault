package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/operations"
)

const localEmbeddingsEndpointPath = "/api/v1/embeddings/local"

// LocalEmbeddingsController uses the daemon's existing vector runtime and CLI builder.
type LocalEmbeddingsController interface {
	Inspect(ctx context.Context) (LocalEmbeddingsStatus, error)
	Enable(ctx context.Context, builtin bool, report func(LocalEmbeddingsStatus)) error
}

type LocalEmbeddingsStatus struct {
	Host            string `json:"host"`
	DocumentRebuild bool   `json:"document_rebuild,omitempty"`
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	Compatible      bool   `json:"compatible"`
	OllamaAvailable bool   `json:"ollama_available"`
	DownloadBytes   int64  `json:"download_bytes"`
	Phase           string `json:"phase" enum:"idle,downloading,indexing"`
	DoneBytes       int64  `json:"done_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	Error           string `json:"error,omitempty"`
	Running         bool   `json:"running,omitempty"`
}

type LocalEmbeddingsRequest struct {
	UseBuiltIn bool `json:"use_builtin"`
}

func (s *Server) SetLocalEmbeddings(controller LocalEmbeddingsController) {
	s.importMu.Lock()
	s.localEmbeddings = controller
	s.importMu.Unlock()
}

func (s *Server) registerLocalEmbeddingsRoutes(api huma.API) {
	get := rawAPIV1Operation("getLocalEmbeddings", http.MethodGet, "/embeddings/local", "Get local search setup")
	get.Responses = jsonResponsesFor[LocalEmbeddingsStatus](api)
	addErrorResponses(api, get.Responses, http.StatusUnauthorized, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetLocalEmbeddings)
	post := rawAPIV1Operation("enableLocalEmbeddings", http.MethodPost, "/embeddings/local", "Turn on local search")
	post.RequestBody = jsonRequestBodyFor[LocalEmbeddingsRequest](api)
	post.Responses = jsonResponsesFor[LocalEmbeddingsStatus](api, http.StatusAccepted)
	addErrorResponses(api, post.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, post, s.handleEnableLocalEmbeddings)
}

func (s *Server) handleGetLocalEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	controller, progress := s.localEmbeddings, s.localSetup
	running := progress.Running
	failedAt := s.localSetupBuildFailedAt
	s.importMu.Unlock()
	if controller == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Local search setup is unavailable")
		return
	}
	status, err := controller.Inspect(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	if !running && progress.Error != "" && !failedAt.IsZero() {
		// A separate Build index action can resolve a failed setup build. Provider
		// installation and publication errors must still remain visible.
		if s.localSetupBuildRecovered(r.Context(), failedAt) {
			s.importMu.Lock()
			if !s.localSetup.Running && s.localSetupBuildFailedAt.Equal(failedAt) {
				s.localSetup.Error = ""
				s.localSetupBuildFailedAt = time.Time{}
				progress.Error = ""
			}
			s.importMu.Unlock()
		}
	}
	status.Error = progress.Error
	status.Running = running
	if running {
		status.Phase, status.DoneBytes, status.TotalBytes = progress.Phase, progress.DoneBytes, progress.TotalBytes
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) localSetupBuildRecovered(ctx context.Context, failedAt time.Time) bool {
	if s.operationHistoryReader == nil {
		return false
	}
	lane, err := s.operationHistoryReader.LaneStatus(ctx, operations.KindMessageEmbedding)
	if err != nil || lane.Latest == nil || lane.Latest.State != operations.StateSucceeded || lane.Latest.FinishedAt == nil || !lane.Latest.FinishedAt.After(failedAt) {
		return false
	}
	index, _, err := s.loadEmbeddingStatus(ctx, 0)
	return err == nil && index.Pending == 0 && index.Failed == 0 && index.ActiveGeneration != nil && index.ActiveGeneration.ID == index.Generation.ID
}

func (s *Server) handleEnableLocalEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req LocalEmbeddingsRequest
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 1024), &req, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid local search request")
		return
	}
	s.importMu.Lock()
	if s.localEmbeddings == nil || s.importsClosed {
		s.importMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Local search setup is unavailable")
		return
	}
	if s.localSetup.Running {
		if s.localSetupUseBuiltin != req.UseBuiltIn {
			s.importMu.Unlock()
			writeError(w, http.StatusConflict, "operation_in_progress", "Local search setup is already running with a different provider choice")
			return
		}
		status := s.localSetup
		s.importMu.Unlock()
		writeJSON(w, http.StatusAccepted, status)
		return
	}
	finishIdle, ok := s.idleTracker.BeginWork()
	if !ok {
		s.importMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "unavailable", "The daemon is stopping")
		return
	}
	controller := s.localEmbeddings
	s.localSetup = LocalEmbeddingsStatus{Phase: "idle", Running: true}
	s.localSetupUseBuiltin = req.UseBuiltIn
	s.localSetupBuildFailedAt = time.Time{}
	status := s.localSetup
	s.importWG.Add(1)
	s.importMu.Unlock()
	go func() {
		defer s.importWG.Done()
		defer finishIdle()
		report := func(status LocalEmbeddingsStatus) {
			s.importMu.Lock()
			status.Running = true
			s.localSetup = status
			s.importMu.Unlock()
		}
		err := controller.Enable(s.importContext, req.UseBuiltIn, report)
		s.importMu.Lock()
		s.localSetup.Running = false
		if err != nil && s.localSetup.Phase == "indexing" {
			s.localSetupBuildFailedAt = time.Now()
		}
		s.localSetup.Phase = "idle"
		if err != nil {
			s.localSetup.Error = err.Error()
		}
		s.importMu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, status)
}
