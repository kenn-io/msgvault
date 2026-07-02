package api

import (
	"net/http"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// VectorStatus describes the daemon's vector-search subsystem state. The
// serve daemon starts with `initializing` and flips to `ready` or `error`
// when the background init finishes; non-daemon servers derive `ready` or
// `disabled` from whether a backend was supplied at construction.
type VectorStatus string

const (
	VectorStatusDisabled     VectorStatus = "disabled"
	VectorStatusInitializing VectorStatus = "initializing"
	VectorStatusReady        VectorStatus = "ready"
	VectorStatusError        VectorStatus = "error"
	// VectorStatusStale means the backend initialized fine, but the active
	// index's fingerprint does not match the configured embedding
	// model/dimension/preprocess policy, so vector search returns
	// index_stale 503s until the index is rebuilt. It is evaluated once at
	// init completion using the same staleness check the query path runs.
	VectorStatusStale VectorStatus = "stale"
)

// SetVectorFeatures installs the vector components into a running server.
// The serve daemon calls this from its background init goroutine once
// migrations and the embed_gen backfill complete.
func (s *Server) SetVectorFeatures(engine *hybrid.Engine, backend vector.Backend, cfg vector.Config) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.hybridEngine = engine
	s.backend = backend
	s.vectorCfg = cfg
	s.vectorStatus = VectorStatusReady
	s.vectorErr = ""
}

// SetVectorInitError marks the vector subsystem as failed. The daemon keeps
// serving; vector endpoints return 503 carrying the message. Calling with a
// nil error is a programmer error — there is nothing to report — and is a
// no-op: it does not flip the status to error or touch any existing state.
func (s *Server) SetVectorInitError(err error) {
	if err == nil {
		return
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusError
	s.vectorErr = err.Error()
}

// SetVectorStale marks the vector subsystem as stale: the backend
// initialized and its components are installed, but the active index does
// not match the configured embedding model/dimension, so vector searches
// return index_stale until the index is rebuilt. detail should name the
// stored vs configured fingerprint and the rebuild command. Calling with an
// empty detail is a no-op — there is nothing actionable to report.
func (s *Server) SetVectorStale(detail string) {
	if detail == "" {
		return
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusStale
	s.vectorErr = detail
}

// VectorStatus returns the vector subsystem status and, when the status is
// VectorStatusError or VectorStatusStale, the associated detail message.
func (s *Server) VectorStatus() (VectorStatus, string) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorStatus, s.vectorErr
}

// vectorHealth returns the health-response view of the vector subsystem,
// or nil when vector search is disabled.
func (s *Server) vectorHealth() *VectorHealth {
	status, errMsg := s.VectorStatus()
	if status == VectorStatusDisabled {
		return nil
	}
	return &VectorHealth{Status: string(status), Error: errMsg}
}

func (s *Server) vectorComponents() (*hybrid.Engine, vector.Backend, vector.Config) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.hybridEngine, s.backend, s.vectorCfg
}

// writeVectorUnavailable reports why vector search cannot serve a request
// right now: still initializing (daemon background migration), failed to
// initialize, or simply not enabled.
func (s *Server) writeVectorUnavailable(w http.ResponseWriter) {
	status, errMsg := s.VectorStatus()
	switch status {
	case VectorStatusInitializing:
		writeError(w, http.StatusServiceUnavailable, "vector_initializing",
			"vector search is initializing (schema migration or backfill in progress); retry shortly")
	case VectorStatusError:
		writeError(w, http.StatusServiceUnavailable, "vector_init_failed",
			"vector search failed to initialize: "+errMsg)
	case VectorStatusStale:
		writeError(w, http.StatusServiceUnavailable, "index_stale", errMsg)
	default:
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
	}
}
