package archive

import (
	"log/slog"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// Store provides msgvault's archive storage operations.
type Store = store.Store

// Config is the existing msgvault configuration, supplied by the caller.
type Config = config.Config

// ServerOptions configures the complete msgvault API and its services.
type ServerOptions = api.ServerOptions

// Server exposes the API, handler, and shutdown lifecycle.
type Server = api.Server

// Operation identifies a registered API operation for caller access policy.
type Operation = api.Operation

// QueryEngine provides archive queries and aggregates.
type QueryEngine = query.Engine

// NewServer constructs the API without starting a listener. It registers the
// same operations as the daemon; configured services determine availability.
// Use Handler for caller-owned HTTP policy or Router for daemon middleware.
// Call Shutdown and drain requests before closing the underlying archive.
func NewServer(options ServerOptions) *Server {
	if options.Config == nil {
		options.Config = &Config{}
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return api.NewServerWithOptions(options)
}
