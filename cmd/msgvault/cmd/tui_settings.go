package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/tui"
)

const (
	tuiSettingsPath             = "/api/v1/settings"
	tuiSettingsCredentialPrefix = "/api/v1/settings/provider-credentials/" // #nosec G101 -- HTTP route, not a credential.
	credentialETagHeader        = "Credential-Etag"                        // #nosec G101 -- concurrency header, not a credential.
)

type tuiDaemonSettingsBackend struct {
	client *daemonclient.Client
}

var _ tui.SettingsBackend = (*tuiDaemonSettingsBackend)(nil)

func newTUISettingsBackend(client *daemonclient.Client) *tuiDaemonSettingsBackend {
	return &tuiDaemonSettingsBackend{client: client}
}

type tuiSettingsHTTPResponse struct {
	Groups                    []tuiSettingsHTTPGroup        `json:"groups"`
	Settings                  []tuiSettingsHTTPField        `json:"settings"`
	PersonEnrichmentProviders []tuiPersonEnrichmentProvider `json:"person_enrichment_providers"`
	CredentialETag            string                        `json:"credential_etag"`
	PendingRestart            bool                          `json:"pending_restart"`
}

type tuiSettingsHTTPGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type tuiSettingsHTTPField struct {
	Key             string                    `json:"key"`
	CredentialID    string                    `json:"credential_id"`
	Group           string                    `json:"group"`
	Label           string                    `json:"label"`
	Description     string                    `json:"description"`
	Kind            string                    `json:"kind"`
	Value           *tuiSettingsHTTPValue     `json:"value"`
	Secret          *tuiSettingsHTTPSecret    `json:"secret"`
	Options         []string                  `json:"options"`
	ReadOnly        bool                      `json:"read_only"`
	RestartRequired bool                      `json:"restart_required"`
	Validation      tuiSettingsHTTPValidation `json:"validation"`
}

type tuiSettingsHTTPValue struct {
	String  *string   `json:"string,omitempty"`
	Integer *int      `json:"integer,omitempty"`
	Number  *float64  `json:"number,omitempty"`
	Boolean *bool     `json:"boolean,omitempty"`
	Strings *[]string `json:"strings,omitempty"`
}

type tuiSettingsHTTPSecret struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source"`
}

type tuiSettingsHTTPValidation struct {
	Hint     string   `json:"hint"`
	Required bool     `json:"required"`
	Minimum  *float64 `json:"minimum"`
	Maximum  *float64 `json:"maximum"`
}

type tuiPersonEnrichmentProvider struct {
	Name         string                 `json:"name"`
	Kind         string                 `json:"kind"`
	Enabled      bool                   `json:"enabled"`
	CredentialID string                 `json:"credential_id"`
	Credential   *tuiSettingsHTTPSecret `json:"credential"`
}

func (b *tuiDaemonSettingsBackend) LoadSettings(ctx context.Context) (tui.SettingsSnapshot, error) {
	if b == nil || b.client == nil {
		return tui.SettingsSnapshot{}, errors.New("daemon settings client unavailable")
	}
	resp, err := b.client.DoGeneratedRequestWithContext(ctx, http.MethodGet, tuiSettingsPath, nil)
	if err != nil {
		return tui.SettingsSnapshot{}, fmt.Errorf("load settings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return tui.SettingsSnapshot{}, daemonclient.HandleErrorResponse(resp)
	}
	var document tuiSettingsHTTPResponse
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&document); err != nil {
		return tui.SettingsSnapshot{}, fmt.Errorf("decode settings: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return tui.SettingsSnapshot{}, errors.New("decode settings: trailing data")
	}
	credentialETag := resp.Header.Get(credentialETagHeader)
	if credentialETag == "" {
		credentialETag = document.CredentialETag
	}
	return tuiSettingsSnapshot(document, resp.Header.Get("ETag"), credentialETag), nil
}

func tuiSettingsSnapshot(
	document tuiSettingsHTTPResponse,
	etag string,
	credentialETag string,
) tui.SettingsSnapshot {
	snapshot := tui.SettingsSnapshot{
		ETag:           etag,
		CredentialETag: credentialETag,
		PendingRestart: document.PendingRestart,
		Groups:         make([]tui.SettingsGroup, 0, len(document.Groups)),
		Fields: make(
			[]tui.SettingField,
			0,
			len(document.Settings)+len(document.PersonEnrichmentProviders),
		),
	}
	for _, group := range document.Groups {
		snapshot.Groups = append(snapshot.Groups, tui.SettingsGroup{ID: group.ID, Label: group.Label})
	}
	for _, field := range document.Settings {
		converted := tui.SettingField{
			Key:             field.Key,
			CredentialID:    field.CredentialID,
			Group:           field.Group,
			Label:           field.Label,
			Description:     field.Description,
			Kind:            tui.SettingKind(field.Kind),
			Options:         append([]string(nil), field.Options...),
			ReadOnly:        field.ReadOnly,
			RestartRequired: field.RestartRequired,
			Validation: tui.SettingValidation{
				Hint: field.Validation.Hint, Required: field.Validation.Required,
				Minimum: field.Validation.Minimum, Maximum: field.Validation.Maximum,
			},
		}
		if field.Value != nil {
			converted.Value = &tui.SettingValue{
				String: field.Value.String, Integer: field.Value.Integer,
				Number: field.Value.Number, Boolean: field.Value.Boolean,
				Strings: field.Value.Strings,
			}
		}
		if field.Secret != nil {
			converted.Secret = &tui.SecretSettingState{
				Configured: field.Secret.Configured,
				Source:     field.Secret.Source,
			}
		}
		snapshot.Fields = append(snapshot.Fields, converted)
	}
	for _, provider := range document.PersonEnrichmentProviders {
		if provider.Credential == nil || strings.TrimSpace(provider.CredentialID) == "" {
			continue
		}
		status := "disabled"
		if provider.Enabled {
			status = "enabled"
		}
		snapshot.Fields = append(snapshot.Fields, tui.SettingField{
			Key:          "people.enrichment.providers." + provider.Name + ".api_key",
			CredentialID: provider.CredentialID,
			Group:        "enrichment",
			Label:        provider.Name + " API key",
			Description: fmt.Sprintf(
				"Named person-enrichment provider. Kind: %s. Status: %s. Provider policy is read-only here; edit it in Web Settings.",
				provider.Kind,
				status,
			),
			Kind: tui.SettingKindSecret,
			Secret: &tui.SecretSettingState{
				Configured: provider.Credential.Configured,
				Source:     provider.Credential.Source,
			},
		})
	}
	return snapshot
}

type tuiSettingsRequestOptions struct {
	body    any
	headers map[string]string
}

func (o *tuiSettingsRequestOptions) GetPathParams() (map[string]any, error) {
	return map[string]any{}, nil
}
func (o *tuiSettingsRequestOptions) GetQuery() (map[string]any, error) {
	return map[string]any{}, nil
}
func (o *tuiSettingsRequestOptions) GetBody() any                          { return o.body }
func (o *tuiSettingsRequestOptions) GetHeader() (map[string]string, error) { return o.headers, nil }

type tuiSettingsPatchBody struct {
	Updates []tuiSettingsPatchUpdate `json:"updates"`
}

type tuiSettingsPatchUpdate struct {
	Key    string                      `json:"key"`
	Value  *tuiSettingsHTTPValue       `json:"value,omitempty"`
	Secret *tuiSettingsPatchSecretBody `json:"secret,omitempty"`
}

type tuiSettingsPatchSecretBody struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
}

type tuiCredentialSetBody struct {
	Value string `json:"value"`
}

func (b *tuiDaemonSettingsBackend) SaveSettings(
	ctx context.Context,
	request tui.SettingsSaveRequest,
) (tui.SettingsSnapshot, error) {
	if b == nil || b.client == nil {
		return tui.SettingsSnapshot{}, errors.New("daemon settings client unavailable")
	}
	savedKeys := make([]string, 0, len(request.Updates)+len(request.ConfigSecrets)+len(request.Credentials))
	configETag := request.ConfigETag
	credentialETag := request.CredentialETag

	if len(request.Updates)+len(request.ConfigSecrets) > 0 {
		newETag, newCredentialETag, err := b.saveConfigSettings(
			ctx, configETag, request.Updates, request.ConfigSecrets,
		)
		if err != nil {
			return tui.SettingsSnapshot{}, err
		}
		if newETag != "" {
			configETag = newETag
		}
		if newCredentialETag != "" {
			credentialETag = newCredentialETag
		}
		for _, update := range request.Updates {
			savedKeys = append(savedKeys, update.Key)
		}
		for _, update := range request.ConfigSecrets {
			savedKeys = append(savedKeys, update.Key)
		}
	}

	for _, credential := range request.Credentials {
		newETag, err := b.saveProviderCredential(ctx, credentialETag, credential)
		if err != nil {
			if len(savedKeys) > 0 {
				return tui.SettingsSnapshot{}, &tui.SettingsPartialSaveError{
					SavedKeys: append([]string(nil), savedKeys...), Err: err,
				}
			}
			return tui.SettingsSnapshot{}, err
		}
		if newETag == "" {
			err := errors.New("credential write returned no concurrency token")
			if len(savedKeys) > 0 {
				return tui.SettingsSnapshot{}, &tui.SettingsPartialSaveError{
					SavedKeys: append([]string(nil), savedKeys...), Err: err,
				}
			}
			return tui.SettingsSnapshot{}, err
		}
		credentialETag = newETag
		savedKeys = append(savedKeys, credential.Key)
	}

	snapshot, err := b.LoadSettings(ctx)
	if err != nil {
		return tui.SettingsSnapshot{}, &tui.SettingsPartialSaveError{
			SavedKeys: append([]string(nil), savedKeys...),
			Err:       fmt.Errorf("settings saved but could not be reloaded: %w", err),
		}
	}
	// Preserve a newly returned token when an older daemon omits one from the
	// follow-up GET. Current daemons return both.
	if snapshot.ETag == "" {
		snapshot.ETag = configETag
	}
	if snapshot.CredentialETag == "" {
		snapshot.CredentialETag = credentialETag
	}
	return snapshot, nil
}

func (b *tuiDaemonSettingsBackend) saveConfigSettings(
	ctx context.Context,
	etag string,
	updates []tui.SettingUpdate,
	secrets []tui.ConfigSecretUpdate,
) (string, string, error) {
	payload := tuiSettingsPatchBody{Updates: make([]tuiSettingsPatchUpdate, 0, len(updates)+len(secrets))}
	for _, update := range updates {
		payload.Updates = append(payload.Updates, tuiSettingsPatchUpdate{
			Key: update.Key, Value: tuiSettingsHTTPValueFromTUI(update.Value),
		})
	}
	for _, update := range secrets {
		payload.Updates = append(payload.Updates, tuiSettingsPatchUpdate{
			Key: update.Key,
			Secret: &tuiSettingsPatchSecretBody{
				Action: update.Action, Value: update.Value,
			},
		})
	}
	resp, err := b.client.DoGeneratedRequestWithContext(
		ctx,
		http.MethodPatch,
		tuiSettingsPath,
		&tuiSettingsRequestOptions{
			body: payload, headers: map[string]string{"If-Match": etag},
		},
	)
	if err != nil {
		return "", "", fmt.Errorf("save settings: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", "", &tui.SettingsConflictError{
			Scope: tui.SettingsConflictConfig,
			Err:   daemonclient.HandleErrorResponse(resp),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", daemonclient.HandleErrorResponse(resp)
	}
	return resp.Header.Get("ETag"), resp.Header.Get(credentialETagHeader), nil
}

func tuiSettingsHTTPValueFromTUI(value *tui.SettingValue) *tuiSettingsHTTPValue {
	if value == nil {
		return nil
	}
	return &tuiSettingsHTTPValue{
		String: value.String, Integer: value.Integer, Number: value.Number,
		Boolean: value.Boolean, Strings: value.Strings,
	}
}

func (b *tuiDaemonSettingsBackend) saveProviderCredential(
	ctx context.Context,
	etag string,
	update tui.CredentialUpdate,
) (string, error) {
	provider, err := validateTUICredentialID(update.CredentialID)
	if err != nil {
		return "", err
	}
	method := http.MethodPut
	var body any = tuiCredentialSetBody{Value: update.Value}
	switch update.Action {
	case "set":
		if update.Value == "" {
			return "", errors.New("credential value is required")
		}
	case "clear":
		method = http.MethodDelete
		body = nil
	default:
		return "", fmt.Errorf("unsupported credential action %q", update.Action)
	}
	resp, err := b.client.DoGeneratedRequestWithContext(
		ctx,
		method,
		tuiSettingsCredentialPrefix+url.PathEscape(provider),
		&tuiSettingsRequestOptions{
			body: body, headers: map[string]string{"If-Match": etag},
		},
	)
	if err != nil {
		return "", fmt.Errorf("save provider credential: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", &tui.SettingsConflictError{
			Scope: tui.SettingsConflictCredentials,
			Err:   daemonclient.HandleErrorResponse(resp),
		}
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", daemonclient.HandleErrorResponse(resp)
	}
	return resp.Header.Get("ETag"), nil
}

func validateTUICredentialID(id string) (string, error) {
	if id == "" {
		return "", errors.New("provider credential ID is required")
	}
	if err := providercredentials.ValidateID(id); err != nil {
		return "", fmt.Errorf("unsupported provider credential ID %q: %w", id, err)
	}
	return id, nil
}
