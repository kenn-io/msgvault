package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
	"go.kenn.io/msgvault/internal/store"
)

type PeopleInferencePolicyUpdateRequest personenrollment.PolicyUpdate

// PeopleInferencePolicyUpdateError reports consent and rollback outcomes without
// exposing provider diagnostics or credentials. Preflight failures omit flags.
type PeopleInferencePolicyUpdateError struct {
	Error                     string `json:"error"`
	Message                   string `json:"message,omitempty"`
	ConsentRemainsRevoked     *bool  `json:"consent_remains_revoked,omitempty"`
	RolledBack                *bool  `json:"rolled_back,omitempty"`
	OperationMayHaveCompleted *bool  `json:"operation_may_have_completed,omitempty"`
}

func (s *Server) registerPeopleInferencePolicyUpdateRoute(api huma.API) {
	operation := rawAPIV1Operation("patchSettingsPeopleInferencePolicy", http.MethodPatch, "/settings/people-inference/providers/{name}/policy", "Update existing non-secret people inference policy")
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: nameKey, In: pathKey, Required: true, Schema: &huma.Schema{Type: huma.TypeString}}, &huma.Param{Name: ifMatchHeaderName, In: headerParamLocation, Required: true, Description: "Exact strong config ETag from the settings read", Schema: &huma.Schema{Type: huma.TypeString}})
	operation.RequestBody = jsonRequestBodyFor[PeopleInferencePolicyUpdateRequest](api)
	operation.Responses = jsonResponsesFor[PeopleInferenceSettingsResponse](api)
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed, http.StatusPreconditionRequired, http.StatusUnprocessableEntity, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		operation.Responses[httpStatusKey(status)] = jsonResponsesFor[PeopleInferencePolicyUpdateError](api, status)[httpStatusKey(status)]
	}
	addSettingsETagHeader(operation.Responses[httpStatusKey(http.StatusOK)])
	registerRawHumaRoute(api, operation, s.handleUpdatePeopleInferencePolicy)
}

var errPeoplePolicyGate = errors.New("people provider policy gate is unavailable")
var errPeoplePolicyCredential = errors.New("people provider credential changed during policy update")
var errPeoplePolicyCheck = errors.New("people provider synthetic check failed")

func (s *Server) handleUpdatePeopleInferencePolicy(w http.ResponseWriter, r *http.Request) {
	ifMatch, ok := requiredSingleIfMatch(w, r)
	if !ok {
		return
	}
	var request PeopleInferencePolicyUpdateRequest
	if !decodeStrictSettingsJSON(w, r, &request) {
		return
	}
	update := personenrollment.PolicyUpdate(request)
	if !update.HasChanges() {
		writeError(w, http.StatusBadRequest, "invalid_policy_update", "Supply at least one non-secret policy field")
		return
	}
	before, configured, selected, old, ok := s.peopleInferenceProfileForRequest(w, r, ifMatch)
	if !ok {
		return
	}
	st, ok := s.store.(peopleInferencePolicyStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_inference_unavailable", "People inference policy store is unavailable")
		return
	}
	name := r.PathValue("name")
	candidate, err := update.Apply(selected.Providers[name])
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "provider_invalid", "People inference policy is invalid")
		return
	}
	if candidate.Protocol == peoplesweep.ProtocolCodexAppServer && !peoplesweep.CodexReleaseAvailable() {
		writeError(w, http.StatusServiceUnavailable, "codex_unavailable", "Codex inference is unavailable until an inference build is approved")
		return
	}
	proposed := selected
	proposed.Providers = maps.Clone(selected.Providers)
	proposed.Providers[name] = candidate
	profile, err := proposed.Profile()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "provider_invalid", "People inference policy is invalid")
		return
	}
	if err := config.ValidateConfigTableEdits(before, []config.TableEdit{personenrollment.ProviderUpdateEdit(name, candidate)}); err != nil {
		s.writeSettingsConfigError(w, err)
		return
	}
	credentials := peoplesweep.NewFileCredentialStore(configured.TokensDir())
	resolver := peoplesweep.NewCredentialResolver(credentials, os.LookupEnv)
	credential, err := resolver.Resolve(name, profile)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "The existing provider credential is unavailable")
		return
	}
	var credentialRevision string
	if profile.Credential == peoplesweep.CredentialStored {
		credentialRevision, _, err = credentials.Revision(name)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "People provider credential store is unavailable")
			return
		}
	}
	client := s.peopleInferenceHTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	registry, err := peoplesweep.NewDriverRegistryWithCodexAuthHome(client, peoplesweep.NewCodexCommandStarter(), peoplesweep.NewReleasedCodexIsolationGate(), filepath.Join(configured.TokensDir(), "people-codex"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "provider_unavailable", "People inference provider is unavailable")
		return
	}
	if candidate.Protocol != peoplesweep.ProtocolCodexAppServer {
		capabilities, err := peoplesweep.NewCapabilityChecker(registry).Negotiate(r.Context(), candidate, credential)
		if err != nil {
			writeError(w, http.StatusBadGateway, "provider_negotiation_failed", "Synthetic provider policy negotiation failed; policy and consent are unchanged")
			return
		}
		candidate.OutputMode = capabilities.OutputMode
		candidate.TokenLimitParameter = capabilities.TokenLimitParameter
		candidate.ReasoningEffort = capabilities.ReasoningEffort
		candidate.ReasoningMode = capabilities.ReasoningMode
		candidate.DriverVersion = capabilities.DriverVersion
	}
	proposed.Providers[name] = candidate
	profile, err = proposed.Profile()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "provider_invalid", "Negotiated people inference policy is invalid")
		return
	}
	edits := []config.TableEdit{personenrollment.ProviderUpdateEdit(name, candidate)}
	if err := config.ValidateConfigTableEdits(before, edits); err != nil {
		s.writeSettingsConfigError(w, err)
		return
	}
	fingerprints := []string{old.Fingerprint}
	if s.cfg.People.Sweep.Enabled && s.cfg.People.Sweep.Provider.Name == name {
		running, err := s.cfg.People.Sweep.Profile()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "running_provider_invalid", "Running people inference policy is invalid")
			return
		}
		if running.Fingerprint != old.Fingerprint {
			fingerprints = append(fingerprints, running.Fingerprint)
		}
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	acquire := func(ctx context.Context) error {
		if s.operationGate == nil {
			return nil
		}
		done, ok := beginGateWorkBounded(ctx, s.operationGate, "people provider policy update")
		if !ok {
			return errPeoplePolicyGate
		}
		release = done
		return nil
	}
	unlock := func() {
		if release != nil {
			release()
			release = nil
		}
	}
	validate := func(etag string) error {
		current, _, err := s.readPersistedSettings()
		if err != nil {
			return err
		}
		if current.ETag != etag {
			return config.ErrConfigConflict
		}
		if credentialRevision != "" {
			currentRevision, _, err := credentials.Revision(name)
			if err != nil {
				return err
			}
			if currentRevision != credentialRevision {
				return errPeoplePolicyCredential
			}
		}
		return nil
	}
	after, err := personenrollment.UpdatePolicy(r.Context(), before, personenrollment.PolicyUpdateHooks{
		Revoke: func(ctx context.Context, guard bool) error {
			if guard {
				if err := acquire(ctx); err != nil {
					return err
				}
				if err := validate(ifMatch); err != nil {
					return err
				}
			} else {
				defer unlock()
			}
			for _, fingerprint := range fingerprints {
				if _, err := st.RevokePersonInferenceConsent(ctx, fingerprint, "policy-update"); err != nil {
					return err
				}
			}
			return nil
		},
		Publish: func(etag string) (config.ConfigFile, error) {
			return config.EditConfigTables(s.cfg.ConfigFilePath(), etag, edits)
		},
		Restore: func(published, original config.ConfigFile) (config.ConfigFile, error) {
			// Publication can fail while the first mutation phase still holds the gate.
			// Check failures arrive after it was released for network access.
			if release == nil {
				if err := acquire(r.Context()); err != nil {
					return config.ConfigFile{}, err
				}
				defer unlock()
			}
			return config.RestoreConfigFile(s.cfg.ConfigFilePath(), published, original)
		},
		Check: func(ctx context.Context, published config.ConfigFile) error {
			loaded, err := config.LoadConfigFile(published, configured.HomeDir)
			if err != nil {
				return err
			}
			checkConfig := loaded.People.Sweep
			checkConfig.Enabled = true
			checkConfig.Provider.Name = name
			runner, err := peoplesweep.NewRunner(checkConfig, st, registry, resolver)
			if err != nil {
				return errors.Join(errPeoplePolicyCheck, err)
			}
			response, err := runner.Check(ctx)
			if err != nil {
				return errors.Join(errPeoplePolicyCheck, err)
			}
			if !peoplesweep.DriverVersionMatches(profile.DriverVersion, response.ProviderVersion) {
				return errPeoplePolicyCheck
			}
			if err := acquire(ctx); err != nil {
				return err
			}
			defer unlock()
			if err := validate(published.ETag); err != nil {
				return err
			}
			if _, err := st.EnsurePersonInferenceProfile(ctx, profile); err != nil {
				return err
			}
			return st.RecordPersonInferenceCheck(ctx, store.PersonInferenceCheck{ProfileFingerprint: profile.Fingerprint, CheckedAt: time.Now().UTC(), DriverVersion: profile.DriverVersion, OutputMode: profile.OutputMode, ProviderRequestID: response.ProviderRequestID, ModelVersion: response.ModelVersion})
		},
	})
	if err != nil {
		if failure, ok := errors.AsType[*personenrollment.PolicyUpdateError](err); ok {
			status := http.StatusInternalServerError
			code := "provider_policy_update_failed"
			if failure.Phase == "check" && failure.RolledBack {
				status = http.StatusBadGateway
				code = "provider_check_failed"
			}
			revoked, uncertain := true, !failure.RolledBack
			writeJSON(w, status, PeopleInferencePolicyUpdateError{Error: code, ConsentRemainsRevoked: &revoked, RolledBack: &failure.RolledBack, OperationMayHaveCompleted: &uncertain})
			return
		}
		switch {
		case errors.Is(err, config.ErrConfigConflict):
			writeError(w, http.StatusPreconditionFailed, "settings_conflict", "The config file changed before policy publication")
		case errors.Is(err, errPeoplePolicyCredential):
			writeError(w, http.StatusConflict, "credential_conflict", "Provider credentials changed during negotiation")
		case errors.Is(err, errPeoplePolicyGate):
			writeOperationGateBusy(w, r, s.operationGate)
		default:
			writeError(w, http.StatusInternalServerError, "provider_policy_update_failed", "Could not update people provider policy")
		}
		return
	}
	s.settingsPendingRestart.Store(true)
	loaded, err := config.LoadConfigFile(after, configured.HomeDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Policy changed but its settings response is unavailable")
		return
	}
	response, err := s.buildPeopleInferenceSettingsResponse(r.Context(), loaded)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings_read_failed", "Policy changed but its settings response is unavailable")
		return
	}
	w.Header().Set(etagHeaderName, after.ETag)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

type peopleInferencePolicyStore interface {
	peopleInferenceCheckStore
	RevokePersonInferenceConsent(ctx context.Context, fingerprint string, actor string) (bool, error)
}
