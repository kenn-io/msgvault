package cmd

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/tui"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (b *tuiDaemonSettingsBackend) StartCodexLogin(ctx context.Context, name string) (tui.CodexDeviceLogin, error) {
	if name == "" {
		return tui.CodexDeviceLogin{}, errors.New("codex profile name is required")
	}
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.CodexDeviceLogin{}, err
	}
	response, err := client.StartSettingsPeopleCodexLoginWithResponse(ctx,
		&generated.StartSettingsPeopleCodexLoginRequestOptions{
			Body: &generated.PeopleCodexLoginRequest{Name: name},
		})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.CodexDeviceLogin{}, peopleInferenceHTTPError("start Codex device login", status, err)
	}
	if response.JSON200 == nil || response.JSON200.SessionID == "" ||
		response.JSON200.VerificationURL == "" || response.JSON200.UserCode == "" ||
		response.JSON200.LocalDeadline.IsZero() {
		return tui.CodexDeviceLogin{}, errors.New("start Codex device login: incomplete response")
	}
	return tui.CodexDeviceLogin{
		DraftID: response.JSON200.SessionID, SessionID: response.JSON200.SessionID,
		URL: response.JSON200.VerificationURL, Code: response.JSON200.UserCode,
		Deadline: response.JSON200.LocalDeadline,
	}, nil
}

func (b *tuiDaemonSettingsBackend) PollCodexLogin(ctx context.Context, session string) (tui.CodexLoginPoll, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return tui.CodexLoginPoll{}, err
	}
	response, err := client.GetSettingsPeopleCodexLoginWithResponse(ctx,
		&generated.GetSettingsPeopleCodexLoginRequestOptions{
			PathParams: &generated.GetSettingsPeopleCodexLoginPath{ID: session},
		})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return tui.CodexLoginPoll{}, peopleInferenceHTTPError("poll Codex device login", status, err)
	}
	if response.JSON200 == nil {
		return tui.CodexLoginPoll{}, errors.New("poll Codex device login: empty response")
	}
	switch response.JSON200.State {
	case "complete":
		return tui.CodexLoginPoll{Complete: true}, nil
	case "pending":
		return tui.CodexLoginPoll{}, nil
	case "failed":
		return tui.CodexLoginPoll{}, errors.New("codex device login failed")
	case "cancelled":
		return tui.CodexLoginPoll{}, errors.New("codex device login was cancelled")
	default:
		return tui.CodexLoginPoll{}, errors.New("codex device login returned an unknown state")
	}
}

func (b *tuiDaemonSettingsBackend) CancelCodexLogin(ctx context.Context, session string) error {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return err
	}
	response, err := client.CancelSettingsPeopleCodexLoginWithResponse(ctx,
		&generated.CancelSettingsPeopleCodexLoginRequestOptions{
			PathParams: &generated.CancelSettingsPeopleCodexLoginPath{ID: session},
		})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return peopleInferenceHTTPError("cancel Codex device login", status, err)
	}
	if response.JSON200 == nil || response.JSON200.State != "cancelled" {
		return errors.New("cancel Codex device login: cancellation was not confirmed")
	}
	return nil
}

func (b *tuiDaemonSettingsBackend) ListCodexModels(ctx context.Context, session string) ([]tui.CodexModelChoice, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return nil, err
	}
	response, err := client.GetSettingsPeopleCodexModelsWithResponse(ctx,
		&generated.GetSettingsPeopleCodexModelsRequestOptions{
			PathParams: &generated.GetSettingsPeopleCodexModelsPath{ID: session},
		})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return nil, peopleInferenceHTTPError("list Codex models", status, err)
	}
	if response.JSON200 == nil {
		return nil, errors.New("list Codex models: empty response")
	}
	models := make([]tui.CodexModelChoice, 0, len(response.JSON200.Models))
	for _, model := range response.JSON200.Models {
		if model.ID != "" {
			models = append(models, tui.CodexModelChoice{
				ID: model.ID, DefaultReasoningEffort: model.DefaultReasoningEffort,
				ReasoningEfforts: append([]string(nil), model.SupportedEfforts...),
			})
		}
	}
	return models, nil
}

func (b *tuiDaemonSettingsBackend) SaveCodexProfile(
	ctx context.Context, session string, request tui.CodexProfileRequest,
) (string, error) {
	client, err := b.peopleInferenceClient()
	if err != nil {
		return "", err
	}
	etag, err := b.peopleInferenceETag(ctx, client)
	if err != nil {
		return "", err
	}
	body := generated.PeopleCodexProfileRequest{
		Model: request.Model, ReasoningEffort: request.ReasoningEffort,
		RetentionPosture: request.RetentionPosture, TrainingPosture: request.TrainingPosture,
		AllowedSources: request.AllowedSources, SourceSince: request.SourceSince,
		AllowSensitive: request.AllowSensitive,
	}
	if request.SourceUntil != "" {
		body.SourceUntil = &request.SourceUntil
	}
	response, err := client.PutSettingsPeopleCodexProfileWithResponse(ctx,
		&generated.PutSettingsPeopleCodexProfileRequestOptions{
			PathParams: &generated.PutSettingsPeopleCodexProfilePath{ID: session},
			Header:     &generated.PutSettingsPeopleCodexProfileHeaders{IfMatch: etag},
			Body:       &body,
		})
	if response != nil && response.StatusCode == http.StatusPreconditionFailed {
		return "", &tui.SettingsConflictError{Scope: tui.SettingsConflictConfig,
			Err: peopleInferenceHTTPError("save Codex profile", response.StatusCode, err)}
	}
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		return "", peopleInferenceHTTPError("save Codex profile", status, err)
	}
	if response.JSON200 == nil {
		return "", errors.New("save Codex profile: empty response")
	}
	for _, profile := range response.JSON200.Profiles {
		if profile.Name == request.Name && profile.Protocol == "codex_app_server" && profile.Model == request.Model {
			return profile.Name, nil
		}
	}
	return "", errors.New("save Codex profile: profile was not returned")
}
