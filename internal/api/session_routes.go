package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

const (
	sessionPath       = "/api/session"
	sessionLoginPath  = "/api/session/login"
	sessionCookieName = "msgvault_session"
)

// AuthMode describes why the current request may access protected API routes.
type AuthMode string

const (
	AuthModeLoopback AuthMode = "loopback"
	AuthModeAPIKey   AuthMode = "api_key"
	AuthModeSession  AuthMode = "session"
	AuthModeRequired AuthMode = "required"
)

// SessionLoginRequest exchanges the active daemon API key for an in-memory
// browser session.
type SessionLoginRequest struct {
	APIKey string `json:"api_key"`
}

// SessionStatus reports the request's effective authentication mode. The CSRF
// token is returned only for a valid browser session so mutation middleware
// can enforce session-bound requests without exposing it to other auth modes.
type SessionStatus struct {
	AuthMode         AuthMode `json:"auth_mode" enum:"loopback,api_key,session,required"`
	CSRFToken        string   `json:"csrf_token,omitempty"`
	HTTPS            bool     `json:"https"`
	PlainHTTPWarning bool     `json:"plain_http_warning"`
}

func (s *Server) registerSessionRoutes(api huma.API) {
	login := huma.Operation{
		OperationID: "loginSession",
		Method:      http.MethodPost,
		Path:        sessionLoginPath,
		Tags:        []string{"Session"},
		Summary:     "Create an in-memory browser session",
		Errors:      []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError},
		RequestBody: jsonRequestBodyFor[SessionLoginRequest](api),
		Responses:   jsonResponsesFor[SessionStatus](api),
	}
	registerRawHumaRoute(api, login, s.handleSessionLogin)

	bootstrap := huma.Operation{
		OperationID: "getSession",
		Method:      http.MethodGet,
		Path:        sessionPath,
		Tags:        []string{"Session"},
		Summary:     "Get browser authentication status",
		Responses:   jsonResponsesFor[SessionStatus](api),
	}
	registerRawHumaRoute(api, bootstrap, s.handleSessionBootstrap)

	logout := huma.Operation{
		OperationID: "logoutSession",
		Method:      http.MethodDelete,
		Path:        sessionPath,
		Tags:        []string{"Session"},
		Summary:     "Delete the current browser session",
		Errors:      []int{http.StatusTooManyRequests},
		Responses: map[string]*huma.Response{
			httpStatusKey(http.StatusNoContent):       {Description: http.StatusText(http.StatusNoContent)},
			httpStatusKey(http.StatusTooManyRequests): errorResponseFor(api),
			"default": errorResponseFor(api),
		},
	}
	registerRawHumaRoute(api, logout, s.handleSessionLogout)
}

func (s *Server) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	var input SessionLoginRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid session login request")
		return
	}
	if s.cfg.Server.APIKey == "" || !constantTimeAPIKeyEqual(input.APIKey, s.cfg.Server.APIKey) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid API key")
		return
	}

	id, session, err := s.sessions.create()
	if err != nil {
		s.logger.Error("create browser session", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not create browser session")
		return
	}
	https := requestUsesHTTPS(r)
	// Secure follows the verified connection scheme; plain HTTP support is an
	// explicit deployment mode surfaced by PlainHTTPWarning.
	//nolint:gosec // Host-only, HttpOnly, and Strict are fixed below; Secure is connection-dependent.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  session.ExpiresAt,
		MaxAge:   max(1, int(s.sessions.ttl/time.Second)),
		HttpOnly: true,
		Secure:   https,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, sessionStatus(AuthModeSession, session.CSRFToken, https))
}

func (s *Server) handleSessionBootstrap(w http.ResponseWriter, r *http.Request) {
	auth := s.requestAuthentication(r)
	csrfToken := ""
	if auth.Mode == AuthModeSession {
		csrfToken = auth.Session.CSRFToken
	}
	writeJSON(w, http.StatusOK, sessionStatus(auth.Mode, csrfToken, requestUsesHTTPS(r)))
}

func (s *Server) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.delete(cookie.Value)
	}
	//nolint:gosec // Expiration mirrors the connection-dependent flags of the session cookie being cleared.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func sessionStatus(mode AuthMode, csrfToken string, https bool) SessionStatus {
	return SessionStatus{
		AuthMode:         mode,
		CSRFToken:        csrfToken,
		HTTPS:            https,
		PlainHTTPWarning: !https,
	}
}

func requestUsesHTTPS(r *http.Request) bool {
	if security, ok := securityFromRequest(r); ok {
		return security.scheme == schemeHTTPS
	}
	return r.TLS != nil
}
