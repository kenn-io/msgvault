package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/vector"
)

const localEmbeddingsEndpointPath = "/api/v1/embeddings/local"

// LocalEmbeddingsController uses the daemon's existing vector runtime and CLI builder.
type LocalEmbeddingsController interface {
	Inspect(ctx context.Context) (LocalEmbeddingsStatus, error)
	Enable(ctx context.Context, report func(LocalEmbeddingsStatus)) error
}

type LocalEmbeddingsStatus struct {
	Host          string `json:"host"`
	Enabled       bool   `json:"enabled"`
	Provider      string `json:"provider"`
	DownloadBytes int64  `json:"download_bytes"`
	Phase         string `json:"phase" enum:"idle,downloading,indexing"`
	DoneBytes     int64  `json:"done_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Error         string `json:"error,omitempty"`
	Running       bool   `json:"running,omitempty"`
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
	post.Responses = jsonResponsesFor[LocalEmbeddingsStatus](api, http.StatusAccepted)
	addErrorResponses(api, post.Responses, http.StatusUnauthorized, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, post, s.handleEnableLocalEmbeddings)
}

func (s *Server) handleGetLocalEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	controller, progress := s.localEmbeddings, s.localSetup
	running := progress.Running
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

	progress.Error = s.localSetupError(r.Context())
	if running || progress.Error != "" {
		status.Error = progress.Error
	}
	status.Running = running
	if running {
		status.Phase, status.DoneBytes, status.TotalBytes = progress.Phase, progress.DoneBytes, progress.TotalBytes
	}
	if isLoopbackAuthority(r.Host, 0) {
		status.Host = ""
	} else {
		status.Host = (&url.URL{Host: r.Host}).Hostname()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) localSetupError(ctx context.Context) string {
	s.importMu.Lock()
	progress := s.localSetup
	s.importMu.Unlock()
	if !progress.Running {
		_, backend, cfg := s.vectorComponents()
		if backend != nil && cfg.Enabled {
			if coverage, _, err := s.loadEmbeddingStatus(ctx, 0); err == nil {
				if progress.Error == "" {
					if coverage.Pending > 0 && coverage.ActiveRunID == 0 {
						return coverage.LatestRunError
					}
					return ""
				}
				if coverage.Pending == 0 {
					if _, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.GenerationFingerprint()); err == nil {
						s.importMu.Lock()
						if s.localSetup == progress {
							s.localSetup.Error = ""
						}
						progress = s.localSetup
						s.importMu.Unlock()
					}
				}
			}
		}
	}
	return progress.Error
}

func (s *Server) handleEnableLocalEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.importMu.Lock()
	if s.localEmbeddings == nil || s.importsClosed {
		s.importMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Local search setup is unavailable")
		return
	}
	if s.localSetup.Running {
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
		err := controller.Enable(s.importContext, report)
		s.importMu.Lock()
		s.localSetup.Running = false
		s.localSetup.Phase = "idle"
		if err != nil {
			s.localSetup.Error = err.Error()
		}
		s.importMu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, status)
}
