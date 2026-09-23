package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/tui"
)

func TestTUICodexAdapterUsesGeneratedLoginAndProfileRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []string
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/settings/people-inference/codex/login":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(map[string]any{"name": "codex-custom"}, body)
			_, _ = io.WriteString(w, `{"session_id":"session-1","verification_url":"https://example.test/device","user_code":"ABCD-EFGH","local_deadline":"2026-09-23T12:05:00Z"}`)
		case "GET /api/v1/settings/people-inference/codex/login/session-1":
			polls++
			state := "pending"
			if polls == 2 {
				state = "complete"
			}
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		case "GET /api/v1/settings/people-inference/codex/login/session-1/models":
			_, _ = io.WriteString(w, `{"models":[{"id":"codex-model-a","display_name":"Model A","default_reasoning_effort":"medium","supported_efforts":["low","medium"]}]}`)
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[]}`)
		case "PUT /api/v1/settings/people-inference/codex/login/session-1/profile":
			assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(map[string]any{
				"model": "codex-model-a", "reasoning_effort": "low", "retention_posture": "operator assertion: no retention",
				"training_posture": "operator assertion: no training", "allowed_sources": []any{"conversation_text"},
				"source_since": "2025-01-01", "source_until": "2025-12-31", "allow_sensitive": false,
			}, body)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-custom","protocol":"codex_app_server","model":"codex-model-a","output_mode":"native_json_schema","credential_source":"none","allowed_sources":["conversation_text"],"source_since":"2025-01-01","retention_posture":"operator assertion: no retention","training_posture":"operator assertion: no training"}]}`)
		case "DELETE /api/v1/settings/people-inference/codex/login/session-1":
			_, _ = io.WriteString(w, `{"state":"cancelled"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	login, err := backend.StartCodexLogin(context.Background(), "codex-custom")
	require.NoError(err)
	assert.Equal("session-1", login.DraftID)
	assert.Equal("session-1", login.SessionID)
	assert.Equal("https://example.test/device", login.URL)
	assert.Equal("ABCD-EFGH", login.Code)
	assert.Equal(time.Date(2026, 9, 23, 12, 5, 0, 0, time.UTC), login.Deadline)
	poll, err := backend.PollCodexLogin(context.Background(), login.SessionID)
	require.NoError(err)
	assert.False(poll.Complete)
	poll, err = backend.PollCodexLogin(context.Background(), login.SessionID)
	require.NoError(err)
	assert.True(poll.Complete)
	models, err := backend.ListCodexModels(context.Background(), login.SessionID)
	require.NoError(err)
	assert.Equal([]tui.CodexModelChoice{{ID: "codex-model-a", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"low", "medium"}}}, models)
	profile, err := backend.SaveCodexProfile(context.Background(), login.SessionID, tui.CodexProfileRequest{
		Name: "codex-custom", Model: models[0].ID, ReasoningEffort: "low",
		RetentionPosture: "operator assertion: no retention", TrainingPosture: "operator assertion: no training",
		AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01", SourceUntil: "2025-12-31",
		AllowSensitive: false,
	})
	require.NoError(err)
	assert.Equal("codex-custom", profile)
	require.NoError(backend.CancelCodexLogin(context.Background(), login.SessionID))
	assert.Equal([]string{
		"POST /api/v1/settings/people-inference/codex/login",
		"GET /api/v1/settings/people-inference/codex/login/session-1",
		"GET /api/v1/settings/people-inference/codex/login/session-1",
		"GET /api/v1/settings/people-inference/codex/login/session-1/models",
		"GET /api/v1/settings/people-inference",
		"PUT /api/v1/settings/people-inference/codex/login/session-1/profile",
		"DELETE /api/v1/settings/people-inference/codex/login/session-1",
	}, calls)
}

func TestTUICodexProfileSaveReportsConfigConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[]}`)
		case "PUT /api/v1/settings/people-inference/codex/login/session-1/profile":
			assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `{"error":"settings_conflict"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
	_, err := backend.SaveCodexProfile(context.Background(), "session-1", tui.CodexProfileRequest{
		Name: "codex-custom", Model: "codex-model-a", ReasoningEffort: "low",
		RetentionPosture: "operator assertion", TrainingPosture: "operator assertion",
		AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01",
	})
	require.Error(err)
	var conflict *tui.SettingsConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
}

func TestTUIPeopleInferenceBackendUsesGeneratedStatusAndPresetRoutes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[],"configured_name":"old","configured_fingerprint":"fp-configured","running_name":"old","running_fingerprint":"fp-running","configured_enabled":true,"running_enabled":true,"pending_restart":false}`)
		case "PUT /api/v1/settings/people-inference/providers/router-primary":
			assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal("openrouter", body["preset_id"])
			assert.Equal("example/model", body["model"])
			assert.Equal(false, body["allow_sensitive"])
			assert.NotContains(body, "endpoint")
			assert.NotContains(body, "key")
			w.Header().Set("ETag", `"config-2"`)
			_, _ = io.WriteString(w, `{"profiles":[],"configured_name":"old","running_name":"old","pending_restart":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	status, err := backend.LoadPeopleInferenceStatus(context.Background())
	require.NoError(err)
	assert.Equal("old", status.Configured)
	assert.Equal("old", status.Running)
	assert.Equal("fp-configured", status.ConfiguredFingerprint)
	assert.Equal("fp-running", status.RunningFingerprint)
	assert.True(status.ConfiguredEnabled)
	assert.True(status.RunningEnabled)
	assert.False(status.PendingRestart)

	status, err = backend.CreatePeopleInferencePreset(context.Background(), "router-primary", tui.PeopleInferencePresetRequest{
		PresetID: "openrouter", Model: "example/model", RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01",
		AllowSensitive: false,
	})
	require.NoError(err)
	assert.True(status.PendingRestart)
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"GET /api/v1/settings/people-inference",
		"PUT /api/v1/settings/people-inference/providers/router-primary",
	}, calls)
}

func TestTUIPeopleInferenceBackendCheckUsesExactProfile(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","protocol":"codex_app_server","model":"codex-model","endpoint":"https://api.example.test/v1","allowed_sources":["conversation_text"],"source_since":"2025-01-01","source_until":"2025-12-31","allow_sensitive":false,"retention_posture":"operator-confirmed","training_posture":"operator-confirmed","checked":true}]}`)
		case "POST /api/v1/settings/people-inference/providers/codex-profile/check":
			assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
			_, _ = io.WriteString(w, `{"ok":true,"fingerprint":"fp-1","model":"codex-model","usage":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	disclosure, err := backend.CheckCodexProfile(context.Background(), "codex-profile")
	require.NoError(err)
	assert.Equal("codex-profile", disclosure.Profile)
	assert.Equal("fp-1", disclosure.Fingerprint)
	for _, field := range []string{"fp-1", "codex_app_server", "codex-model", "https://api.example.test/v1", "conversation_text", "2025-01-01", "2025-12-31", "operator-confirmed", "Sensitive content: no"} {
		assert.Contains(disclosure.Text, field)
	}
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"POST /api/v1/settings/people-inference/providers/codex-profile/check",
		"GET /api/v1/settings/people-inference",
	}, calls)
}

func TestTUIPeopleInferenceBackendCheckRejectsChangedFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-new","model":"codex-model"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"fingerprint":"fp-old","model":"codex-model","usage":{}}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.CheckCodexProfile(context.Background(), "codex-profile")
	require.Error(err)
	assert.Contains(err.Error(), "changed")
}

func TestTUIPeopleInferenceBackendConsentSendsExactConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-2"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","checked":true}]}`)
		case "POST /api/v1/settings/people-inference/providers/codex-profile/consent":
			assert.Equal(`"config-2"`, r.Header.Get("If-Match"))
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(map[string]any{"confirmed": true, "fingerprint": "fp-1"}, body)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","consent_active":true}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	require.NoError(backend.ConsentCodexProfile(context.Background(), "codex-profile", "fp-1"))
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"POST /api/v1/settings/people-inference/providers/codex-profile/consent",
	}, calls)
}

func TestTUIPeopleInferenceBackendConsentRejectsStaleFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"config-2"`)
		_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-new","checked":true}]}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	err := backend.ConsentCodexProfile(context.Background(), "codex-profile", "fp-old")
	require.Error(err)
	assert.Contains(err.Error(), "changed")
	assert.Equal([]string{"GET /api/v1/settings/people-inference"}, calls)
}

func TestTUIPeopleInferenceBackendRevokeUsesExactFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-3"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","consent_active":true}],"configured_name":"codex-profile","configured_fingerprint":"fp-1"}`)
		case "POST /api/v1/settings/people-inference/providers/codex-profile/revoke":
			assert.Equal(`"config-3"`, r.Header.Get("If-Match"))
			_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","consent_active":false}],"configured_name":"codex-profile","configured_fingerprint":"fp-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	status, err := backend.RevokePeopleInferenceConsent(context.Background(), "codex-profile", "fp-1")
	require.NoError(err)
	assert.Equal("fp-1", status.ConfiguredFingerprint)
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"POST /api/v1/settings/people-inference/providers/codex-profile/revoke",
	}, calls)
}

func TestTUIPeopleInferenceBackendDisableUsesExactFingerprint(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-3"`)
			_, _ = io.WriteString(w, `{"profiles":[],"configured_name":"codex-profile","configured_fingerprint":"fp-1","configured_enabled":true,"running_enabled":true}`)
		case "POST /api/v1/settings/people-inference/disable":
			assert.Equal(`"config-3"`, r.Header.Get("If-Match"))
			_, _ = io.WriteString(w, `{"profiles":[],"configured_name":"codex-profile","configured_fingerprint":"fp-1","configured_enabled":false,"running_enabled":true,"pending_restart":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	status, err := backend.DisablePeopleInference(context.Background(), "fp-1")
	require.NoError(err)
	assert.False(status.ConfiguredEnabled)
	assert.True(status.PendingRestart)
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"POST /api/v1/settings/people-inference/disable",
	}, calls)
}

func TestTUIPeopleInferenceBackendRemoveUsesExactFingerprint(t *testing.T) {
	require := require.New(t)

	assert := assert.New(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-4"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"router","fingerprint":"fp-router"},{"name":"backup","fingerprint":"fp-backup"}],"configured_name":"router","configured_fingerprint":"fp-router","configured_enabled":false}`)
		case "DELETE /api/v1/settings/people-inference/providers/router":
			assert.Equal(`"config-4"`, r.Header.Get("If-Match"))
			_, _ = io.WriteString(w, `{"profiles":[{"name":"backup","fingerprint":"fp-backup"}],"configured_name":"backup","configured_fingerprint":"fp-backup","pending_restart":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	status, err := backend.RemovePeopleInferenceProfile(context.Background(), "router", "fp-router")
	require.NoError(err)
	assert.Equal("backup", status.Configured)
	assert.True(status.PendingRestart)
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"DELETE /api/v1/settings/people-inference/providers/router",
	}, calls)
}

func TestTUIPeopleInferenceBackendRemoveRejectsChangedFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"config-new"`)
		_, _ = io.WriteString(w, `{"profiles":[{"name":"router","fingerprint":"fp-new"}],"configured_enabled":false}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.RemovePeopleInferenceProfile(context.Background(), "router", "fp-old")
	require.Error(err)
	assert.Contains(err.Error(), "changed")
	assert.Equal([]string{"GET /api/v1/settings/people-inference"}, calls)
}

func TestTUIPeopleInferenceBackendRemoveReportsConflict(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", `"config-4"`)
					_, _ = io.WriteString(w, `{"profiles":[{"name":"router","fingerprint":"fp-router"}],"configured_enabled":false}`)
					return
				}
				assert.Equal(`"config-4"`, r.Header.Get("If-Match"))
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"provider_in_use"}`)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

			_, err := backend.RemovePeopleInferenceProfile(context.Background(), "router", "fp-router")
			require.Error(err)
			if status == http.StatusPreconditionFailed {
				var conflict *tui.SettingsConflictError
				require.ErrorAs(err, &conflict)
				assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
			} else {
				assert.Contains(err.Error(), "another profile")
			}
		})
	}
}

func TestTUIPeopleInferenceBackendRevokeAndDisableRejectStaleFingerprint(t *testing.T) {
	for _, operation := range []string{"revoke", "disable"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", `"config-new"`)
				_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-new"}],"configured_fingerprint":"fp-new","configured_enabled":true}`)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
			var err error
			if operation == "revoke" {
				_, err = backend.RevokePeopleInferenceConsent(context.Background(), "codex-profile", "fp-old")
			} else {
				_, err = backend.DisablePeopleInference(context.Background(), "fp-old")
			}
			require.Error(err)
			assert.Contains(err.Error(), "changed")
			assert.Equal([]string{"GET /api/v1/settings/people-inference"}, calls)
		})
	}
}

func TestTUIPeopleInferenceBackendRevokeAndDisablePreserveConfigConflict(t *testing.T) {
	for _, operation := range []string{"revoke", "disable"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", `"config-old"`)
					_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1"}],"configured_fingerprint":"fp-1","configured_enabled":true}`)
					return
				}
				assert.Equal(`"config-old"`, r.Header.Get("If-Match"))
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `{"error":"settings_conflict"}`)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
			var err error
			if operation == "revoke" {
				_, err = backend.RevokePeopleInferenceConsent(context.Background(), "codex-profile", "fp-1")
			} else {
				_, err = backend.DisablePeopleInference(context.Background(), "fp-1")
			}
			var conflict *tui.SettingsConflictError
			require.ErrorAs(err, &conflict)
			assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
		})
	}
}

func TestTUIPeopleInferenceBackendCheckAndConsentConflicts(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		status    int
		body      string
		want      string
		conflict  bool
	}{
		{name: "check config conflict", operation: "check", status: 412, body: `{"error":"settings_conflict"}`, conflict: true},
		{name: "consent config conflict", operation: "consent", status: 412, body: `{"error":"settings_conflict"}`, conflict: true},
		{name: "consent changed disclosure", operation: "consent", status: 409, body: `{"error":"consent_disclosure_changed"}`, want: "disclosure changed"},
		{name: "consent check missing", operation: "consent", status: 409, body: `{"error":"check_required"}`, want: "synthetic check"},
		{name: "check daemon error", operation: "check", status: 500, body: `{"error":"settings_read_failed"}`, want: "500"},
		{name: "consent daemon error", operation: "consent", status: 500, body: `{"error":"settings_read_failed"}`, want: "500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", `"config-1"`)
					_, _ = io.WriteString(w, `{"profiles":[{"name":"codex-profile","fingerprint":"fp-1","checked":true}]}`)
					return
				}
				assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
			var err error
			if test.operation == "check" {
				_, err = backend.CheckCodexProfile(context.Background(), "codex-profile")
			} else {
				err = backend.ConsentCodexProfile(context.Background(), "codex-profile", "fp-1")
			}
			require.Error(err)
			if test.conflict {
				var conflict *tui.SettingsConflictError
				require.ErrorAs(err, &conflict)
				assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
			} else {
				assert.Contains(err.Error(), test.want)
			}
		})
	}
}

func TestTUIPeopleInferenceBackendSelectPreservesConfigConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-old"`)
			_, _ = io.WriteString(w, `{"profiles":[],"pending_restart":false}`)
		case "POST /api/v1/settings/people-inference/select":
			assert.Equal(`"config-old"`, r.Header.Get("If-Match"))
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal("codex-profile", body["name"])
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	err := backend.SelectCodexProfile(context.Background(), "codex-profile")
	var conflict *tui.SettingsConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
}

func TestTUIPeopleInferenceBackendCreatePreservesConfigConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"config-old"`)
			_, _ = io.WriteString(w, `{"profiles":[]}`)
			return
		}
		assert.Equal(`"config-old"`, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.CreatePeopleInferencePreset(context.Background(), "router", tui.PeopleInferencePresetRequest{
		PresetID: "openrouter", Model: "example/model", RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01",
	})
	var conflict *tui.SettingsConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(tui.SettingsConflictConfig, conflict.Scope)
}

func TestTUIPeopleInferenceBackendSelectReportsDaemonGate(t *testing.T) {
	for _, test := range []struct {
		code string
		want string
	}{
		{code: "check_required", want: "run an exact synthetic check before selecting"},
		{code: "consent_required", want: "grant exact people inference consent before selecting"},
	} {
		t.Run(test.code, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", `"config-1"`)
					_, _ = io.WriteString(w, `{"profiles":[]}`)
					return
				}
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"`+test.code+`"}`)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

			err := backend.SelectCodexProfile(context.Background(), "codex-profile")
			require.Error(err)
			assert.Equal(test.want, err.Error())
		})
	}
}

func TestTUIPeopleInferenceBackendRejectsServerFailure(t *testing.T) {
	for _, operation := range []string{"status", "create", "select"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if operation != "status" && r.Method == http.MethodGet {
					w.Header().Set("ETag", `"config-1"`)
					_, _ = io.WriteString(w, `{"profiles":[]}`)
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"settings_read_failed"}`)
			}))
			t.Cleanup(server.Close)
			backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
			var err error
			switch operation {
			case "status":
				_, err = backend.LoadPeopleInferenceStatus(context.Background())
			case "create":
				_, err = backend.CreatePeopleInferencePreset(context.Background(), "router", tui.PeopleInferencePresetRequest{
					PresetID: "openrouter", Model: "example/model", RetentionPosture: "zero_retention",
					TrainingPosture: "no_training", AllowedSources: []string{"conversation_text"}, SourceSince: "2025-01-01",
				})
			case "select":
				err = backend.SelectCodexProfile(context.Background(), "codex-profile")
			}
			require.Error(err)
			assert.Contains(err.Error(), "500")
		})
	}
}

func TestTUIPeopleInferenceBackendKeyWriteUsesProfileCredentialRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const secret = "synthetic-key-value"
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-not-a-credential-revision"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"router","credential_source":"stored","preset_id":"openrouter","credential_revision":"credential-rev-1"}],"pending_restart":false}`)
		case "PUT /api/v1/settings/people-inference/providers/router/key":
			assert.Equal("credential-rev-1", r.Header.Get("If-Match"))
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			assert.Equal(map[string]any{"value": secret}, body)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"router","credential_source":"stored","credential_configured":true,"credential_revision":"credential-rev-2"}],"pending_restart":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	status, err := backend.SetPeopleInferenceKey(context.Background(), "router", secret)
	require.NoError(err)
	assert.False(status.PendingRestart)
	assert.Equal([]string{
		"GET /api/v1/settings/people-inference",
		"PUT /api/v1/settings/people-inference/providers/router/key",
	}, calls)
}

func TestTUIPeopleInferenceBackendKeyConflictUsesPeopleCredentialScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const secret = "synthetic-key-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Header().Set("ETag", `"config-1"`)
			_, _ = io.WriteString(w, `{"profiles":[{"name":"router","credential_source":"stored","preset_id":"openrouter","credential_revision":"credential-rev-1"}]}`)
			return
		}
		assert.Equal("credential-rev-1", r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"error":"credential_conflict"}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.SetPeopleInferenceKey(context.Background(), "router", secret)
	var conflict *tui.SettingsConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(tui.SettingsConflictPeopleCredentials, conflict.Scope)
	assert.NotContains(err.Error(), secret)
}

func TestTUIPeopleInferenceBackendKeyWriteRequiresProfileRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"config-revision-only"`)
		_, _ = io.WriteString(w, `{"profiles":[{"name":"router","credential_source":"stored","preset_id":"openrouter"}]}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.SetPeopleInferenceKey(context.Background(), "router", "synthetic-key-value")
	require.Error(err)
	assert.Contains(err.Error(), "credential revision")
	assert.Equal([]string{"GET /api/v1/settings/people-inference"}, calls)
}

func TestTUIPeopleInferenceBackendKeyWriteReportsChangedDestination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"profiles":[{"name":"router","credential_source":"stored","preset_id":"openrouter","credential_revision":"credential-rev-1"}]}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"provider_binding_changed"}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.SetPeopleInferenceKey(context.Background(), "router", "synthetic-key-value")
	require.Error(err)
	assert.Equal("provider destination changed; reload settings", err.Error())
}
