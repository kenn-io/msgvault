package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
)

const settingsProviderCredentialRoute = "/settings/provider-credentials/{credential_id}" // #nosec G101 -- HTTP route, not a credential.

type ProviderCredentialWriteRequest struct {
	Value string `json:"value" minLength:"1"`
}

type ProviderCredentialResponse struct {
	CredentialID   string             `json:"credential_id"`
	State          SecretSettingState `json:"state"`
	PendingRestart bool               `json:"pending_restart"`
}

type providerCredentialBinding struct {
	id          string
	endpoint    string
	environment string
}

func validateSafePublicSettingsEndpoints(cfg *config.Config) error {
	endpoints := []string{
		cfg.Vector.Embeddings.Endpoint,
		cfg.Vector.Multimodal.Endpoint,
	}
	for _, provider := range cfg.People.Enrichment.Providers {
		if !provider.Enabled {
			continue
		}
		endpoints = append(endpoints, provider.Endpoint, provider.PollEndpoint)
	}
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) == "" {
			continue
		}
		if _, err := providercredentials.EndpointOrigin(endpoint); err != nil {
			return fmt.Errorf("unsafe public provider endpoint: %w", err)
		}
	}
	return nil
}

func (s *Server) registerProviderCredentialSettingsRoutes(api huma.API) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		operationID := "putSettingsProviderCredential"
		summary := "Set a write-only provider credential"
		if method == http.MethodDelete {
			operationID = "deleteSettingsProviderCredential"
			summary = "Clear a stored provider credential"
		}
		operation := rawAPIV1Operation(operationID, method, settingsProviderCredentialRoute, summary)
		operation.Parameters = append(operation.Parameters,
			&huma.Param{Name: "credential_id", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
			&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
				Description: "Strong ETag for the provider credential store", Schema: &huma.Schema{Type: huma.TypeString}},
		)
		if method == http.MethodPut {
			operation.RequestBody = jsonRequestBodyFor[ProviderCredentialWriteRequest](api)
		}
		operation.Responses = jsonResponsesFor[ProviderCredentialResponse](api)
		for _, status := range []int{http.StatusBadRequest, http.StatusNotFound,
			http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusUnprocessableEntity} {
			operation.Responses[httpStatusKey(status)] = errorResponseFor(api)
		}
		addSettingsETagHeader(operation.Responses[httpStatusKey(http.StatusOK)])
		if method == http.MethodPut {
			registerRawHumaRoute(api, operation, s.handlePutProviderCredential)
		} else {
			registerRawHumaRoute(api, operation, s.handleDeleteProviderCredential)
		}
	}
}

func (s *Server) handlePutProviderCredential(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	credentialID := strings.TrimSpace(r.PathValue("credential_id"))
	if err := providercredentials.ValidateID(credentialID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential_id", "Provider credential ID is invalid")
		return
	}
	var request ProviderCredentialWriteRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	if request.Value == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Credential value is required")
		return
	}
	_, cfg, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	binding, ok := providerCredentialBindingForID(cfg, credentialID)
	if !ok {
		writeError(w, http.StatusNotFound, "provider_not_found", "Configured provider was not found")
		return
	}
	snapshot, err := providercredentials.Put(cfg.TokensDir(), ifMatch, credentialID, binding.endpoint, request.Value)
	if err != nil {
		writeProviderCredentialError(w, err)
		return
	}
	pendingRestart := providerCredentialRestartRequired(credentialID)
	if pendingRestart {
		s.settingsPendingRestart.Store(true)
	}
	writeProviderCredentialResponse(w, snapshot.ETag, credentialID,
		providercredentials.State{Configured: true, Source: providercredentials.SourceStored}, pendingRestart)
}

func (s *Server) handleDeleteProviderCredential(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	credentialID := strings.TrimSpace(r.PathValue("credential_id"))
	if err := providercredentials.ValidateID(credentialID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_credential_id", "Provider credential ID is invalid")
		return
	}
	_, cfg, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	current, err := providercredentials.Read(cfg.TokensDir())
	if err != nil {
		writeProviderCredentialError(w, err)
		return
	}
	if !current.Stored(credentialID) {
		writeError(w, http.StatusNotFound, "credential_not_found", "No stored provider credential was found")
		return
	}
	snapshot, err := providercredentials.Delete(cfg.TokensDir(), ifMatch, credentialID)
	if err != nil {
		writeProviderCredentialError(w, err)
		return
	}
	// A credential may outlive the provider that config.toml referenced when
	// it was stored; deleting it must not depend on that provider still being
	// configured. The reported state falls back to the environment only when
	// the provider is still known.
	state := providercredentials.State{Configured: false, Source: providercredentials.SourceNone}
	if binding, ok := providerCredentialBindingForID(cfg, credentialID); ok {
		_, state, err = snapshot.Resolve(credentialID, binding.endpoint, binding.environment, osLookupEnv)
		if err != nil {
			writeProviderCredentialError(w, err)
			return
		}
	}
	pendingRestart := providerCredentialRestartRequired(credentialID)
	if pendingRestart {
		s.settingsPendingRestart.Store(true)
	}
	writeProviderCredentialResponse(w, snapshot.ETag, credentialID, state, pendingRestart)
}

func providerCredentialRestartRequired(id string) bool {
	return id == providercredentials.VectorEmbeddingsID || id == providercredentials.VectorMultimodalID
}

func providerCredentialBindingForID(cfg *config.Config, id string) (providerCredentialBinding, bool) {
	switch id {
	case providercredentials.VectorEmbeddingsID:
		return providerCredentialBinding{id: id, endpoint: cfg.Vector.Embeddings.Endpoint,
			environment: cfg.Vector.Embeddings.APIKeyEnv}, true
	case providercredentials.VectorMultimodalID:
		return providerCredentialBinding{id: id, endpoint: cfg.Vector.Multimodal.Endpoint,
			environment: cfg.Vector.Multimodal.APIKeyEnv}, true
	}
	if !strings.HasPrefix(id, "people.enrichment/") {
		return providerCredentialBinding{}, false
	}
	name := strings.TrimPrefix(id, "people.enrichment/")
	for _, provider := range cfg.People.Enrichment.Providers {
		if provider.Name == name {
			endpoint, err := provider.CredentialEndpoint()
			if err != nil {
				return providerCredentialBinding{}, false
			}
			return providerCredentialBinding{id: id, endpoint: endpoint, environment: provider.APIKeyEnv}, true
		}
	}
	return providerCredentialBinding{}, false
}

func writeProviderCredentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providercredentials.ErrConflict):
		writeError(w, http.StatusPreconditionFailed, "credential_conflict", "Provider credentials changed; reload settings and retry")
	case errors.Is(err, providercredentials.ErrOriginMismatch):
		writeError(w, http.StatusConflict, "credential_origin_mismatch", "Stored credential is bound to a different endpoint")
	case errors.Is(err, providercredentials.ErrUnavailable):
		writeError(w, http.StatusInternalServerError, "credential_store_unavailable", "Provider credential store is unavailable")
	default:
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "Provider credential settings are invalid")
	}
}

func writeProviderCredentialResponse(
	w http.ResponseWriter,
	etag, id string,
	state providercredentials.State,
	pendingRestart bool,
) {
	w.Header().Set(etagHeaderName, etag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ProviderCredentialResponse{
		CredentialID:   id,
		State:          SecretSettingState{Configured: state.Configured, Source: string(state.Source)},
		PendingRestart: pendingRestart,
	})
}
