package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGetSettingsUsesAllowlistETagAndSecretStates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _ := newSettingsTestServer(t, "# keep\n[web]\ntheme = \"dark\"\n"+
		"[server]\napi_key = \"test-api-key\"\n"+
		"[vector.embeddings]\ndocument_prefix = \"search_document: \"\nquery_prefix = \"search_query: \"\n"+
		"[integrations.tasks]\napi_key = \"task-secret\"\n"+
		"[unsupported]\nprivate_value = \"must-not-leak\"\n")
	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "test-api-key")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assertions.NotEmpty(resp.Header().Get("ETag"))
	assertions.Equal("no-store", resp.Header().Get("Cache-Control"))

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirements.NotNil(byKey["web.theme"].Value)
	requirements.NotNil(byKey["web.theme"].Value.String)
	assertions.Equal("dark", *byKey["web.theme"].Value.String)
	assertions.Equal(&SecretSettingState{Configured: true}, byKey["server.api_key"].Secret)
	assertions.Nil(byKey["server.api_key"].Value)
	assertions.Equal(&SecretSettingState{Configured: true}, byKey["integrations.tasks.api_key"].Secret)
	requirements.NotNil(byKey["vector.embeddings.api_format"].Value)
	requirements.NotNil(byKey["vector.embeddings.api_format"].Value.String)
	assertions.Equal("openai", *byKey["vector.embeddings.api_format"].Value.String)
	assertions.Equal([]string{"openai", "voyage-contextual"}, byKey["vector.embeddings.api_format"].Options)
	requirements.NotNil(byKey["vector.embeddings.document_prefix"].Value)
	requirements.NotNil(byKey["vector.embeddings.document_prefix"].Value.String)
	assertions.Equal("search_document: ", *byKey["vector.embeddings.document_prefix"].Value.String)
	requirements.NotNil(byKey["vector.embeddings.query_prefix"].Value)
	requirements.NotNil(byKey["vector.embeddings.query_prefix"].Value.String)
	assertions.Equal("search_query: ", *byKey["vector.embeddings.query_prefix"].Value.String)
	requirements.NotNil(byKey["vector.people.enabled"].Value)
	requirements.NotNil(byKey["vector.people.enabled"].Value.Boolean)
	assertions.False(*byKey["vector.people.enabled"].Value.Boolean)
	requirements.NotNil(byKey["vector.people.retention_posture"].Value)
	requirements.NotNil(byKey["vector.people.training_posture"].Value)
	requirements.NotNil(byKey["server.trusted_proxies"].Value)
	assertions.NotNil(byKey["server.trusted_proxies"].Value.Strings)
	assertions.NotContains(byKey, "unsupported.private_value")
	for _, setting := range body.Settings {
		wantRestartRequired := !strings.HasPrefix(setting.Key, "web.") && setting.Key != "carddav.password"
		assertions.Equal(wantRestartRequired, setting.RestartRequired, setting.Key)
		wantReadOnly := strings.HasPrefix(setting.Key, "server.bind_addr") ||
			setting.Key == "server.api_port" ||
			setting.Key == "server.api_key" ||
			setting.Key == "server.allow_insecure" ||
			setting.Key == "server.trusted_proxies" ||
			setting.Key == "vector.backend" ||
			setting.Key == "vector.db_path" ||
			setting.Key == "vector.skip_extension_create" ||
			setting.Key == "vector.embeddings.api_key_env" ||
			setting.Key == "vector.multimodal.api_key_env" ||
			setting.Key == "vector.multimodal.capabilities_file" ||
			strings.HasPrefix(setting.Key, "carddav.")
		assertions.Equal(wantReadOnly, setting.ReadOnly, setting.Key)
	}
	assertions.NotContains(resp.Body.String(), "test-api-key")
	assertions.NotContains(resp.Body.String(), "task-secret")
	assertions.NotContains(resp.Body.String(), "must-not-leak")
}

func TestGetSettingsIsSelfDescribingAndIncludesSafeCatalog(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, "")

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body struct {
		Groups   []map[string]any `json:"groups"`
		Settings []map[string]any `json:"settings"`
	}
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	requirements.NotEmpty(body.Groups, "the daemon must describe category labels for generic clients")

	byKey := make(map[string]map[string]any, len(body.Settings))
	for _, setting := range body.Settings {
		key, ok := setting["key"].(string)
		requirements.True(ok)
		assertions.NotEmpty(setting["label"], "setting %s must have a stable label", key)
		assertions.NotEmpty(setting["description"], "setting %s must have a stable description", key)
		byKey[key] = setting
	}
	for _, key := range []string{
		"sync.rate_limit_qps",
		"log.enabled", "log.level", "log.sql_slow_ms", "log.sql_trace",
		"analytics.min_rebuild_interval", "analytics.builder_memory_limit",
		"analytics.builder_threads", "analytics.builder_temp_limit",
		"server.daemon_idle_timeout", "server.daemon_auto_restart",
		"activity.timezone", "activity.max_direct_counterparts", "activity.batch_size", "activity.schedule",
		"backup.zstd_level",
		"beeper.accounts", "beeper.exclude_accounts", "beeper.rate_limit_qps",
		"beeper.media", "beeper.media_scope", "beeper.media_max_participants", "beeper.max_media_mb",
		"slack.enabled", "slack.schedule", "slack.channels", "slack.exclude_channels",
		"slack.media", "slack.media_scope", "slack.media_max_participants", "slack.max_media_mb",
		"discord.media", "discord.media_scope", "discord.media_max_participants", "discord.max_media_mb",
		"teams.media", "teams.media_scope", "teams.media_max_participants", "teams.max_media_mb",
		"vector.embeddings.timeout", "vector.preprocess.strip_quotes", "vector.preprocess.strip_signatures",
		"vector.preprocess.strip_html", "vector.preprocess.strip_base64",
		"vector.preprocess.strip_url_tracking", "vector.preprocess.collapse_whitespace",
		"vector.search.max_page_size_hybrid", "vector.embed.backstop_interval",
		"vector.embeddings.api_key", "vector.multimodal.api_key",
		"people.enrichment.enabled", "people.enrichment.schedule", "people.enrichment.batch_size",
		"people.enrichment.lease_duration",
	} {
		assertions.Contains(byKey, key)
	}
	for _, key := range []string{
		"server.bind_addr", "server.api_port", "server.api_key", "server.allow_insecure", "server.trusted_proxies",
		"vector.backend", "vector.db_path", "vector.skip_extension_create",
	} {
		setting := byKey[key]
		requirements.NotNil(setting, key)
		assertions.Equal(true, setting["read_only"], key)
	}
	for _, key := range []string{"chat.server", "chat.model", "chat.max_results"} {
		assertions.NotContains(byKey, key, "legacy chat settings have no production consumer")
	}
}

func TestGetSettingsPublishesValidationMetadataFromRegisteredRouter(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, "")

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body struct {
		Settings []map[string]any `json:"settings"`
	}
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := make(map[string]map[string]any, len(body.Settings))
	for _, setting := range body.Settings {
		key, ok := setting["key"].(string)
		requirements.True(ok)
		byKey[key] = setting
	}

	activityBatch, ok := byKey["activity.batch_size"]["validation"].(map[string]any)
	requirements.True(ok, "activity.batch_size must publish validation metadata")
	assertions.InDelta(float64(1), activityBatch["minimum"], 0)
	assertions.InDelta(float64(10_000), activityBatch["maximum"], 0)

	backupLevel, ok := byKey["backup.zstd_level"]["validation"].(map[string]any)
	requirements.True(ok, "backup.zstd_level must publish validation metadata")
	assertions.InDelta(float64(0), backupLevel["minimum"], 0)
	assertions.InDelta(float64(19), backupLevel["maximum"], 0)

	mediaSize, ok := byKey["discord.max_media_mb"]["validation"].(map[string]any)
	requirements.True(ok, "attachment size controls must publish validation metadata")
	assertions.InDelta(float64(0), mediaSize["minimum"], 0)
	assertions.Contains(mediaSize["hint"], "0 uses the Discord default of 50 MiB")

	embeddingEndpoint, ok := byKey["vector.embeddings.endpoint"]["validation"].(map[string]any)
	requirements.True(ok, "provider endpoints must publish safe input guidance")
	assertions.Equal(true, embeddingEndpoint["required"])
	assertions.Contains(embeddingEndpoint["hint"], "without credentials, query, or fragment")

	activitySchedule, ok := byKey["activity.schedule"]["validation"].(map[string]any)
	requirements.True(ok, "schedules must identify their accepted format")
	hint, ok := activitySchedule["hint"].(string)
	requirements.True(ok)
	assertions.Contains(strings.ToLower(hint), "five-field cron")
	assertions.NotEqual(true, byKey["integrations.tasks.endpoint"]["testable"],
		"the daemon has no provider endpoint test operation")
}

func TestSettingsCatalogDoesNotPublishGenericMetadataFallbacks(t *testing.T) {
	assertions := assert.New(t)
	for _, definition := range settingsCatalog {
		_, ok := settingsMetadata[definition.key]
		assertions.True(ok, "%s needs an intentional label and description", definition.key)
	}
}

func TestSettingsProviderCredentialsAreWriteOnlyOwnerOnlyAndETagProtected(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	t.Setenv("TEXT_EMBEDDING_KEY", "environment-secret-must-not-leak")
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_EMBEDDING_KEY"
model = "synthetic-model"
dimension = 8
`)

	first := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	assertions.NotContains(first.Body.String(), "environment-secret-must-not-leak")
	assertions.Equal(map[string]any{"configured": true, "source": "environment"},
		rawEmbeddingSecretState(t, first.Body.Bytes()))
	configETag := first.Header().Get("ETag")
	credentialETag := first.Header().Get("Credential-Etag")
	requirements.NotEmpty(configETag)
	requirements.NotEmpty(credentialETag)

	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.embeddings",
		[]byte(`{"value":"browser-secret-must-not-leak"}`), credentialETag, "")
	requirements.Equal(http.StatusOK, set.Code, set.Body.String())
	var setResponse ProviderCredentialResponse
	requirements.NoError(json.Unmarshal(set.Body.Bytes(), &setResponse))
	assertions.True(setResponse.PendingRestart)
	assertions.NotContains(set.Body.String(), "browser-secret-must-not-leak")
	assertions.NotContains(set.Body.String(), "environment-secret-must-not-leak")
	storedCredentialETag := set.Header().Get("ETag")
	assertions.NotEqual(credentialETag, storedCredentialETag)

	stored := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, stored.Code, stored.Body.String())
	assertions.Equal(map[string]any{"configured": true, "source": "stored"},
		rawEmbeddingSecretState(t, stored.Body.Bytes()))
	assertions.Equal(configETag, stored.Header().Get("ETag"), "credential writes must not masquerade as config writes")
	assertions.Equal(storedCredentialETag, stored.Header().Get("Credential-Etag"))

	credentialPath := filepath.Join(srv.cfg.TokensDir(), "provider-credentials.json")
	info, err := os.Stat(credentialPath)
	requirements.NoError(err)
	if runtime.GOOS != "windows" {
		assertions.Equal(os.FileMode(0o600), info.Mode().Perm())
		dirInfo, statErr := os.Stat(srv.cfg.TokensDir())
		requirements.NoError(statErr)
		assertions.Equal(os.FileMode(0o700), dirInfo.Mode().Perm())
	}
	credentialBytes, err := os.ReadFile(credentialPath)
	requirements.NoError(err)
	assertions.Contains(string(credentialBytes), "browser-secret-must-not-leak")

	stale := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, credentialETag, "")
	assertions.Equal(http.StatusPreconditionFailed, stale.Code, stale.Body.String())

	clearResponseRecorder := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, storedCredentialETag, "")
	requirements.Equal(http.StatusOK, clearResponseRecorder.Code, clearResponseRecorder.Body.String())
	var clearResponse ProviderCredentialResponse
	requirements.NoError(json.Unmarshal(clearResponseRecorder.Body.Bytes(), &clearResponse))
	assertions.True(clearResponse.PendingRestart)
	cleared := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
	assertions.Equal(map[string]any{"configured": true, "source": "environment"},
		rawEmbeddingSecretState(t, cleared.Body.Bytes()))
	assertions.NotContains(cleared.Body.String(), "environment-secret-must-not-leak")
}

func TestPatchSettingsPersistsSafeScalarAndAttachmentPolicies(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, "")
	body, err := json.Marshal(map[string]any{"updates": []map[string]any{
		{"key": "sync.rate_limit_qps", "value": map[string]any{"integer": 12}},
		{"key": "log.enabled", "value": map[string]any{"boolean": true}},
		{"key": "log.level", "value": map[string]any{"string": "debug"}},
		{"key": "log.sql_slow_ms", "value": map[string]any{"integer": 250}},
		{"key": "log.sql_trace", "value": map[string]any{"boolean": true}},
		{"key": "analytics.min_rebuild_interval", "value": map[string]any{"string": "2h"}},
		{"key": "analytics.builder_threads", "value": map[string]any{"integer": 3}},
		{"key": "server.daemon_idle_timeout", "value": map[string]any{"string": "30m"}},
		{"key": "server.daemon_auto_restart", "value": map[string]any{"string": "always"}},
		{"key": "activity.timezone", "value": map[string]any{"string": "America/New_York"}},
		{"key": "activity.max_direct_counterparts", "value": map[string]any{"integer": 50}},
		{"key": "activity.batch_size", "value": map[string]any{"integer": 750}},
		{"key": "activity.schedule", "value": map[string]any{"string": "5 * * * *"}},
		{"key": "backup.zstd_level", "value": map[string]any{"integer": 7}},
		{"key": "beeper.accounts", "value": map[string]any{"strings": []string{"signal"}}},
		{"key": "beeper.exclude_accounts", "value": map[string]any{"strings": []string{"whatsapp"}}},
		{"key": "beeper.rate_limit_qps", "value": map[string]any{"number": 8.5}},
		{"key": "beeper.media", "value": map[string]any{"boolean": false}},
		{"key": "beeper.media_scope", "value": map[string]any{"string": "direct"}},
		{"key": "beeper.media_max_participants", "value": map[string]any{"integer": 5}},
		{"key": "beeper.max_media_mb", "value": map[string]any{"integer": 80}},
		{"key": "slack.channels", "value": map[string]any{"strings": []string{"general"}}},
		{"key": "slack.media", "value": map[string]any{"boolean": true}},
		{"key": "slack.media_scope", "value": map[string]any{"string": "none"}},
		{"key": "slack.media_max_participants", "value": map[string]any{"integer": 0}},
		{"key": "slack.max_media_mb", "value": map[string]any{"integer": 90}},
		{"key": "discord.media", "value": map[string]any{"boolean": true}},
		{"key": "discord.media_scope", "value": map[string]any{"string": "all"}},
		{"key": "discord.media_max_participants", "value": map[string]any{"integer": 10}},
		{"key": "discord.max_media_mb", "value": map[string]any{"integer": 70}},
		{"key": "teams.media", "value": map[string]any{"boolean": false}},
		{"key": "teams.media_scope", "value": map[string]any{"string": "direct"}},
		{"key": "teams.media_max_participants", "value": map[string]any{"integer": 6}},
		{"key": "teams.max_media_mb", "value": map[string]any{"integer": 60}},
		{"key": "vector.embeddings.timeout", "value": map[string]any{"string": "45s"}},
		{"key": "vector.preprocess.strip_quotes", "value": map[string]any{"boolean": false}},
		{"key": "vector.search.max_page_size_hybrid", "value": map[string]any{"integer": 0}},
		{"key": "vector.embed.backstop_interval", "value": map[string]any{"string": "12h"}},
	}})
	requirements.NoError(err)

	resp := patchSettings(t, srv, string(body))
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.Equal(12, loaded.Sync.RateLimitQPS)
	assertions.Equal("debug", loaded.Log.Level)
	assertions.Equal(int64(250), loaded.Log.SQLSlowMs)
	assertions.Equal(3, loaded.Analytics.BuilderThreads)
	assertions.Equal("America/New_York", loaded.Activity.Timezone)
	assertions.Equal(7, loaded.Backup.ZstdLevel)
	assertions.Equal([]string{"signal"}, loaded.Beeper.Accounts)
	assertions.Equal([]string{"whatsapp"}, loaded.Beeper.ExcludeAccounts)
	assertions.InDelta(8.5, loaded.Beeper.RateLimitQPS, 0.001)
	assertions.False(loaded.Beeper.MediaEnabled())
	assertions.Equal("direct", loaded.Beeper.MediaScope)
	assertions.Equal(5, loaded.Beeper.MediaMaxParticipants)
	assertions.Equal(80, loaded.Beeper.MaxMediaMB)
	assertions.Equal("none", loaded.Slack.MediaScope)
	assertions.Equal(90, loaded.Slack.MaxMediaMB)
	assertions.Equal(10, loaded.Discord.MediaMaxParticipants)
	assertions.Equal(60, loaded.Teams.MaxMediaMB)
	assertions.False(loaded.Vector.Preprocess.StripQuotesEnabled())
	assertions.Equal(0, loaded.Vector.Search.MaxPageSizeHybridClamp())
}

func TestPatchSettingsRejectsInvalidAttachmentPolicy(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "")
	resp := patchSettings(t, srv,
		`{"updates":[{"key":"teams.media_max_participants","value":{"integer":-1}}]}`)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.NotContains(t, resp.Body.String(), "provider-credentials")
}

func TestPatchSettingsRejectsHostAndAuthSettings(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{key: "server.bind_addr", value: `{"string":"127.0.0.2"}`},
		{key: "server.api_port", value: `{"integer":8080}`},
		{key: "server.api_key", value: `null`},
		{key: "server.allow_insecure", value: `{"boolean":true}`},
		{key: "server.trusted_proxies", value: `{"strings":["127.0.0.1"]}`},
		{key: "vector.backend", value: `{"string":"pgvector"}`},
		{key: "vector.db_path", value: `{"string":"/tmp/remote-controlled.db"}`},
		{key: "vector.skip_extension_create", value: `{"boolean":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			before := "[web]\ntheme = \"system\"\n"
			srv, path := newSettingsTestServer(t, before)
			body := fmt.Sprintf(`{"updates":[{"key":%q,"value":%s}]}`, tt.key, tt.value)
			if tt.key == "server.api_key" {
				body = `{"updates":[{"key":"server.api_key","secret":{"action":"set","value":"remote-secret"}}]}`
			}
			resp := patchSettings(t, srv, body)
			assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, string(got))
		})
	}
}

func TestPutSettingsPersonEnrichmentProviderPreservesStableNamesAndOrder(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, `[people.enrichment]
enabled = false
schedule = "0 * * * *"
batch_size = 10
lease_duration = "10m"
suppression_key_env = "SUPPRESSION_KEY"

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = false
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100

[[people.enrichment.providers]]
name = "sixtyfour-primary"
kind = "sixtyfour"
enabled = false
api_key_env = "SIXTYFOUR_KEY"
tier = "standard"
allowed_identifiers = ["name", "current_company"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 5
max_requests_per_day = 50
`)

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	resp := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary", []byte(`{
"kind":"exa",
"enabled":true,
"endpoint":"https://api.exa.ai/search",
"mode":"people",
"allowed_identifiers":["name","email"],
"target_keys":["attribute:bio"],
"allow_sensitive_targets":false,
"retention_posture":"zero_retention",
"training_posture":"no_training",
"refresh_interval":"24h",
"request_timeout":"1m",
"max_retries":5,
"max_requests_per_run":20,
"max_requests_per_day":100
}`), get.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirements.NoError(err)
	requirements.Len(loaded.People.Enrichment.Providers, 2)
	assertions.Equal("exa-primary", loaded.People.Enrichment.Providers[0].Name)
	assertions.True(loaded.People.Enrichment.Providers[0].Enabled)
	assertions.Equal(int64(20), loaded.People.Enrichment.Providers[0].MaxRequestsPerRun)
	assertions.Equal("sixtyfour-primary", loaded.People.Enrichment.Providers[1].Name)
	assertions.Equal(int64(50), loaded.People.Enrichment.Providers[1].MaxRequestsPerDay)
}

func TestPatchSettingsFirstEnrichmentEnableGeneratesPrivateSuppressionKey(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = true
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`)

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"people.enrichment.enabled","value":{"boolean":true}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.True(loaded.People.Enrichment.Enabled)
	assertions.NotEmpty(loaded.People.Enrichment.SuppressionKeyEnv)
	credentialBytes, err := os.ReadFile(filepath.Join(loaded.TokensDir(), "provider-credentials.json"))
	requirements.NoError(err)
	credentials, err := providercredentials.Read(loaded.TokensDir())
	requirements.NoError(err)
	suppressionKey, configured, err := credentials.ResolveSuppression()
	requirements.NoError(err)
	requirements.True(configured)
	assertions.NotContains(resp.Body.String(), suppressionKey)
	assertions.Greater(len(credentialBytes), 64)
}

func TestPatchSettingsRejectedFirstEnrichmentEnableLeavesCredentialStoreUnchanged(t *testing.T) {
	for _, test := range []struct {
		name       string
		editorErr  error
		wantStatus int
	}{
		{name: "invalid candidate", editorErr: config.ErrInvalidConfigCandidate, wantStatus: http.StatusUnprocessableEntity},
		{name: "stale config", editorErr: config.ErrConfigConflict, wantStatus: http.StatusPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			srv, _ := newSettingsTestServer(t, `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = true
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`)
			srv.settingsConfigEditor = func(string, string, []config.Edit) (config.ConfigFile, error) {
				return config.ConfigFile{}, test.editorErr
			}

			resp := patchSettings(t, srv,
				`{"updates":[{"key":"people.enrichment.enabled","value":{"boolean":true}}]}`)
			assertions.Equal(test.wantStatus, resp.Code, resp.Body.String())
			credentials, err := providercredentials.Read(srv.cfg.TokensDir())
			requirements.NoError(err)
			_, configured, err := credentials.ResolveSuppression()
			requirements.NoError(err)
			assertions.False(configured)
		})
	}
}

func TestSettingsProviderCredentialStoreFailsClosedWhenUnsafeOrCorrupt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	t.Setenv("TEXT_EMBEDDING_KEY", "environment-fallback-must-not-be-used")
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_EMBEDDING_KEY"
model = "synthetic-model"
dimension = 8
`)
	requirements.NoError(os.MkdirAll(srv.cfg.TokensDir(), 0o700))
	path := filepath.Join(srv.cfg.TokensDir(), "provider-credentials.json")
	requirements.NoError(os.WriteFile(path, []byte(`{"version":1,"credentials":`), 0o600))

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	assertions.Equal(http.StatusInternalServerError, resp.Code, resp.Body.String())
	assertions.NotContains(resp.Body.String(), "environment-fallback-must-not-be-used")
	assertions.NotContains(resp.Body.String(), "provider-credentials.json")
}

func TestSettingsStoredCredentialIsBoundToEndpointOrigin(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://first.example.test/v1"
api_key_env = "UNSET_TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.embeddings",
		[]byte(`{"value":"origin-bound-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirements.Equal(http.StatusOK, set.Code, set.Body.String())

	changed := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://second.example.test/v1"}}]}`),
		get.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, changed.Code, changed.Body.String())
	assertions.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, changed.Body.Bytes()))
	assertions.NotContains(changed.Body.String(), "origin-bound-secret")
}

func TestPatchSettingsHardensSecretBearingConfigFile(t *testing.T) {
	requirements := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows owner-only DACL coverage lives in platform-specific config tests")
	}
	srv, path := newSettingsTestServer(t, "[server]\napi_key = \"secret\"\n[web]\ntheme = \"system\"\n")
	requirements.NoError(os.Chmod(path, 0o644))

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "secret")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	patched := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		resp.Header().Get("ETag"), "secret")
	requirements.Equal(http.StatusOK, patched.Code, patched.Body.String())
	info, err := os.Stat(path)
	requirements.NoError(err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPatchSettingsPreservesUntouchedMediaPointerAndOpaqueOverrides(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, `[discord]
media_scope = "all"
[discord.guilds.G01]
media = false
max_media_mb = 30
`)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	var document struct {
		Settings []map[string]any `json:"settings"`
	}
	requirements.NoError(json.Unmarshal(get.Body.Bytes(), &document))
	for _, setting := range document.Settings {
		if setting["key"] == "discord.media" {
			assertions.Equal(true, setting["inherited"], "omitted provider policy must be identified as inherited/default")
			assertions.Equal("Provider default is enabled; changes affect future downloads only.", setting["description"])
		}
		if setting["key"] == "discord.max_media_mb" {
			assertions.Contains(setting["description"], "0 uses the Discord default of 50 MiB")
		}
	}

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"discord.media_scope","value":{"string":"direct"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.Nil(loaded.Discord.Media, "untouched provider default must remain omitted")
	requirements.Contains(loaded.Discord.Guilds, "G01")
	assertions.NotNil(loaded.Discord.Guilds["G01"].Media)
	assertions.False(*loaded.Discord.Guilds["G01"].Media)
	assertions.Equal(30, loaded.Discord.Guilds["G01"].MaxMediaMB)
}

func TestSettingsRejectsAndRedactsCredentialBearingEmbeddingEndpoints(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://user:legacy-password@embeddings.example.test/v1"
api_key_env = "TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	assertions.Equal(http.StatusInternalServerError, get.Code, get.Body.String())
	assertions.NotContains(get.Body.String(), "legacy-password")
	assertions.NotContains(get.Body.String(), "user:")

	clean, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)
	first := performSettingsRequest(t, clean, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	patch := performSettingsRequest(t, clean, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://user:new-password@other.example.test/v1?api_key=query-secret#fragment-secret"}}]}`),
		first.Header().Get("ETag"), "")
	assertions.Equal(http.StatusUnprocessableEntity, patch.Code, patch.Body.String())
	for _, secret := range []string{"new-password", "query-secret", "fragment-secret", "user:"} {
		assertions.NotContains(patch.Body.String(), secret)
	}
}

func TestPatchSettingsExposesCompleteSemanticPersonOptInPolicy(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv, path := newSettingsTestServer(t, "[vector]\n"+
		"enabled = true\n"+
		"[vector.embeddings]\n"+
		"endpoint = \"https://embedding.example.test/v1\"\n"+
		"model = \"synthetic-model\"\n"+
		"dimension = 4\n")
	response := patchSettings(t, srv, `{"updates":[
		{"key":"vector.people.enabled","value":{"boolean":true}},
		{"key":"vector.people.retention_posture","value":{"string":"zero_data_retention"}},
		{"key":"vector.people.training_posture","value":{"string":"no_training"}}
	]}`)
	must.Equal(http.StatusOK, response.Code, response.Body.String())

	got, err := os.ReadFile(path)
	must.NoError(err)
	check.Contains(string(got), "people.enabled = true")
	check.Contains(string(got), `people.retention_posture = "zero_data_retention"`)
	check.Contains(string(got), `people.training_posture = "no_training"`)
}

func TestGetSettingsExposesReadOnlyCardDAVAccountStateWithoutCredential(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, _ := newSettingsTestServer(t, `[carddav]
base_url = "https://contacts.example/dav"
username = "alice"
schedule = "0 3 * * *"
enabled = true
`)
	st := testutil.NewTestStore(t)
	account, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: srv.cfg.CardDAV.BaseURL, Username: srv.cfg.CardDAV.Username,
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
		}},
	})
	requirements.NoError(err)
	requirements.NoError(carddav.SaveCredential(srv.cfg.TokensDir(), carddav.Credential{
		Password: "must-not-cross-api", BaseURL: srv.cfg.CardDAV.BaseURL,
		Username: srv.cfg.CardDAV.Username, ConnectionGeneration: account.ConnectionGeneration,
	}))
	srv.cardDAV, err = NewCardDAVController(srv.cfg, st, slog.New(slog.DiscardHandler))
	requirements.NoError(err)

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	for _, key := range []string{"carddav.base_url", "carddav.username", "carddav.schedule", "carddav.enabled", "carddav.password"} {
		requirements.Contains(byKey, key)
		assertions.True(byKey[key].ReadOnly, key)
	}
	assertions.Equal(&SecretSettingState{Configured: true}, byKey["carddav.password"].Secret)
	assertions.NotContains(resp.Body.String(), "must-not-cross-api")

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"carddav.enabled","value":{"boolean":false}}]}`),
		resp.Header().Get("ETag"), "")
	assertions.Equal(http.StatusBadRequest, patch.Code, patch.Body.String())
}

func TestGetSettingsReportsStaleCardDAVCredentialAsNotConfigured(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, _ := newSettingsTestServer(t, `[carddav]
base_url = "https://contacts.example/dav"
username = "alice"
enabled = true
`)
	st := testutil.NewTestStore(t)
	discovery := store.CardDAVDiscoveryInput{
		BaseURL: srv.cfg.CardDAV.BaseURL, Username: srv.cfg.CardDAV.Username,
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: "https://contacts.example/books/alice/personal/",
		}},
	}
	account, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	requirements.NoError(err)
	requirements.NoError(carddav.SaveCredential(srv.cfg.TokensDir(), carddav.Credential{
		Password: "stale-password", BaseURL: srv.cfg.CardDAV.BaseURL,
		Username: srv.cfg.CardDAV.Username, ConnectionGeneration: account.ConnectionGeneration,
	}))
	discovery.CredentialsChanged = true
	account, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	requirements.NoError(err)
	assertions.Equal(int64(2), account.ConnectionGeneration)
	srv.cardDAV, err = NewCardDAVController(srv.cfg, st, slog.New(slog.DiscardHandler))
	requirements.NoError(err)

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertions.Equal(&SecretSettingState{Configured: false}, settingsByKey(body.Settings)["carddav.password"].Secret)
	assertions.NotContains(resp.Body.String(), "stale-password")
}

func TestPatchSettingsSelectsVoyageContextualEmbeddingFormat(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t, "[vector.embeddings]\n"+
		"endpoint = \"https://api.voyageai.com/v1\"\n"+
		"model = \"text-embedding-test\"\n"+
		"dimension = 1024\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.api_format","value":{"string":"voyage-contextual"}},{"key":"vector.embeddings.model","value":{"string":"voyage-context-4"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), `api_format = "voyage-contextual"`)
	assertions.Contains(string(got), `model = "voyage-context-4"`)
}

func TestGetSettingsExposesMultimodalPolicyWithoutCredentialState(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	t.Setenv("SYNTHETIC_VOYAGE_KEY", "synthetic-key-value")
	srv, _ := newSettingsTestServer(t, `[vector.multimodal]
enabled = true
api_key_env = "SYNTHETIC_VOYAGE_KEY"
include_images = false
include_video = true
`)
	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirements.NotNil(byKey["vector.multimodal.enabled"].Value.Boolean)
	assertions.True(*byKey["vector.multimodal.enabled"].Value.Boolean)
	requirements.NotNil(byKey["vector.multimodal.include_images"].Value.Boolean)
	assertions.False(*byKey["vector.multimodal.include_images"].Value.Boolean)
	requirements.NotNil(byKey["vector.multimodal.include_video"].Value.Boolean)
	assertions.True(*byKey["vector.multimodal.include_video"].Value.Boolean)
	assertions.True(byKey["vector.multimodal.api_key_env"].ReadOnly)
	assertions.NotContains(resp.Body.String(), "synthetic-key-value")
}

func TestPatchSettingsRequiresMatchingETag(t *testing.T) {
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")

	missing := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")
	assertions.Equal(http.StatusPreconditionRequired, missing.Code, missing.Body.String())

	mismatch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "\"sha256-stale\"", "")
	assertions.Equal(http.StatusPreconditionFailed, mismatch.Code, mismatch.Body.String())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assertions.Equal("[web]\ntheme = \"system\"\n", string(got))
}

func TestPatchSettingsPreservesFileAndReturnsNewETag(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t, "# operator comment\n[unknown]\nkeep = true\n\n"+
		"[web]\ntheme = \"system\" # display\n")
	if runtime.GOOS != "windows" {
		requirements.NoError(os.Chmod(path, 0o640))
	}
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	etag := get.Header().Get("ETag")

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), etag, "")
	requirements.Equal(http.StatusOK, patch.Code, patch.Body.String())
	assertions.NotEqual(etag, patch.Header().Get("ETag"))
	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal("# operator comment\n[unknown]\nkeep = true\n\n[web]\ntheme = \"dark\" # display\n", string(got))
	if runtime.GOOS != "windows" {
		// Settings publication hardens the entire secret-bearing config to
		// owner-only. Windows security lives in the DACL, which
		// the config package's own Windows tests verify; Stat mode bits there
		// are synthetic.
		info, err := os.Stat(path)
		requirements.NoError(err)
		assertions.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestPatchSettingsValidatesWholeCandidateAndRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{
			name:   "invalid catalog value",
			body:   `{"updates":[{"key":"analytics.engine","value":{"string":"invalid"}}]}`,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "unsupported key",
			body:   `{"updates":[{"key":"unsupported.private_value","value":{"string":"changed"}}]}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "secret sent as ordinary value",
			body:   `{"updates":[{"key":"server.api_key","value":{"string":"leak"}}]}`,
			status: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := "[analytics]\nengine = \"auto\"\n[unsupported]\nprivate_value = \"keep\"\n"
			srv, path := newSettingsTestServer(t, before)
			get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
			resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath, []byte(tt.body),
				get.Header().Get("ETag"), "")
			assert.Equal(t, tt.status, resp.Code, resp.Body.String())
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, string(got))
		})
	}
}

func TestPatchSettingsRejectsHostManagedServerAPIKeyUpdates(t *testing.T) {
	assertions := assert.New(t)
	requests := []string{
		`{"updates":[{"key":"server.api_key","value":{"string":"new-key"}}]}`,
		`{"updates":[{"key":"server.api_key","secret":{"action":"set","value":"new-key"}}]}`,
		`{"updates":[{"key":"server.api_key","secret":{"action":"clear"}}]}`,
	}
	for _, request := range requests {
		srv, path := newSettingsTestServer(t, "[server]\napi_key = \"old-key\"\n")
		get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "old-key")
		resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
			[]byte(request), get.Header().Get("ETag"), "old-key")

		assertions.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
		assertions.Contains(resp.Body.String(), "host-managed")
		assertions.NotContains(resp.Body.String(), "new-key")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assertions.Equal("[server]\napi_key = \"old-key\"\n", string(got))
	}
}

func TestPatchSettingsClearsTaskAPIKeyWhenEndpointOriginChanges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertions.Contains(string(got), "api_key = \"\"")
	assertions.NotContains(string(got), "task-secret")

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	assertions.Equal(&SecretSettingState{Configured: false}, byKey["integrations.tasks.api_key"].Secret)
	assertions.True(body.PendingRestart)
}

func TestPatchSettingsKeepsNewTaskAPIKeyProvidedWithEndpointChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[`+
			`{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}},`+
			`{"key":"integrations.tasks.api_key","secret":{"action":"set","value":"rotated-secret"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertions.Contains(string(got), "api_key = \"rotated-secret\"")

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertions.Equal(&SecretSettingState{Configured: true},
		settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
}

func TestPatchSettingsRetainsTaskAPIKeyWhenEndpointOriginIsUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "identical endpoint", endpoint: "https://tasks.example.com/api"},
		{name: "same origin different path", endpoint: "https://tasks.example.com/v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv, path := newSettingsTestServer(t,
				"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

			resp := patchSettings(t, srv, fmt.Sprintf(
				`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":%q}}]}`, tt.endpoint))
			requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

			got, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Contains(string(got), "api_key = \"task-secret\"")

			var body SettingsResponse
			requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
			assertions.Equal(&SecretSettingState{Configured: true},
				settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
		})
	}
}

func TestPatchSettingsEndpointChangeWithoutStoredCredentialAddsNoKey(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertions.NotContains(string(got), "api_key")
}

func TestPatchSettingsRetainsEmbeddingsAPIKeyEnvWhenEndpointOriginChanges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://elsewhere.example.net/v1"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), "endpoint = \"https://elsewhere.example.net/v1\"")
	assertions.Contains(string(got), "api_key_env = \"MSGVAULT_EMBED_API_KEY\"")

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirements.NotNil(byKey["vector.embeddings.api_key_env"].Value)
	requirements.NotNil(byKey["vector.embeddings.api_key_env"].Value.String)
	assertions.Equal("MSGVAULT_EMBED_API_KEY", *byKey["vector.embeddings.api_key_env"].Value.String)
}

func TestPatchSettingsRetainsMultimodalAPIKeyEnvWhenEndpointOriginChanges(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t,
		"[vector.multimodal]\nendpoint = \"https://api.voyageai.com/v1\"\n"+
			"api_key_env = \"SYNTHETIC_VOYAGE_KEY\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.multimodal.endpoint","value":{"string":"https://voyage.example.test/v1"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), `endpoint = "https://voyage.example.test/v1"`)
	assertions.Contains(string(got), `api_key_env = "SYNTHETIC_VOYAGE_KEY"`)

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	keySetting := settingsByKey(body.Settings)["vector.multimodal.api_key_env"]
	requirements.NotNil(keySetting.Value)
	requirements.NotNil(keySetting.Value.String)
	assertions.Equal("SYNTHETIC_VOYAGE_KEY", *keySetting.Value.String)
}

func TestPatchSettingsEditableChangeSucceedsWhileReadOnlySettingIsConfigured(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n"+
		"[vector.embeddings]\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n")

	resp := patchSettings(t, srv, `{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`)
	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(got), "theme = \"dark\"")
	assertions.Contains(string(got), "api_key_env = \"MSGVAULT_EMBED_API_KEY\"")

	var body SettingsResponse
	requirements.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertions.True(settingsByKey(body.Settings)["vector.embeddings.api_key_env"].ReadOnly)
}

func TestPatchSettingsRejectsEmbeddingsAPIKeyEnvUpdates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	before := "[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n"
	srv, path := newSettingsTestServer(t, before)

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.api_key_env","value":{"string":"AWS_SECRET_ACCESS_KEY"}}]}`)
	requirements.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
	assertions.Contains(resp.Body.String(), "host-managed")

	got, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(before, string(got))
}

func TestSettingsErrorsAreNotCached(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")

	assert.Equal(t, http.StatusPreconditionRequired, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestSettingsMiddlewareErrorsAreNotCached(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[server]\napi_key = \"test-api-key\"\n")
	login := performSessionRequest(t, srv, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"test-api-key"}`), nil, false)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	cookie := requireSessionCookie(t, login)
	resp := performSessionRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		http.Header{"Cookie": []string{cookie.String()}}, false)

	assert.Equal(t, http.StatusForbidden, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestPatchSettingsRejectsTrailingJSON(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]} {}`),
		get.Header().Get("ETag"), "")

	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

func TestPatchSettingsClassifiesFilesystemFailureAsServerError(t *testing.T) {
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	blockSettingsConfigFilesystem(t, path)

	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")

	assert.Equal(t, http.StatusInternalServerError, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestPatchSettingsMarksRestartPendingWhenPublishedWriteReturnsError(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, path := newSettingsTestServer(t, "[server]\ndaemon_idle_timeout = \"15m\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(configPath, ifMatch string, edits []config.Edit) (config.ConfigFile, error) {
		requirements.Equal(path, configPath)
		requirements.Equal(get.Header().Get("ETag"), ifMatch)
		requirements.Len(edits, 1)
		requirements.NoError(os.WriteFile(path, []byte("[server]\ndaemon_idle_timeout = \"1h\"\n"), 0o600))
		return config.ConfigFile{}, fmt.Errorf("%w: cleanup failed", config.ErrConfigChanged)
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"server.daemon_idle_timeout","value":{"string":"1h"}}]}`),
		get.Header().Get("ETag"), "")
	assertions.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())

	after := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, after.Code, after.Body.String())
	var persisted SettingsResponse
	requirements.NoError(json.Unmarshal(after.Body.Bytes(), &persisted))
	assertions.True(persisted.PendingRestart)
}

func TestPatchSettingsMarksRestartPendingBeforeLoadingCommittedSnapshot(t *testing.T) {
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, "[server]\ndaemon_idle_timeout = \"15m\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(string, string, []config.Edit) (config.ConfigFile, error) {
		return config.ConfigFile{
			LogicalPath: "config.toml",
			Path:        "config.toml",
			Content:     []byte("invalid = ["),
			ETag:        `"sha256-committed"`,
			Exists:      true,
		}, nil
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"server.daemon_idle_timeout","value":{"string":"1h"}}]}`),
		get.Header().Get("ETag"), "")
	assertions.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())
	assertions.True(srv.settingsPendingRestart.Load())
}

func TestPatchSettingsPrefersChangedOutcomeOverConflictClassification(t *testing.T) {
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(string, string, []config.Edit) (config.ConfigFile, error) {
		return config.ConfigFile{}, errors.Join(config.ErrConfigChanged, config.ErrConfigConflict)
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		get.Header().Get("ETag"), "")
	assert.Equal(t, http.StatusInternalServerError, patch.Code, patch.Body.String())
}

func TestSettingsOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := OpenAPIDocument()
	requirements.NotNil(doc.Paths[settingsPath])
	get := doc.Paths[settingsPath].Get
	patch := doc.Paths[settingsPath].Patch
	requirements.NotNil(get)
	requirements.NotNil(patch)
	assertions.Contains(get.Responses["200"].Headers, "ETag")
	requirements.Len(patch.Parameters, 1)
	assertions.Equal("If-Match", patch.Parameters[0].Name)
	assertions.Equal("header", patch.Parameters[0].In)
	assertions.True(patch.Parameters[0].Required)
	for _, status := range []string{"400", "409", "412", "422", "428"} {
		assertions.Contains(patch.Responses, status)
	}
	assertions.Equal(APISchemaVersion, doc.Info.Version)

	settingValue := doc.Components.Schemas.Map()["SettingValue"]
	requirements.NotNil(settingValue)
	assertions.Len(settingValue.OneOf, 5)
	assertions.Empty(settingValue.Properties)
	for _, arm := range settingValue.OneOf {
		assertions.Len(arm.Required, 1)
		assertions.Equal([]string{arm.Required[0]}, arm.Required)
		assertions.Equal(false, arm.AdditionalProperties)
	}
	setting := doc.Components.Schemas.Map()["Setting"]
	requirements.NotNil(setting)
	assertions.ElementsMatch([]any{
		"browser", "server", "archive", "sync", "logging", "search", "sources", "attachments",
		"activity", "backup", "enrichment", "integrations",
	}, setting.Properties["group"].Enum)
	assertions.ElementsMatch([]any{"string", "integer", "number", "boolean", "string_array", "secret"}, setting.Properties["kind"].Enum)
	patchRequest := doc.Components.Schemas.Map()["SettingsPatchRequest"]
	requirements.NotNil(patchRequest)
	assertions.False(patchRequest.Properties["updates"].Nullable)
	settingsResponse := doc.Components.Schemas.Map()["SettingsResponse"]
	requirements.NotNil(settingsResponse)
	assertions.False(settingsResponse.Properties["groups"].Nullable)
}

func newSettingsTestServer(t *testing.T, content string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	cfg, err := config.Load(path, "")
	require.NoError(t, err)
	logger := slog.New(slog.DiscardHandler)
	return NewServer(cfg, nil, nil, logger), path
}

func performSettingsRequest(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body []byte,
	ifMatch string,
	apiKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}

// patchSettings performs a GET to obtain the current ETag and issues a PATCH
// with the supplied JSON body against an unauthenticated test server.
func patchSettings(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	return performSettingsRequest(t, srv, http.MethodPatch, settingsPath, []byte(body),
		get.Header().Get("ETag"), "")
}

func settingsByKey(settings []Setting) map[string]Setting {
	result := make(map[string]Setting, len(settings))
	for _, setting := range settings {
		result[setting.Key] = setting
	}
	return result
}

func rawEmbeddingSecretState(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var document struct {
		Settings []struct {
			Key    string         `json:"key"`
			Secret map[string]any `json:"secret"`
		} `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(body, &document))
	for _, setting := range document.Settings {
		if setting.Key == "vector.embeddings.api_key" {
			return setting.Secret
		}
	}
	require.FailNow(t, "setting not found", "vector.embeddings.api_key")
	return nil
}

const settingsStoredVectorConfig = `[vector.embeddings]
endpoint = "https://first.example.test/v1"
api_key_env = "UNSET_TEXT_KEY"
model = "synthetic-model"
dimension = 8
`

func storeVectorEmbeddingsCredential(t *testing.T, srv *Server, value string) string {
	t.Helper()
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.embeddings",
		[]byte(`{"value":"`+value+`"}`), get.Header().Get("Credential-Etag"), "")
	require.Equal(t, http.StatusOK, set.Code, set.Body.String())
	return set.Header().Get("ETag")
}

func TestPatchSettingsSeversStoredCredentialOnlyWhenEndpointOriginChanges(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, settingsStoredVectorConfig)
	storeVectorEmbeddingsCredential(t, srv, "origin-bound-secret")

	samePath := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://first.example.test/v2"}}]}`)
	requirements.Equal(http.StatusOK, samePath.Code, samePath.Body.String())
	assertions.Equal(map[string]any{"configured": true, "source": "stored"},
		rawEmbeddingSecretState(t, samePath.Body.Bytes()))
	retained, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.True(retained.Stored(providercredentials.VectorEmbeddingsID))

	moved := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://second.example.test/v1"}}]}`)
	requirements.Equal(http.StatusOK, moved.Code, moved.Body.String())
	assertions.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, moved.Body.Bytes()))
	assertions.NotContains(moved.Body.String(), "origin-bound-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(severed.Stored(providercredentials.VectorEmbeddingsID))
	assertions.Equal(severed.ETag, moved.Header().Get("Credential-Etag"))

	after := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, after.Code, after.Body.String())
	assertions.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, after.Body.Bytes()))
	credentialBytes, err := os.ReadFile(filepath.Join(srv.cfg.TokensDir(), providercredentials.Filename))
	requirements.NoError(err)
	assertions.NotContains(string(credentialBytes), "origin-bound-secret")
}

func TestDeleteProviderCredentialOutlivesConfiguredProvider(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, settingsStoredVectorConfig)
	credentialETag := storeVectorEmbeddingsCredential(t, srv, "orphaned-secret")

	requirements.NoError(os.WriteFile(path, []byte("[analytics]\nengine = \"auto\"\n"), 0o600))

	deleted := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, credentialETag, "")
	requirements.Equal(http.StatusOK, deleted.Code, deleted.Body.String())
	var response ProviderCredentialResponse
	requirements.NoError(json.Unmarshal(deleted.Body.Bytes(), &response))
	assertions.Equal(SecretSettingState{Configured: false, Source: "none"}, response.State)
	remaining, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(remaining.Stored(providercredentials.VectorEmbeddingsID))
	assertions.Equal(remaining.ETag, deleted.Header().Get("ETag"))

	missing := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, remaining.ETag, "")
	assertions.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	assertions.Contains(missing.Body.String(), "credential_not_found")
}

func TestPutPersonEnrichmentProviderSeversStoredCredentialWhenOriginChanges(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[people.enrichment]
enabled = false
suppression_key_env = "SUPPRESSION_KEY"

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = false
endpoint = "https://api.exa.example/search"
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	credentialID := providercredentials.PersonEnrichmentID("exa-primary")
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(credentialID),
		[]byte(`{"value":"exa-origin-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirements.Equal(http.StatusOK, set.Code, set.Body.String())

	providerUpdate := func(endpoint string) []byte {
		return []byte(`{"kind":"exa","enabled":false,"endpoint":"` + endpoint + `","mode":"people",` +
			`"allowed_identifiers":["name","email"],"target_keys":["attribute:bio"],` +
			`"allow_sensitive_targets":false,"retention_posture":"zero_retention",` +
			`"training_posture":"no_training","refresh_interval":"24h","request_timeout":"1m",` +
			`"max_retries":5,"max_requests_per_run":20,"max_requests_per_day":100}`)
	}
	samePath := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary",
		providerUpdate("https://api.exa.example/v2/search"), get.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, samePath.Code, samePath.Body.String())
	var retainedResponse SettingsResponse
	requirements.NoError(json.Unmarshal(samePath.Body.Bytes(), &retainedResponse))
	requirements.Len(retainedResponse.PersonEnrichmentProviders, 1)
	assertions.Equal(&SecretSettingState{Configured: true, Source: "stored"},
		retainedResponse.PersonEnrichmentProviders[0].Credential)

	moved := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary",
		providerUpdate("https://elsewhere.example/search"), samePath.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, moved.Code, moved.Body.String())
	var severedResponse SettingsResponse
	requirements.NoError(json.Unmarshal(moved.Body.Bytes(), &severedResponse))
	requirements.Len(severedResponse.PersonEnrichmentProviders, 1)
	assertions.Equal(&SecretSettingState{Configured: false, Source: "none"},
		severedResponse.PersonEnrichmentProviders[0].Credential)
	assertions.NotContains(moved.Body.String(), "exa-origin-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(severed.Stored(credentialID))
	assertions.Equal(severed.ETag, moved.Header().Get("Credential-Etag"))
}

func TestPutPersonEnrichmentProviderRollsBackConfigWhenCredentialCleanupConflicts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const before = `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = false
endpoint = "https://api.exa.example/search"
api_key_env = "EXA_KEY"
allowed_identifiers = ["name", "email"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`
	srv, path := newSettingsTestServer(t, before)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	credentialID := providercredentials.PersonEnrichmentID("exa-primary")
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(credentialID),
		[]byte(`{"value":"origin-bound-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirements.Equal(http.StatusOK, set.Code, set.Body.String())
	srv.settingsCredentialDeleter = func(
		string, providercredentials.Snapshot, []string,
	) (providercredentials.Snapshot, error) {
		return providercredentials.Snapshot{}, providercredentials.ErrConflict
	}

	update := []byte(`{"kind":"exa","enabled":false,"endpoint":"https://elsewhere.example/search",` +
		`"mode":"people","allowed_identifiers":["name","email"],"target_keys":["attribute:bio"],` +
		`"allow_sensitive_targets":false,"retention_posture":"zero_retention",` +
		`"training_posture":"no_training","refresh_interval":"24h","request_timeout":"1m",` +
		`"max_retries":5,"max_requests_per_run":20,"max_requests_per_day":100}`)
	conflict := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary",
		update, get.Header().Get("ETag"), "")
	assertions.Equal(http.StatusPreconditionFailed, conflict.Code, conflict.Body.String())

	written, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(before, string(written))
	credentials, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	value, state, err := credentials.Resolve(
		credentialID, "https://api.exa.example/search", "", nil,
	)
	requirements.NoError(err)
	assertions.Equal("origin-bound-secret", value)
	assertions.True(state.Configured)
	assertions.False(srv.settingsPendingRestart.Load())
}

func TestPatchSettingsRemovesStaleStoredCredentialOnLaterWrite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, settingsStoredVectorConfig)
	storeVectorEmbeddingsCredential(t, srv, "stale-origin-secret")

	// The endpoint moves without the API severing the credential, as after an
	// interrupted cleanup or a host edit of config.toml.
	moved := strings.Replace(settingsStoredVectorConfig,
		"https://first.example.test/v1", "https://second.example.test/v1", 1)
	requirements.NoError(os.WriteFile(path, []byte(moved), 0o600))

	before := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, before.Code, before.Body.String())
	assertions.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, before.Body.Bytes()))
	stale, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	requirements.True(stale.Stored(providercredentials.VectorEmbeddingsID))

	unrelated := patchSettings(t, srv,
		`{"updates":[{"key":"log.level","value":{"string":"debug"}}]}`)
	requirements.Equal(http.StatusOK, unrelated.Code, unrelated.Body.String())
	assertions.NotContains(unrelated.Body.String(), "stale-origin-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(severed.Stored(providercredentials.VectorEmbeddingsID))
	assertions.Equal(severed.ETag, unrelated.Header().Get("Credential-Etag"))
}

func TestPatchSettingsRemovesStaleNamedProviderCredentialsAfterHostEdit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const initial = `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "removed-provider"
kind = "exa"
enabled = false
endpoint = "https://removed.example/search"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100

[[people.enrichment.providers]]
name = "moved-provider"
kind = "exa"
enabled = false
endpoint = "https://first.example/search"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`
	srv, path := newSettingsTestServer(t, initial)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	removedID := providercredentials.PersonEnrichmentID("removed-provider")
	movedID := providercredentials.PersonEnrichmentID("moved-provider")
	stored := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(removedID),
		[]byte(`{"value":"removed-provider-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirements.Equal(http.StatusOK, stored.Code, stored.Body.String())
	stored = performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(movedID),
		[]byte(`{"value":"moved-provider-secret"}`), stored.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, stored.Code, stored.Body.String())

	const hostEdited = `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "moved-provider"
kind = "exa"
enabled = false
endpoint = "https://second.example/search"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`
	requirements.NoError(os.WriteFile(path, []byte(hostEdited), 0o600))
	stale, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	requirements.True(stale.Stored(removedID))
	requirements.True(stale.Stored(movedID))

	unrelated := patchSettings(t, srv,
		`{"updates":[{"key":"log.level","value":{"string":"debug"}}]}`)
	requirements.Equal(http.StatusOK, unrelated.Code, unrelated.Body.String())
	cleaned, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	assertions.False(cleaned.Stored(removedID))
	assertions.False(cleaned.Stored(movedID))
	assertions.Equal(cleaned.ETag, unrelated.Header().Get("Credential-Etag"))
}

func TestPutPersonEnrichmentProviderRequiresValidKindEvenWhenDisabled(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, path := newSettingsTestServer(t, "[people.enrichment]\nenabled = false\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())

	for _, kind := range []string{"", "other"} {
		body := []byte(`{"kind":"` + kind + `","enabled":false,"endpoint":"https://api.exa.example/search",` +
			`"allowed_identifiers":["name"],"target_keys":["attribute:bio"],` +
			`"allow_sensitive_targets":false,"retention_posture":"zero_retention",` +
			`"training_posture":"no_training","refresh_interval":"24h","request_timeout":"1m",` +
			`"max_retries":5,"max_requests_per_run":20,"max_requests_per_day":100}`)
		rejected := performSettingsRequest(t, srv, http.MethodPut,
			"/api/v1/settings/person-enrichment/providers/new-provider", body, get.Header().Get("ETag"), "")
		assertions.Equal(http.StatusUnprocessableEntity, rejected.Code, rejected.Body.String())
		assertions.Contains(rejected.Body.String(), "validation_failed")
	}
	written, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.NotContains(string(written), "new-provider")
}

func TestSettingsReadsAndRepairsInvalidDisabledPersonEnrichmentProvider(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[people.enrichment]
enabled = false

[[people.enrichment.providers]]
name = "exa-primary"
kind = "exa"
enabled = false
endpoint = "https://provider.example/search?token=example-credential"
api_key_env = "EXA_KEY"
allowed_identifiers = ["name"]
target_keys = ["attribute:bio"]
retention_posture = "zero_retention"
training_posture = "no_training"
refresh_interval = "24h"
max_requests_per_run = 10
max_requests_per_day = 100
`)

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirements.Equal(http.StatusOK, get.Code, get.Body.String())
	assertions.NotContains(get.Body.String(), "token=example-credential")
	var before SettingsResponse
	requirements.NoError(json.Unmarshal(get.Body.Bytes(), &before))
	requirements.Len(before.PersonEnrichmentProviders, 1)
	assertions.Empty(before.PersonEnrichmentProviders[0].Endpoint)
	assertions.Nil(before.PersonEnrichmentProviders[0].Credential)

	repaired := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary", []byte(`{
"kind":"exa",
"enabled":false,
"endpoint":"https://api.exa.example/search",
"mode":"people",
"allowed_identifiers":["name"],
"target_keys":["attribute:bio"],
"allow_sensitive_targets":false,
"retention_posture":"zero_retention",
"training_posture":"no_training",
"refresh_interval":"24h",
"request_timeout":"1m",
"max_retries":5,
"max_requests_per_run":10,
"max_requests_per_day":100
}`), get.Header().Get("ETag"), "")
	requirements.Equal(http.StatusOK, repaired.Code, repaired.Body.String())
	var after SettingsResponse
	requirements.NoError(json.Unmarshal(repaired.Body.Bytes(), &after))
	requirements.Len(after.PersonEnrichmentProviders, 1)
	assertions.Equal("https://api.exa.example/search", after.PersonEnrichmentProviders[0].Endpoint)
	assertions.NotNil(after.PersonEnrichmentProviders[0].Credential)
}
