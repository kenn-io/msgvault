package api

import (
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
// serving; vector endpoints return 503 carrying the message.
func (s *Server) SetVectorInitError(err error) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusError
	if err != nil {
		s.vectorErr = err.Error()
	}
}

// VectorStatus returns the vector subsystem status and, when the status is
// VectorStatusError, the failure message.
func (s *Server) VectorStatus() (VectorStatus, string) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorStatus, s.vectorErr
}

//nolint:unparam // hybridEngine is used by vector handlers in later tasks
func (s *Server) vectorComponents() (*hybrid.Engine, vector.Backend, vector.Config) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.hybridEngine, s.backend, s.vectorCfg
}
