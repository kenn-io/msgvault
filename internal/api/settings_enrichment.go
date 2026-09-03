package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

const settingsPersonEnrichmentProviderRoute = "/settings/person-enrichment/providers/{name}"

// PersonEnrichmentProviderSetting is the browser/TUI-safe provider policy.
// APIKeyEnv and hard monetary caps deliberately remain host-only.
type PersonEnrichmentProviderSetting struct {
	Name                  string              `json:"name"`
	Kind                  string              `json:"kind" enum:"exa,sixtyfour"`
	Enabled               bool                `json:"enabled"`
	Endpoint              string              `json:"endpoint"`
	PollEndpoint          string              `json:"poll_endpoint,omitempty"`
	Mode                  string              `json:"mode,omitempty"`
	Tier                  string              `json:"tier,omitempty"`
	NumResults            int                 `json:"num_results,omitempty"`
	AllowedIdentifiers    []string            `json:"allowed_identifiers"`
	TargetKeys            []string            `json:"target_keys"`
	AllowSensitiveTargets bool                `json:"allow_sensitive_targets"`
	RetentionPosture      string              `json:"retention_posture"`
	TrainingPosture       string              `json:"training_posture"`
	RefreshInterval       string              `json:"refresh_interval"`
	RequestTimeout        string              `json:"request_timeout"`
	PollInterval          string              `json:"poll_interval"`
	MaxJobAge             string              `json:"max_job_age"`
	MaxRetries            int                 `json:"max_retries"`
	MaxRequestsPerRun     int64               `json:"max_requests_per_run"`
	MaxRequestsPerDay     int64               `json:"max_requests_per_day"`
	Credential            *SecretSettingState `json:"credential,omitempty"`
	CredentialID          string              `json:"credential_id"`
}

type PersonEnrichmentProviderUpdate struct {
	Kind                  string   `json:"kind" enum:"exa,sixtyfour"`
	Enabled               bool     `json:"enabled"`
	Endpoint              string   `json:"endpoint"`
	PollEndpoint          string   `json:"poll_endpoint,omitempty"`
	Mode                  string   `json:"mode,omitempty"`
	Tier                  string   `json:"tier,omitempty"`
	NumResults            *int     `json:"num_results,omitempty"`
	AllowedIdentifiers    []string `json:"allowed_identifiers"`
	TargetKeys            []string `json:"target_keys"`
	AllowSensitiveTargets bool     `json:"allow_sensitive_targets"`
	RetentionPosture      string   `json:"retention_posture"`
	TrainingPosture       string   `json:"training_posture"`
	RefreshInterval       string   `json:"refresh_interval"`
	RequestTimeout        string   `json:"request_timeout"`
	PollInterval          string   `json:"poll_interval,omitempty"`
	MaxJobAge             string   `json:"max_job_age,omitempty"`
	MaxRetries            int      `json:"max_retries"`
	MaxRequestsPerRun     int64    `json:"max_requests_per_run"`
	MaxRequestsPerDay     int64    `json:"max_requests_per_day"`
}

func (s *Server) registerPersonEnrichmentSettingsRoute(api huma.API) {
	operation := rawAPIV1Operation("putSettingsPersonEnrichmentProvider", http.MethodPut,
		settingsPersonEnrichmentProviderRoute, "Create or update one named person-enrichment provider")
	operation.Parameters = append(operation.Parameters,
		&huma.Param{Name: "name", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}},
		&huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
			Description: "Strong config ETag returned by the latest settings read", Schema: &huma.Schema{Type: huma.TypeString}},
	)
	operation.RequestBody = jsonRequestBodyFor[PersonEnrichmentProviderUpdate](api)
	operation.Responses = jsonResponsesFor[SettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusUnprocessableEntity} {
		operation.Responses[httpStatusKey(status)] = errorResponseFor(api)
	}
	addSettingsETagHeader(operation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, operation, s.handlePutPersonEnrichmentProviderSetting)
}

func (s *Server) handlePutPersonEnrichmentProviderSetting(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if err := providercredentials.ValidateID(providercredentials.PersonEnrichmentID(name)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_provider_name", "Provider name is invalid")
		return
	}
	var request PersonEnrichmentProviderUpdate
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	snapshot, current, err := s.readPersistedSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	providers := append([]personenrichment.ProviderConfig(nil), current.People.Enrichment.Providers...)
	index := -1
	for providerIndex := range providers {
		if providers[providerIndex].Name == name {
			index = providerIndex
			break
		}
	}
	var base personenrichment.ProviderConfig
	if index >= 0 {
		base = providers[index]
		if request.Kind != base.Kind {
			writeError(w, http.StatusConflict, "provider_kind_conflict", "Provider kind cannot change for an existing name")
			return
		}
	} else {
		base = personenrichment.ProviderConfig{Name: name, Kind: request.Kind}
		base.ApplyDefaults()
	}
	updated, err := applyPersonEnrichmentProviderUpdate(base, request)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "Person-enrichment provider settings are invalid")
		return
	}
	if ifMatch != snapshot.ETag {
		writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed; reload settings and retry")
		return
	}
	written, err := config.EditPersonEnrichmentProvider(s.cfg.ConfigFilePath(), ifMatch, name, updated)
	if err != nil {
		s.writeSettingsConfigError(w, err)
		return
	}
	loaded, err := config.LoadConfigFile(written, "")
	if err != nil {
		s.settingsPendingRestart.Store(true)
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	credentials, err := providercredentials.Read(loaded.TokensDir())
	if err != nil {
		s.settingsPendingRestart.Store(true)
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	// A stored credential is bound to the endpoint origin it was saved for.
	// Moving the provider elsewhere severs it in the same request, and a
	// credential left behind by an earlier interrupted cleanup or by a
	// removed provider with the same name is severed here as well.
	credentialID := providercredentials.PersonEnrichmentID(name)
	if storedCredentialOriginMismatch(credentials, credentialID, updated.Endpoint) {
		deleter := s.settingsCredentialDeleter
		if deleter == nil {
			deleter = deleteStoredCredentials
		}
		credentials, err = deleter(loaded.TokensDir(), credentials, []string{credentialID})
		if err != nil {
			if _, rollbackErr := config.RestoreConfigFile(s.cfg.ConfigFilePath(), written, snapshot); rollbackErr != nil {
				s.settingsPendingRestart.Store(true)
				s.writeSettingsConfigError(w, errors.Join(config.ErrConfigChanged, err, rollbackErr))
				return
			}
			writeProviderCredentialError(w, err)
			return
		}
	}
	s.settingsPendingRestart.Store(true)
	response, err := s.buildSettingsResponse(r.Context(), loaded, credentials, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Could not read settings")
		return
	}
	w.Header().Set(etagHeaderName, written.ETag)
	w.Header().Set(settingsCredentialETagHeader, credentials.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func applyPersonEnrichmentProviderUpdate(
	provider personenrichment.ProviderConfig,
	request PersonEnrichmentProviderUpdate,
) (personenrichment.ProviderConfig, error) {
	// Kind is immutable once a name exists, so it must be valid even for a
	// disabled provider; otherwise the name could never be enabled.
	if request.Kind != personenrichment.ProviderExa && request.Kind != personenrichment.ProviderSixtyfour {
		return personenrichment.ProviderConfig{}, fmt.Errorf(
			"kind must be %q or %q", personenrichment.ProviderExa, personenrichment.ProviderSixtyfour)
	}
	provider.Kind = request.Kind
	provider.Enabled = request.Enabled
	provider.Endpoint = strings.TrimSpace(request.Endpoint)
	provider.PollEndpoint = strings.TrimSpace(request.PollEndpoint)
	provider.Mode = strings.TrimSpace(request.Mode)
	provider.Tier = strings.TrimSpace(request.Tier)
	if request.NumResults != nil {
		provider.NumResults = *request.NumResults
	}
	provider.AllowedIdentifiers = make([]personenrichment.IdentifierClass, len(request.AllowedIdentifiers))
	for index, identifier := range request.AllowedIdentifiers {
		provider.AllowedIdentifiers[index] = personenrichment.IdentifierClass(identifier)
	}
	provider.TargetKeys = append([]string(nil), request.TargetKeys...)
	provider.AllowSensitiveTargets = request.AllowSensitiveTargets
	provider.RetentionPosture = request.RetentionPosture
	provider.TrainingPosture = request.TrainingPosture
	var err error
	if provider.RefreshInterval, err = time.ParseDuration(request.RefreshInterval); err != nil {
		return personenrichment.ProviderConfig{}, fmt.Errorf("parse refresh interval: %w", err)
	}
	if provider.RequestTimeout, err = time.ParseDuration(request.RequestTimeout); err != nil {
		return personenrichment.ProviderConfig{}, fmt.Errorf("parse request timeout: %w", err)
	}
	if request.PollInterval != "" {
		if provider.PollInterval, err = time.ParseDuration(request.PollInterval); err != nil {
			return personenrichment.ProviderConfig{}, fmt.Errorf("parse poll interval: %w", err)
		}
	}
	if request.MaxJobAge != "" {
		if provider.MaxJobAge, err = time.ParseDuration(request.MaxJobAge); err != nil {
			return personenrichment.ProviderConfig{}, fmt.Errorf("parse maximum job age: %w", err)
		}
	}
	provider.MaxRetries = request.MaxRetries
	provider.MaxRequestsPerRun = request.MaxRequestsPerRun
	provider.MaxRequestsPerDay = request.MaxRequestsPerDay
	if provider.Endpoint != "" {
		if _, err := providercredentials.EndpointOrigin(provider.Endpoint); err != nil {
			return personenrichment.ProviderConfig{}, err
		}
	}
	if provider.PollEndpoint != "" {
		if _, err := providercredentials.EndpointOrigin(provider.PollEndpoint); err != nil {
			return personenrichment.ProviderConfig{}, err
		}
	}
	if _, err := provider.CredentialEndpoint(); err != nil {
		return personenrichment.ProviderConfig{}, err
	}
	if err := provider.Validate(); err != nil {
		return personenrichment.ProviderConfig{}, err
	}
	return provider, nil
}

func personEnrichmentProviderSettings(
	cfg *config.Config,
	credentials providercredentials.Snapshot,
) ([]PersonEnrichmentProviderSetting, error) {
	result := make([]PersonEnrichmentProviderSetting, 0, len(cfg.People.Enrichment.Providers))
	for _, provider := range cfg.People.Enrichment.Providers {
		identifiers := make([]string, len(provider.AllowedIdentifiers))
		for index, identifier := range provider.AllowedIdentifiers {
			identifiers[index] = string(identifier)
		}
		credentialID := providercredentials.PersonEnrichmentID(provider.Name)
		var credentialState *SecretSettingState
		credentialEndpoint, err := provider.CredentialEndpoint()
		if err != nil {
			if provider.Enabled {
				return nil, err
			}
		} else {
			_, state, resolveErr := credentials.Resolve(credentialID, credentialEndpoint, provider.APIKeyEnv, osLookupEnv)
			if errors.Is(resolveErr, providercredentials.ErrOriginMismatch) {
				state = providercredentials.State{Configured: false, Source: providercredentials.SourceNone}
			} else if resolveErr != nil {
				return nil, resolveErr
			}
			credentialState = &SecretSettingState{Configured: state.Configured, Source: string(state.Source)}
		}
		result = append(result, PersonEnrichmentProviderSetting{
			Name: provider.Name, Kind: provider.Kind, Enabled: provider.Enabled,
			Endpoint:     safePublicProviderEndpoint(provider.Endpoint),
			PollEndpoint: safePublicProviderEndpoint(provider.PollEndpoint), Mode: provider.Mode,
			Tier: provider.Tier, NumResults: provider.NumResults, AllowedIdentifiers: identifiers,
			TargetKeys:            append([]string(nil), provider.TargetKeys...),
			AllowSensitiveTargets: provider.AllowSensitiveTargets,
			RetentionPosture:      provider.RetentionPosture, TrainingPosture: provider.TrainingPosture,
			RefreshInterval: provider.RefreshInterval.String(), RequestTimeout: provider.RequestTimeout.String(),
			PollInterval: provider.PollInterval.String(), MaxJobAge: provider.MaxJobAge.String(),
			MaxRetries: provider.MaxRetries, MaxRequestsPerRun: provider.MaxRequestsPerRun,
			MaxRequestsPerDay: provider.MaxRequestsPerDay,
			Credential:        credentialState,
			CredentialID:      credentialID,
		})
	}
	return result, nil
}

func safePublicProviderEndpoint(endpoint string) string {
	if strings.TrimSpace(endpoint) == "" {
		return ""
	}
	if _, err := providercredentials.EndpointOrigin(endpoint); err != nil {
		return ""
	}
	return endpoint
}

func decodeStrictSettingsJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid settings request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid settings request")
		return false
	}
	return true
}

func requiredSingleIfMatch(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values(ifMatchHeaderName)
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return "", false
	}
	return values[0], true
}

func osLookupEnv(name string) (string, bool) { return os.LookupEnv(name) }
