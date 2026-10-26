package api

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

type nativeMCPHandlers struct {
	sync.RWMutex

	read, write http.Handler
	close       func()
	closed      bool
}

func isNativeMCPPath(path string) bool { return path == "/mcp" || path == "/mcp/writes" }

// InstallMCPHandlers installs independent catalogs once, after the daemon starts.
// The caller retains cleanup ownership if installation fails.
func (s *Server) InstallMCPHandlers(read, write http.Handler, closeHandlers func()) error {
	s.nativeMCP.Lock()
	defer s.nativeMCP.Unlock()
	if s.nativeMCP.closed {
		return errors.New("native MCP handlers closed")
	}
	s.nativeMCP.read, s.nativeMCP.write, s.nativeMCP.close = read, write, closeHandlers
	return nil
}

func (s *Server) closeNativeMCP() {
	s.nativeMCP.Lock()
	closeHandlers := s.nativeMCP.close
	s.nativeMCP.close = nil
	s.nativeMCP.closed = true
	s.nativeMCP.Unlock()
	if closeHandlers != nil {
		closeHandlers()
	}
}

func (s *Server) mountNativeMCP(mux *http.ServeMux) {
	for index, path := range []string{"/mcp", "/mcp/writes"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			// Require bearer syntax as well as owner API-key mode, including during initialization.
			credential, bearer := apiprotocol.BearerAuthorization(r)
			authorized := false
			if bearer {
				request := r.Clone(context.WithValue(r.Context(), requestSecurityContextKey{}, nil))
				request.Header.Set("Authorization", "Bearer "+credential)
				authorized = s.requestAuthentication(request).Mode == AuthModeAPIKey
			}
			if !authorized {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			s.nativeMCP.RLock()
			handlers, closed := [2]http.Handler{s.nativeMCP.read, s.nativeMCP.write}, s.nativeMCP.closed
			s.nativeMCP.RUnlock()
			if closed || handlers[0] == nil {
				http.Error(w, "MCP unavailable", http.StatusServiceUnavailable)
				return
			}
			if handlers[index] == nil {
				http.NotFound(w, r)
				return
			}
			request := r.Clone(r.Context())
			security, _ := securityFromRequest(r)
			request.Host = security.host
			handlers[index].ServeHTTP(w, request)
		})
	}
}
