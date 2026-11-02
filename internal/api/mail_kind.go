package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/store"
)

// MailKindStatus describes mail-specific egress readiness without any secret.
type MailKindStatus struct {
	Enabled               bool                 `json:"enabled"`
	Ready                 bool                 `json:"ready"`
	CredentialAvailable   bool                 `json:"credential_available"`
	ConsentActive         bool                 `json:"consent_active"`
	BatchSize             int                  `json:"batch_size"`
	DataFields            string               `json:"data_fields"`
	Disclosure            *mailkind.Disclosure `json:"disclosure,omitzero" nullable:"false"`
	DisclosureFingerprint string               `json:"disclosure_fingerprint,omitempty"`
	Blocker               string               `json:"blocker,omitempty"`
}

type MailKindConsentRequest struct {
	DisclosureFingerprint string `json:"disclosure_fingerprint"`
}

type MailKindConsentResponse struct {
	DisclosureFingerprint string `json:"disclosure_fingerprint"`
	ConsentActive         bool   `json:"consent_active"`
	Changed               bool   `json:"changed"`
}

type mailKindConsentStore interface {
	GrantMailKindConsentContext(ctx context.Context, disclosure mailkind.Disclosure, actor string, beginMutation func(context.Context) (func(), error)) (*store.ProviderConsent, bool, error)
	RevokeMailKindConsentContext(ctx context.Context, fingerprint string, actor string, beginMutation func(context.Context) (func(), error)) (bool, error)
	HasMailKindConsentContext(ctx context.Context, fingerprint string) (bool, error)
}

func (s *Server) registerMailKindRoutes(api huma.API) {
	status := rawAPIV1Operation("getMailKindStatus", http.MethodGet, "/mail/kind/status", "Get optional mail kind configuration and exact disclosure")
	status.Responses = jsonResponsesFor[MailKindStatus](api)
	addErrorResponses(api, status.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, status, s.handleMailKindStatus)
	for _, action := range []string{"consent", "revoke"} {
		op := rawAPIV1Operation("mailKind"+strings.ToUpper(action[:1])+action[1:], http.MethodPost,
			"/mail/kind/"+action, "Record mail kind "+action+" for an exact disclosure")
		op.RequestBody = jsonRequestBodyFor[MailKindConsentRequest](api)
		op.RequestBody.Required = true
		op.Responses = jsonResponsesFor[MailKindConsentResponse](api)
		addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
		if action == "consent" {
			registerRawHumaRoute(api, op, s.handleMailKindConsent)
		} else {
			registerRawHumaRoute(api, op, s.handleMailKindRevoke)
		}
	}
}

func (s *Server) mailKindStatus(ctx context.Context, st mailKindConsentStore) (MailKindStatus, error) {
	status := MailKindStatus{DataFields: mailkind.DataFields}
	if s.cfg == nil {
		status.Blocker = "classification_disabled"
		return status, nil
	}
	cfg := s.cfg.Mail.KindClassification
	cfg.ApplyDefaults()
	status.Enabled, status.BatchSize = cfg.Enabled, cfg.BatchSize
	if !cfg.Enabled {
		status.Blocker = "classification_disabled"
		return status, nil
	}
	disclosure, err := cfg.Disclosure()
	if err != nil {
		status.Blocker = "invalid_config"
		return status, nil //nolint:nilerr // Optional configuration errors are reported as a blocker.
	}
	status.Disclosure = &disclosure
	status.DisclosureFingerprint, err = disclosure.Fingerprint()
	if err != nil {
		return status, err
	}
	_, credentialErr := cfg.CredentialFromEnvironment()
	status.CredentialAvailable = credentialErr == nil
	status.ConsentActive, err = st.HasMailKindConsentContext(ctx, status.DisclosureFingerprint)
	if err != nil {
		return status, err
	}
	switch {
	case !status.CredentialAvailable:
		status.Blocker = "credential_unavailable"
	case !status.ConsentActive:
		status.Blocker = "consent_required"
	default:
		status.Ready = true
	}
	return status, nil
}

func (s *Server) handleMailKindStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.store.(mailKindConsentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Mail kind consent store is unavailable")
		return
	}
	status, err := s.mailKindStatus(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "status_unavailable", "Mail kind status is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleMailKindConsent(w http.ResponseWriter, r *http.Request) {
	s.handleMailKindConsentDecision(w, r, true)
}

func (s *Server) handleMailKindRevoke(w http.ResponseWriter, r *http.Request) {
	s.handleMailKindConsentDecision(w, r, false)
}

func (s *Server) handleMailKindConsentDecision(w http.ResponseWriter, r *http.Request, grant bool) {
	w.Header().Set("Cache-Control", "no-store")
	st, ok := s.store.(mailKindConsentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Mail kind consent store is unavailable")
		return
	}
	var body MailKindConsentRequest
	if !decodeIdentityMatchJSON(w, r, &body) {
		return
	}
	if !validPersonMatchDisclosureFingerprint(body.DisclosureFingerprint) {
		writeError(w, http.StatusBadRequest, "invalid_disclosure_fingerprint", "An exact disclosure fingerprint is required")
		return
	}
	var changed bool
	var err error
	if grant {
		status, statusErr := s.mailKindStatus(r.Context(), st)
		if statusErr != nil {
			writeError(w, http.StatusServiceUnavailable, "status_unavailable", "Mail kind status is unavailable")
			return
		}
		if status.Disclosure == nil || body.DisclosureFingerprint != status.DisclosureFingerprint {
			writeError(w, http.StatusConflict, "disclosure_changed", "Read the current enabled disclosure before deciding")
			return
		}
		_, changed, err = st.GrantMailKindConsentContext(r.Context(), *status.Disclosure, "local_operator", s.beginMailKindMutation)
	} else {
		changed, err = st.RevokeMailKindConsentContext(r.Context(), body.DisclosureFingerprint, "local_operator", s.beginMailKindMutation)
	}
	if err != nil {
		if errors.Is(err, errMailKindGateBusy) {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "consent_unavailable", "Mail kind consent could not be recorded")
		return
	}
	writeJSON(w, http.StatusOK, MailKindConsentResponse{DisclosureFingerprint: body.DisclosureFingerprint, ConsentActive: grant, Changed: changed})
}

var errMailKindGateBusy = errors.New("archive is busy or shutting down")

func (s *Server) beginMailKindMutation(ctx context.Context) (func(), error) {
	if s.operationGate == nil {
		return func() {}, nil
	}
	done, ok := beginGateWorkBounded(ctx, s.operationGate, "mail kind policy")
	if !ok {
		return nil, errMailKindGateBusy
	}
	return done, nil
}
