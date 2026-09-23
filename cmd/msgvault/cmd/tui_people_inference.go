package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/tui"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var _ tui.PeopleInferenceControlBackend = (*tuiDaemonSettingsBackend)(nil)
var _ tui.PeopleInferenceBackend = (*tuiDaemonSettingsBackend)(nil)

// People inference enrollment runs through daemon routes; the TUI does not
// store Codex credentials or reproduce the daemon's enrollment rules.
func (b *tuiDaemonSettingsBackend) LoadPeopleInferenceStatus(ctx context.Context) (tui.PeopleInferenceStatus, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	response, err := client.GetSettingsPeopleInferenceWithResponse(ctx)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("load people inference status", status, err)
	}
	if response.JSON200 == nil {
		return tui.PeopleInferenceStatus{}, errors.New("load people inference status: empty response")
	}
	return tuiPeopleInferenceStatus(response.JSON200), nil
}

func (b *tuiDaemonSettingsBackend) CreatePeopleInferencePreset(
	ctx context.Context, name string, request tui.PeopleInferencePresetRequest,
) (tui.PeopleInferenceStatus, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	etag, err := b.peopleInferenceETag(ctx, client)
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	body := generated.PeopleInferencePresetCreateRequest{
		PresetID:         generated.PeopleInferencePresetCreateRequestPresetID(request.PresetID),
		Model:            request.Model,
		RetentionPosture: request.RetentionPosture,
		TrainingPosture:  request.TrainingPosture,
		AllowedSources:   request.AllowedSources,
		SourceSince:      request.SourceSince,
		AllowSensitive:   request.AllowSensitive,
	}
	if request.SourceUntil != "" {
		body.SourceUntil = &request.SourceUntil
	}
	response, err := client.PutSettingsPeopleInferencePresetWithResponse(ctx,
		&generated.PutSettingsPeopleInferencePresetRequestOptions{
			PathParams: &generated.PutSettingsPeopleInferencePresetPath{Name: name},
			Header:     &generated.PutSettingsPeopleInferencePresetHeaders{IfMatch: etag},
			Body:       &body,
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceStatus{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("create people inference preset", response.StatusCode, err)}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("create people inference preset", status, err)
	}
	if response.JSON200 == nil {
		return tui.PeopleInferenceStatus{}, errors.New("create people inference preset: empty response")
	}
	return tuiPeopleInferenceStatus(response.JSON200), nil
}

// SetPeopleInferenceKey writes one API key after reading that profile's own
// credential revision. The settings config ETag is never a credential CAS.
func (b *tuiDaemonSettingsBackend) SetPeopleInferenceKey(
	ctx context.Context, name, value string,
) (tui.PeopleInferenceStatus, error) {
	if value == "" {
		return tui.PeopleInferenceStatus{}, errors.New("people inference API key is required")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	revision, err := b.peopleInferenceCredentialRevision(ctx, client, name)
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	response, err := client.PutSettingsPeopleInferenceKeyWithResponse(ctx,
		&generated.PutSettingsPeopleInferenceKeyRequestOptions{
			PathParams: &generated.PutSettingsPeopleInferenceKeyPath{Name: name},
			Header:     &generated.PutSettingsPeopleInferenceKeyHeaders{IfMatch: revision},
			Body:       &generated.PeopleInferenceKeyWriteRequest{Value: value},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceStatus{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictPeopleCredentials,
			Err: peopleInferenceHTTPError("set people inference key", response.StatusCode, err)}
	}
	if response != nil && response.StatusCode == http.StatusConflict && response.JSON409 != nil &&
		response.JSON409.ErrorData == "provider_binding_changed" {
		return tui.PeopleInferenceStatus{}, errors.New("provider destination changed; reload settings")
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("set people inference key", status, err)
	}
	if response.JSON200 == nil {
		return tui.PeopleInferenceStatus{}, errors.New("set people inference key: empty response")
	}
	return tuiPeopleInferenceStatus(response.JSON200), nil
}

func (b *tuiDaemonSettingsBackend) CheckCodexProfile(
	ctx context.Context, name string,
) (tui.PeopleInferenceDisclosure, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceDisclosure{}, err
	}
	etag, err := b.peopleInferenceETag(ctx, client)
	if err != nil {
		return tui.PeopleInferenceDisclosure{}, err
	}
	response, err := client.CheckSettingsPeopleInferenceProviderWithResponse(ctx,
		&generated.CheckSettingsPeopleInferenceProviderRequestOptions{
			PathParams: &generated.CheckSettingsPeopleInferenceProviderPath{Name: name},
			Header:     &generated.CheckSettingsPeopleInferenceProviderHeaders{IfMatch: etag},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceDisclosure{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("check people inference profile", response.StatusCode, err)}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceDisclosure{}, peopleInferenceHTTPError("check people inference profile", status, err)
	}
	if response.JSON200 == nil || !response.JSON200.Ok || response.JSON200.Fingerprint == "" {
		return tui.PeopleInferenceDisclosure{}, errors.New("check people inference profile: no successful check was returned")
	}
	profile, _, err := b.peopleInferenceProfileSnapshot(ctx, client, name)
	if err != nil {
		return tui.PeopleInferenceDisclosure{}, err
	}
	if profile.Fingerprint == nil || *profile.Fingerprint != response.JSON200.Fingerprint || profile.Model != response.JSON200.Model || !profile.Checked {
		return tui.PeopleInferenceDisclosure{}, errors.New("people inference profile changed after check; reload settings")
	}
	return tui.PeopleInferenceDisclosure{
		Profile: name, Fingerprint: response.JSON200.Fingerprint, Text: peopleInferenceDisclosureText(profile),
	}, nil
}

func (b *tuiDaemonSettingsBackend) ConsentCodexProfile(
	ctx context.Context, name, fingerprint string,
) error {
	if fingerprint == "" {
		return errors.New("check people inference profile before consenting")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return err
	}
	profile, etag, err := b.peopleInferenceProfileSnapshot(ctx, client, name)
	if err != nil {
		return err
	}
	if profile.Fingerprint == nil || *profile.Fingerprint != fingerprint {
		return errors.New("people inference profile changed after check; reload settings")
	}
	if !profile.Checked {
		return errors.New("run an exact synthetic check before consenting")
	}
	response, err := client.ConsentSettingsPeopleInferenceProviderWithResponse(ctx,
		&generated.ConsentSettingsPeopleInferenceProviderRequestOptions{
			PathParams: &generated.ConsentSettingsPeopleInferenceProviderPath{Name: name},
			Header:     &generated.ConsentSettingsPeopleInferenceProviderHeaders{IfMatch: etag},
			Body:       &generated.PeopleInferenceConsentRequest{Fingerprint: fingerprint, Confirmed: true},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("consent to people inference profile", response.StatusCode, err)}
	}
	if response != nil && response.StatusCode == http.StatusConflict && response.JSON409 != nil {
		switch response.JSON409.ErrorData {
		case "consent_disclosure_changed":
			return errors.New("people inference disclosure changed; run the check again")
		case "check_required":
			return errors.New("run an exact synthetic check before consenting")
		}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return peopleInferenceHTTPError("consent to people inference profile", status, err)
	}
	if response.JSON200 == nil {
		return errors.New("consent to people inference profile: empty response")
	}
	for _, latest := range response.JSON200.Profiles {
		if latest.Name == name && latest.Fingerprint != nil && *latest.Fingerprint == fingerprint && latest.ConsentActive {
			return nil
		}
	}
	return errors.New("consent to people inference profile: confirmation was not recorded")
}

func (b *tuiDaemonSettingsBackend) RevokePeopleInferenceConsent(
	ctx context.Context, name, fingerprint string,
) (tui.PeopleInferenceStatus, error) {
	if fingerprint == "" {
		return tui.PeopleInferenceStatus{}, errors.New("people inference profile fingerprint is required")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	profile, etag, err := b.peopleInferenceProfileSnapshot(ctx, client, name)
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	if profile.Fingerprint == nil || *profile.Fingerprint != fingerprint {
		return tui.PeopleInferenceStatus{}, errors.New("people inference profile changed; reload settings")
	}
	response, err := client.RevokeSettingsPeopleInferenceProviderWithResponse(ctx,
		&generated.RevokeSettingsPeopleInferenceProviderRequestOptions{
			PathParams: &generated.RevokeSettingsPeopleInferenceProviderPath{Name: name},
			Header:     &generated.RevokeSettingsPeopleInferenceProviderHeaders{IfMatch: etag},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceStatus{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("revoke people inference consent", response.StatusCode, err)}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("revoke people inference consent", status, err)
	}
	if response.JSON200 == nil {
		return tui.PeopleInferenceStatus{}, errors.New("revoke people inference consent: empty response")
	}
	for _, latest := range response.JSON200.Profiles {
		if latest.Name == name && latest.Fingerprint != nil && *latest.Fingerprint == fingerprint && !latest.ConsentActive {
			return tuiPeopleInferenceStatus(response.JSON200), nil
		}
	}
	return tui.PeopleInferenceStatus{}, errors.New("revoke people inference consent: revocation was not recorded")
}

func (b *tuiDaemonSettingsBackend) DisablePeopleInference(
	ctx context.Context, fingerprint string,
) (tui.PeopleInferenceStatus, error) {
	if fingerprint == "" {
		return tui.PeopleInferenceStatus{}, errors.New("configured people inference fingerprint is required")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	settings, etag, err := b.peopleInferenceSettingsSnapshot(ctx, client)
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	if settings.ConfiguredFingerprint == nil || *settings.ConfiguredFingerprint != fingerprint {
		return tui.PeopleInferenceStatus{}, errors.New("configured people inference profile changed; reload settings")
	}
	response, err := client.DisableSettingsPeopleInferenceWithResponse(ctx,
		&generated.DisableSettingsPeopleInferenceRequestOptions{
			Header: &generated.DisableSettingsPeopleInferenceHeaders{IfMatch: etag},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceStatus{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("disable people inference", response.StatusCode, err)}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("disable people inference", status, err)
	}
	if response.JSON200 == nil || response.JSON200.ConfiguredEnabled {
		return tui.PeopleInferenceStatus{}, errors.New("disable people inference: disabled status was not returned")
	}
	return tuiPeopleInferenceStatus(response.JSON200), nil
}

func (b *tuiDaemonSettingsBackend) RemovePeopleInferenceProfile(
	ctx context.Context, name, fingerprint string,
) (tui.PeopleInferenceStatus, error) {
	if fingerprint == "" {
		return tui.PeopleInferenceStatus{}, errors.New("people inference profile fingerprint is required")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	profile, etag, err := b.peopleInferenceProfileSnapshot(ctx, client, name)
	if err != nil {
		return tui.PeopleInferenceStatus{}, err
	}
	if profile.Fingerprint == nil || *profile.Fingerprint != fingerprint {
		return tui.PeopleInferenceStatus{}, errors.New("people inference profile changed; reload settings")
	}
	response, err := client.DeleteSettingsPeopleInferenceProviderWithResponse(ctx,
		&generated.DeleteSettingsPeopleInferenceProviderRequestOptions{
			PathParams: &generated.DeleteSettingsPeopleInferenceProviderPath{Name: name},
			Header:     &generated.DeleteSettingsPeopleInferenceProviderHeaders{IfMatch: etag},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return tui.PeopleInferenceStatus{}, &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("remove people inference profile", response.StatusCode, err)}
	}
	if response != nil && response.StatusCode == http.StatusConflict && response.JSON409 != nil &&
		response.JSON409.ErrorData == "provider_in_use" {
		return tui.PeopleInferenceStatus{}, errors.New("disable people inference or configure another profile before removing")
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.PeopleInferenceStatus{}, peopleInferenceHTTPError("remove people inference profile", status, err)
	}
	if response.JSON200 == nil {
		return tui.PeopleInferenceStatus{}, errors.New("remove people inference profile: empty response")
	}
	for _, latest := range response.JSON200.Profiles {
		if latest.Name == name {
			return tui.PeopleInferenceStatus{}, errors.New("remove people inference profile: profile is still present")
		}
	}
	return tuiPeopleInferenceStatus(response.JSON200), nil
}

func (b *tuiDaemonSettingsBackend) SelectCodexProfile(ctx context.Context, name string) error {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return err
	}
	etag, err := b.peopleInferenceETag(ctx, client)
	if err != nil {
		return err
	}
	response, err := client.SelectSettingsPeopleInferenceWithResponse(ctx,
		&generated.SelectSettingsPeopleInferenceRequestOptions{
			Header: &generated.SelectSettingsPeopleInferenceHeaders{IfMatch: etag},
			Body:   &generated.PeopleInferenceSelectionRequest{Name: name},
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("select people inference profile", response.StatusCode, err)}
	}
	if response != nil && response.StatusCode == http.StatusConflict && response.JSON409 != nil {
		switch response.JSON409.ErrorData {
		case "check_required":
			return errors.New("run an exact synthetic check before selecting")
		case "consent_required":
			return errors.New("grant exact people inference consent before selecting")
		}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return peopleInferenceHTTPError("select people inference profile", status, err)
	}
	return nil
}

func (b *tuiDaemonSettingsBackend) peopleInferenceClient() (*apiclient.Client, error) {
	if b == nil || b.client == nil {
		return nil, errors.New("daemon settings client unavailable")
	}
	return b.client.GeneratedClient()
}

func (b *tuiDaemonSettingsBackend) peopleInferenceETag(ctx context.Context, client *apiclient.Client) (string, error) {
	response, err := client.GetSettingsPeopleInferenceWithResponse(ctx)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return "", peopleInferenceHTTPError("load people inference config revision", status, err)
	}
	if response.Headers200 == nil || response.Headers200.ETag == "" {
		return "", errors.New("people inference status returned no config ETag")
	}
	return response.Headers200.ETag, nil
}

func (b *tuiDaemonSettingsBackend) peopleInferenceProfileSnapshot(
	ctx context.Context, client *apiclient.Client, name string,
) (generated.PeopleInferenceProfileSetting, string, error) {
	settings, etag, err := b.peopleInferenceSettingsSnapshot(ctx, client)
	if err != nil {
		return generated.PeopleInferenceProfileSetting{}, "", err
	}
	for _, profile := range settings.Profiles {
		if profile.Name == name {
			return profile, etag, nil
		}
	}
	return generated.PeopleInferenceProfileSetting{}, "", errors.New("people inference profile was not found")
}

func (b *tuiDaemonSettingsBackend) peopleInferenceSettingsSnapshot(
	ctx context.Context, client *apiclient.Client,
) (*generated.PeopleInferenceSettingsResponse, string, error) {
	response, err := client.GetSettingsPeopleInferenceWithResponse(ctx)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return nil, "", peopleInferenceHTTPError("load people inference status", status, err)
	}
	if response.JSON200 == nil || response.Headers200 == nil || response.Headers200.ETag == "" {
		return nil, "", errors.New("people inference status returned no body or config ETag")
	}
	return response.JSON200, response.Headers200.ETag, nil
}

func peopleInferenceDisclosureText(profile generated.PeopleInferenceProfileSetting) string {
	sourceDates := profile.SourceSince
	if profile.SourceUntil != nil && *profile.SourceUntil != "" {
		sourceDates += " to " + *profile.SourceUntil
	}
	sensitive := "no"
	if profile.AllowSensitive {
		sensitive = "yes"
	}
	lines := []string{
		"Provider: " + profile.Name,
		"Fingerprint: " + *profile.Fingerprint,
		"Protocol: " + profile.Protocol,
		"Model: " + profile.Model,
	}
	if profile.Endpoint != nil && *profile.Endpoint != "" {
		lines = append(lines, "Endpoint: "+*profile.Endpoint)
	}
	lines = append(lines,
		"Sources: "+strings.Join(profile.AllowedSources, ", "),
		"Source dates: "+sourceDates,
		"Sensitive content: "+sensitive,
		"Retention: "+profile.RetentionPosture,
		"Training: "+profile.TrainingPosture,
	)
	return strings.Join(lines, "\n")
}

func (b *tuiDaemonSettingsBackend) peopleInferenceCredentialRevision(
	ctx context.Context, client *apiclient.Client, name string,
) (string, error) {
	response, err := client.GetSettingsPeopleInferenceWithResponse(ctx)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return "", peopleInferenceHTTPError("load people provider credential revision", status, err)
	}
	if response.JSON200 == nil {
		return "", errors.New("load people provider credential revision: empty response")
	}
	for _, profile := range response.JSON200.Profiles {
		if profile.Name != name {
			continue
		}
		if profile.CredentialSource != "stored" || profile.PresetID == nil || *profile.PresetID == "" {
			return "", errors.New("people inference profile has no stored preset key")
		}
		if profile.CredentialRevision == nil || *profile.CredentialRevision == "" {
			return "", errors.New("people inference profile returned no credential revision")
		}
		return *profile.CredentialRevision, nil
	}
	return "", errors.New("people inference profile was not found")
}

func peopleInferenceHTTPError(operation string, status int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if status == 0 {
		return fmt.Errorf("%s: empty response", operation)
	}
	return fmt.Errorf("%s: daemon returned HTTP %d", operation, status)
}

func tuiPeopleInferenceStatus(response *generated.PeopleInferenceSettingsResponse) tui.PeopleInferenceStatus {
	status := tui.PeopleInferenceStatus{
		ConfiguredEnabled: response.ConfiguredEnabled, RunningEnabled: response.RunningEnabled,
		PendingRestart: response.PendingRestart,
	}
	if response.ConfiguredName != nil {
		status.Configured = *response.ConfiguredName
	}
	if response.ConfiguredFingerprint != nil {
		status.ConfiguredFingerprint = *response.ConfiguredFingerprint
	}
	if response.RunningName != nil {
		status.Running = *response.RunningName
	}
	if response.RunningFingerprint != nil {
		status.RunningFingerprint = *response.RunningFingerprint
	}
	return status
}
