package api

import (
	"context"
	"encoding/json/v2"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const localEmbeddingsEndpointPath = "/api/v1/embeddings/local"

// LocalEmbeddingsController uses the daemon's existing vector runtime and CLI builder.
type LocalEmbeddingsController interface {
	Inspect(ctx context.Context) (LocalEmbeddingsStatus, error)
	Enable(ctx context.Context, builtin bool, report func(LocalEmbeddingsStatus)) error
}

type LocalEmbeddingsStatus struct {
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
	addErrorResponses(api, post.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, post, s.handleEnableLocalEmbeddings)
}

func (s *Server) handleGetLocalEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	controller, progress, running := s.localEmbeddings, s.localSetup, s.localSetupRunning
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
	status.Error = progress.Error
	status.Running = running
	if running {
		status.Phase, status.DoneBytes, status.TotalBytes = progress.Phase, progress.DoneBytes, progress.TotalBytes
	}
	writeJSON(w, http.StatusOK, status)
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
	if s.localSetupRunning {
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
	s.localSetupRunning = true
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
		s.localSetupRunning = false
		s.localSetup.Running = false
		s.localSetup.Phase = "idle"
		if err != nil {
			s.localSetup.Error = err.Error()
		}
		s.importMu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, status)
}
