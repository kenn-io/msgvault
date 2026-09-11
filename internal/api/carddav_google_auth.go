package api

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/oauth"
)

type CardDAVGoogleAuthorizeRequest struct {
	Email       string `json:"email"`
	OAuthApp    string `json:"oauth_app,omitempty"`
	RedirectURI string `json:"redirect_uri"`
}

type CardDAVGoogleAuthorizeResponse struct {
	URL   string `json:"url"`
	State string `json:"state"`
}

type CardDAVGoogleCallbackRequest struct {
	State string `json:"state"`
	Code  string `json:"code" writeOnly:"true"`
}

type cardDAVGoogleAuthorization struct {
	flow    *oauth.WebAuthorization
	expires time.Time
}

func (s *Server) handleGoogleCardDAVAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil || s.cardDAV.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV settings are unavailable")
		return
	}
	var req CardDAVGoogleAuthorizeRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	address, err := mail.ParseAddress(req.Email)
	redirect, redirectErr := url.Parse(req.RedirectURI)
	if err != nil || address.Address != req.Email || redirectErr != nil ||
		redirect.Scheme+"://"+redirect.Host != r.Header.Get("Origin") || redirect.Path != "/" || redirect.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Provide an account email and this Web UI's root URL as the OAuth callback")
		return
	}
	secrets, err := s.cardDAV.cfg.OAuth.ClientSecretsFor(req.OAuthApp)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth_not_configured", "Configure the selected Google OAuth app's client_secrets before connecting")
		return
	}
	mgr, err := carddav.NewGoogleOAuthManager(secrets, s.cardDAV.cfg.TokensDir(), req.OAuthApp, req.Email, s.logger)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth_not_configured", "Unable to load the selected Google OAuth app")
		return
	}
	flow, err := mgr.BeginWebAuthorization(req.Email, req.RedirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth_configuration", err.Error())
		return
	}
	c := s.cardDAV
	c.googleAuthMu.Lock()
	defer c.googleAuthMu.Unlock()
	if c.googleAuthorizations == nil {
		c.googleAuthorizations = make(map[string]cardDAVGoogleAuthorization)
	}
	for state, pending := range c.googleAuthorizations {
		if time.Now().After(pending.expires) {
			delete(c.googleAuthorizations, state)
		}
	}
	if len(c.googleAuthorizations) >= 16 {
		writeError(w, http.StatusServiceUnavailable, "oauth_busy", "Too many pending sign-ins. Wait ten minutes and try again")
		return
	}
	c.googleAuthorizations[flow.State] = cardDAVGoogleAuthorization{flow: flow, expires: time.Now().Add(10 * time.Minute)}
	writeJSON(w, http.StatusOK, CardDAVGoogleAuthorizeResponse{URL: flow.URL, State: flow.State})
}

func (c *CardDAVController) takeGoogleAuthorization(state string) (*oauth.WebAuthorization, error) {
	c.googleAuthMu.Lock()
	defer c.googleAuthMu.Unlock()
	pending, ok := c.googleAuthorizations[state]
	delete(c.googleAuthorizations, state)
	if !ok || time.Now().After(pending.expires) {
		return nil, errors.New("sign-in for Google Contacts expired or was already used; connect again")
	}
	return pending.flow, nil
}

func (s *Server) handleGoogleCardDAVCallback(w http.ResponseWriter, r *http.Request) {
	if s.cardDAV == nil {
		writeError(w, http.StatusServiceUnavailable, "carddav_unavailable", "CardDAV settings are unavailable")
		return
	}
	var req CardDAVGoogleCallbackRequest
	if !decodeCardDAV(w, r, &req) {
		return
	}
	flow, err := s.cardDAV.takeGoogleAuthorization(req.State)
	if err != nil {
		writeError(w, http.StatusBadRequest, "oauth_expired", err.Error())
		return
	}
	if err := flow.Complete(r.Context(), req.State, req.Code); err != nil {
		if unavailable, ok := errors.AsType[*oauth.AuthorizationUnavailableError](err); ok {
			if unavailable.RetryAfter > 0 {
				seconds := max(int64(1), int64((unavailable.RetryAfter+time.Second-1)/time.Second))
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			}
			writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "Google authorization is temporarily unavailable. Start sign-in again to retry")
			return
		}
		if errors.Is(err, oauth.ErrTokenChanged) {
			writeError(w, http.StatusBadRequest, "oauth_changed", oauth.ErrTokenChanged.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "oauth_failed", "Google authorization failed. Select the requested account and grant all requested permissions, then try again")
		return
	}
	if err := s.cardDAV.ReconcileSchedule(); err != nil {
		s.logger.Error("reconcile CardDAV schedule after Google authorization", "error", err)
		writeError(w, http.StatusServiceUnavailable, "carddav_schedule_failed", "Google Contacts authorized, but scheduling failed. Save the CardDAV account to retry")
		return
	}
	writeJSON(w, http.StatusOK, StatusMessageResponse{Status: "ok", Message: "Google Contacts authorized"})
}
