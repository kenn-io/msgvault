package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/tui"
)

func TestPersonProviderCodexEnrollRejectsNoninteractiveBeforeLogin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opened := false
	command := newPersonProviderCodexEnrollCommand(codexEnrollDeps{
		isTerminal: func(*cobra.Command) bool { return false },
		openBackend: func(context.Context) (tui.PeopleInferenceBackend, func(), error) {
			opened = true
			return nil, nil, nil
		},
	})
	command.SetArgs([]string{
		"codex-profile", "--source", "conversation_text", "--source-since", "2025-01-01",
		"--retention-posture", "operator assertion", "--training-posture", "operator assertion",
		"--allow-sensitive=false", "--yes",
	})
	command.SetIn(strings.NewReader(""))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "terminal")
	assert.False(opened)
}

func TestPersonProviderCodexEnrollRejectsBlankPolicyBeforeLogin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opened := false
	command := newPersonProviderCodexEnrollCommand(codexEnrollDeps{
		isTerminal: func(*cobra.Command) bool { return true },
		openBackend: func(context.Context) (tui.PeopleInferenceBackend, func(), error) {
			opened = true
			return nil, nil, nil
		},
	})
	command.SetArgs([]string{
		"codex-profile", "--source", "conversation_text", "--source-since", "2025-01-01",
		"--retention-posture", " ", "--training-posture", "operator assertion", "--allow-sensitive=false",
	})
	command.SetIn(strings.NewReader(""))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	err := command.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "retention-posture")
	assert.False(opened)
}

func TestPersonProviderCodexEnrollCompletesDaemonJourney(t *testing.T) {
	testPersonProviderCodexEnrollDaemonJourney(t, "y\n", true)
}

func TestPersonProviderCodexEnrollDeclinedConsentLeavesSavedProfileUnselected(t *testing.T) {
	testPersonProviderCodexEnrollDaemonJourney(t, "n\n", false)
}

func testPersonProviderCodexEnrollDaemonJourney(t *testing.T, consentAnswer string, wantSelected bool) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	var calls []string
	saved, checked, consented, selected := false, false, false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writeStatus := func() {
			profiles := []map[string]any{}
			if saved {
				profiles = append(profiles, map[string]any{
					"name": "codex-profile", "protocol": "codex_app_server", "model": "gpt-test",
					"output_mode": "native_json_schema", "credential_source": "none",
					"allowed_sources": []string{"conversation_text"}, "source_since": "2025-01-01",
					"retention_posture": "operator assertion: no retention",
					"training_posture":  "operator assertion: no training",
					"fingerprint":       "fp-1", "checked": checked, "consent_active": consented,
				})
			}
			status := map[string]any{"profiles": profiles, "running_name": "old-profile", "pending_restart": selected}
			if selected {
				status["configured_name"] = "codex-profile"
			}
			assert.NoError(json.NewEncoder(w).Encode(status))
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/settings/people-inference/codex/login":
			var body map[string]any
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("codex-profile", body["name"])
			_, _ = w.Write([]byte(`{"session_id":"session-1","verification_url":"https://example.test/device","user_code":"ABCD-EFGH","local_deadline":"2099-09-23T12:05:00Z"}`))
		case "GET /api/v1/settings/people-inference/codex/login/session-1":
			_, _ = w.Write([]byte(`{"state":"complete"}`))
		case "GET /api/v1/settings/people-inference/codex/login/session-1/models":
			_, _ = w.Write([]byte(`{"models":[{"id":"gpt-test","display_name":"Test model","default_reasoning_effort":"medium","supported_efforts":["low","medium"]}]}`))
		case "GET /api/v1/settings/people-inference":
			w.Header().Set("ETag", `"config-1"`)
			writeStatus()
		case "PUT /api/v1/settings/people-inference/codex/login/session-1/profile":
			assert.Equal(`"config-1"`, r.Header.Get("If-Match"))
			var body map[string]any
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("gpt-test", body["model"])
			assert.Equal("low", body["reasoning_effort"])
			assert.Equal(false, body["allow_sensitive"])
			saved = true
			writeStatus()
		case "POST /api/v1/settings/people-inference/providers/codex-profile/check":
			checked = true
			_, _ = w.Write([]byte(`{"ok":true,"fingerprint":"fp-1","model":"gpt-test","usage":{}}`))
		case "POST /api/v1/settings/people-inference/providers/codex-profile/consent":
			var body map[string]any
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("fp-1", body["fingerprint"])
			assert.Equal(true, body["confirmed"])
			consented = true
			writeStatus()
		case "POST /api/v1/settings/people-inference/select":
			var body map[string]any
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal("codex-profile", body["name"])
			selected = true
			writeStatus()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
	command := newPersonProviderCodexEnrollCommand(codexEnrollDeps{
		isTerminal: func(*cobra.Command) bool { return true },
		openBackend: func(context.Context) (tui.PeopleInferenceBackend, func(), error) {
			return backend, func() {}, nil
		},
		pollInterval: time.Millisecond,
	})
	command.SetArgs([]string{
		"codex-profile", "--source", "conversation_text", "--source-since", "2025-01-01",
		"--retention-posture", "operator assertion: no retention", "--training-posture", "operator assertion: no training",
		"--allow-sensitive=false", "--model", "gpt-test", "--reasoning-effort", "low",
	})
	command.SetIn(strings.NewReader(consentAnswer))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	err := command.Execute()
	if wantSelected {
		require.NoError(err)
	} else {
		require.ErrorContains(err, "consent declined; profile was saved but not selected")
	}
	assert.True(saved)
	assert.True(checked)
	assert.Equal(wantSelected, consented)
	assert.Equal(wantSelected, selected)
	assert.Contains(output.String(), "https://example.test/device")
	assert.Contains(output.String(), "ABCD-EFGH")
	assert.Contains(output.String(), "fp-1")
	assert.Contains(output.String(), "codex-profile")
	assert.NotContains(strings.Join(calls, "\n"), "DELETE /api/v1/settings/people-inference/codex/login/session-1")
	wantCalls := []string{
		"POST /api/v1/settings/people-inference/codex/login",
		"GET /api/v1/settings/people-inference/codex/login/session-1",
		"GET /api/v1/settings/people-inference/codex/login/session-1/models",
		"GET /api/v1/settings/people-inference",
		"PUT /api/v1/settings/people-inference/codex/login/session-1/profile",
		"GET /api/v1/settings/people-inference",
		"POST /api/v1/settings/people-inference/providers/codex-profile/check",
		"GET /api/v1/settings/people-inference",
	}
	if wantSelected {
		wantCalls = append(wantCalls,
			"GET /api/v1/settings/people-inference",
			"POST /api/v1/settings/people-inference/providers/codex-profile/consent",
			"GET /api/v1/settings/people-inference",
			"POST /api/v1/settings/people-inference/select",
			"GET /api/v1/settings/people-inference",
		)
	} else {
		assert.NotContains(strings.Join(calls, "\n"), "POST /api/v1/settings/people-inference/providers/codex-profile/consent")
		assert.NotContains(strings.Join(calls, "\n"), "POST /api/v1/settings/people-inference/select")
	}
	assert.Equal(wantCalls, calls)
}

func TestChooseCodexEnrollmentModelPromptsForAvailableChoice(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	models := []tui.CodexModelChoice{{ID: "gpt-test", DefaultReasoningEffort: "medium", ReasoningEfforts: []string{"low", "medium"}}}
	var output bytes.Buffer
	model, effort, err := chooseCodexEnrollmentModel(&output, bufio.NewReader(strings.NewReader("gpt-test\nlow\n")), models, "", "")
	require.NoError(err)
	assert.Equal("gpt-test", model)
	assert.Equal("low", effort)
	assert.Contains(output.String(), "gpt-test")
	assert.Contains(output.String(), "low, medium")
}
