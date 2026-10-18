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
	"strconv"
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
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, _ := newSettingsTestServer(t, "# keep\n[web]\ntheme = \"dark\"\n"+
		"[server]\napi_key = \"test-api-key\"\n"+
		"[vector.embeddings]\ndocument_prefix = \"search_document: \"\nquery_prefix = \"search_query: \"\n"+
		"[integrations.tasks]\napi_key = \"task-secret\"\n"+
		"[integrations.kata]\napi_key = \"kata-secret\"\n"+
		"[unsupported]\nprivate_value = \"must-not-leak\"\n")
	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "test-api-key")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	assertionsT.NotEmpty(resp.Header().Get("ETag"))
	assertionsT.Equal("no-store", resp.Header().Get("Cache-Control"))

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirementsT.NotNil(byKey["web.theme"].Value)
	requirementsT.NotNil(byKey["web.theme"].Value.String)
	assertionsT.Equal("dark", *byKey["web.theme"].Value.String)
	assertionsT.Equal(&SecretSettingState{Configured: true, Hint: "tes…key"}, byKey["server.api_key"].Secret)
	assertionsT.Nil(byKey["server.api_key"].Value)
	assertionsT.Equal(&SecretSettingState{Configured: true}, byKey["integrations.tasks.api_key"].Secret)
	assertionsT.Equal(&SecretSettingState{Configured: true}, byKey["integrations.kata.api_key"].Secret)
	assertionsT.Nil(byKey["integrations.kata.api_key"].Value)
	requirementsT.NotNil(byKey["vector.embeddings.api_format"].Value)
	requirementsT.NotNil(byKey["vector.embeddings.api_format"].Value.String)
	assertionsT.Equal("openai", *byKey["vector.embeddings.api_format"].Value.String)
	assertionsT.Equal([]string{"openai", "voyage-contextual"}, byKey["vector.embeddings.api_format"].Options)
	requirementsT.NotNil(byKey["vector.embeddings.document_prefix"].Value)
	requirementsT.NotNil(byKey["vector.embeddings.document_prefix"].Value.String)
	assertionsT.Equal("search_document: ", *byKey["vector.embeddings.document_prefix"].Value.String)
	requirementsT.NotNil(byKey["vector.embeddings.query_prefix"].Value)
	requirementsT.NotNil(byKey["vector.embeddings.query_prefix"].Value.String)
	assertionsT.Equal("search_query: ", *byKey["vector.embeddings.query_prefix"].Value.String)
	requirementsT.NotNil(byKey["vector.people.enabled"].Value)
	requirementsT.NotNil(byKey["vector.people.enabled"].Value.Boolean)
	assertionsT.False(*byKey["vector.people.enabled"].Value.Boolean)
	requirementsT.NotNil(byKey["vector.people.retention_posture"].Value)
	requirementsT.NotNil(byKey["vector.people.training_posture"].Value)
	requirementsT.NotNil(byKey["server.trusted_proxies"].Value)
	assertionsT.NotNil(byKey["server.trusted_proxies"].Value.Strings)
	assertionsT.NotContains(byKey, "unsupported.private_value")
	for _, setting := range body.Settings {
		wantRestartRequired := !strings.HasPrefix(setting.Key, "web.") && setting.Key != "carddav.password"
		assertionsT.Equal(wantRestartRequired, setting.RestartRequired, setting.Key)
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
		assertionsT.Equal(wantReadOnly, setting.ReadOnly, setting.Key)
	}
	assertionsT.NotContains(resp.Body.String(), "test-api-key")
	assertionsT.NotContains(resp.Body.String(), "task-secret")
	assertionsT.NotContains(resp.Body.String(), "kata-secret")
	assertionsT.NotContains(resp.Body.String(), "must-not-leak")
}

func TestGetSettingsIsSelfDescribingAndIncludesSafeCatalog(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, "")

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body struct {
		Groups   []map[string]any `json:"groups"`
		Settings []map[string]any `json:"settings"`
	}
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	requirementsT.NotEmpty(body.Groups, "the daemon must describe category labels for generic clients")

	byKey := make(map[string]map[string]any, len(body.Settings))
	for _, setting := range body.Settings {
		key, ok := setting["key"].(string)
		requirementsT.True(ok)
		assertionsT.NotEmpty(setting["label"], "setting %s must have a stable label", key)
		assertionsT.NotEmpty(setting["description"], "setting %s must have a stable description", key)
		byKey[key] = setting
	}
	for _, key := range []string{
		"sync.rate_limit_qps",
		"log.enabled", "log.level", "log.sql_slow_ms", "log.sql_trace",
		"analytics.min_rebuild_interval", "analytics.builder_memory_limit",
		"analytics.builder_threads", "analytics.builder_temp_limit",
		"server.daemon_idle_timeout", "server.daemon_auto_start", "server.daemon_auto_restart",
		"activity.timezone", "activity.max_direct_counterparts", "activity.batch_size", "activity.schedule",
		"backup.zstd_level",
		"beeper.accounts", "beeper.exclude_accounts", "beeper.rate_limit_qps",
		"beeper.media", "beeper.media_scope", "beeper.media_max_participants", "beeper.max_media_mb",
		"slack.enabled", "slack.schedule", "slack.channels", "slack.exclude_channels",
		"slack.dms", "slack.group_dms",
		"slack.media", "slack.media_scope", "slack.media_max_participants", "slack.max_media_mb",
		"discord.media", "discord.media_scope", "discord.media_max_participants", "discord.max_media_mb",
		"teams.media", "teams.media_scope", "teams.media_max_participants", "teams.max_media_mb",
		"vector.embeddings.timeout", "vector.preprocess.strip_quotes", "vector.preprocess.strip_signatures",
		"vector.preprocess.strip_html", "vector.preprocess.strip_base64",
		"vector.preprocess.strip_url_tracking", "vector.preprocess.collapse_whitespace",
		"vector.search.max_page_size_hybrid", "vector.search.sqlite_accelerator",
		"vector.search.ann_nprobe", "vector.search.ann_oversample", "vector.search.ann_threads",
		"vector.embed.backstop_interval",
		"vector.embeddings.api_key", "vector.multimodal.api_key",
		"people.enrichment.enabled", "people.enrichment.schedule", "people.enrichment.batch_size",
		"people.enrichment.lease_duration",
	} {
		assertionsT.Contains(byKey, key)
	}
	for _, key := range []string{
		"server.bind_addr", "server.api_port", "server.api_key", "server.allow_insecure", "server.trusted_proxies",
		"vector.backend", "vector.db_path", "vector.skip_extension_create",
	} {
		setting := byKey[key]
		requirementsT.NotNil(setting, key)
		assertionsT.Equal(true, setting["read_only"], key)
	}
	for _, key := range []string{"chat.server", "chat.model", "chat.max_results"} {
		assertionsT.NotContains(byKey, key, "legacy chat settings have no production consumer")
	}
	assertionsT.Equal(map[string]any{"boolean": true}, byKey["server.daemon_auto_start"]["value"])
}

func TestSlackConversationSelectionSettings(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, "")

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	var before SettingsResponse
	requirementsT.NoError(json.Unmarshal(get.Body.Bytes(), &before))
	byKey := settingsByKey(before.Settings)
	for _, key := range []string{"slack.dms", "slack.group_dms"} {
		setting, ok := byKey[key]
		requirementsT.True(ok, key)
		requirementsT.NotNil(setting.Value, key)
		requirementsT.NotNil(setting.Value.Boolean, key)
		assertionsT.True(*setting.Value.Boolean, key)
		assertionsT.True(setting.Inherited, key)
	}

	patched := patchSettings(t, srv, `{"updates":[`+
		`{"key":"slack.dms","value":{"boolean":false}},`+
		`{"key":"slack.group_dms","value":{"boolean":false}}]}`)
	requirementsT.Equal(http.StatusOK, patched.Code, patched.Body.String())
	loaded, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.False(loaded.Slack.DMsEnabled())
	assertionsT.False(loaded.Slack.GroupDMsEnabled())

	get = performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	var after SettingsResponse
	requirementsT.NoError(json.Unmarshal(get.Body.Bytes(), &after))
	byKey = settingsByKey(after.Settings)
	for _, key := range []string{"slack.dms", "slack.group_dms"} {
		setting := byKey[key]
		requirementsT.NotNil(setting.Value, key)
		requirementsT.NotNil(setting.Value.Boolean, key)
		assertionsT.False(*setting.Value.Boolean, key)
		assertionsT.False(setting.Inherited, key)
	}
}

func TestGetSettingsPublishesValidationMetadataFromRegisteredRouter(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, "")

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body struct {
		Settings []map[string]any `json:"settings"`
	}
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := make(map[string]map[string]any, len(body.Settings))
	for _, setting := range body.Settings {
		key, ok := setting["key"].(string)
		requirementsT.True(ok)
		byKey[key] = setting
	}

	activityBatch, ok := byKey["activity.batch_size"]["validation"].(map[string]any)
	requirementsT.True(ok, "activity.batch_size must publish validation metadata")
	assertionsT.InDelta(float64(1), activityBatch["minimum"], 0)
	assertionsT.InDelta(float64(10_000), activityBatch["maximum"], 0)

	backupLevel, ok := byKey["backup.zstd_level"]["validation"].(map[string]any)
	requirementsT.True(ok, "backup.zstd_level must publish validation metadata")
	assertionsT.InDelta(float64(0), backupLevel["minimum"], 0, "minimum keeps covering the stored off value for older clients")
	assertionsT.InDelta(float64(19), backupLevel["maximum"], 0)
	backupOff, ok := backupLevel["off"].(map[string]any)
	requirementsT.True(ok, "backup.zstd_level must publish zero as its off value")
	assertionsT.Equal("0", backupOff["value"])
	assertionsT.Equal("Encoder default", backupOff["label"])
	assertionsT.InDelta(float64(1), backupOff["on_minimum"], 0)
	assertionsT.Nil(backupLevel["hint"], "bounds belong on the control, not in hint text")

	mediaSize, ok := byKey["discord.max_media_mb"]["validation"].(map[string]any)
	requirementsT.True(ok, "attachment size controls must publish validation metadata")
	assertionsT.InDelta(float64(0), mediaSize["minimum"], 0)
	mediaOff, ok := mediaSize["off"].(map[string]any)
	requirementsT.True(ok, "attachment size controls must publish their provider default as the off state")
	assertionsT.Equal("Discord default of 50 MiB", mediaOff["label"])
	assertionsT.Equal("50", mediaOff["suggest"])
	assertionsT.InDelta(float64(1), mediaOff["on_minimum"], 0)
	chatSize, ok := byKey["beeper.max_media_mb"]["validation"].(map[string]any)
	requirementsT.True(ok)
	chatOff, ok := chatSize["off"].(map[string]any)
	requirementsT.True(ok)
	assertionsT.Equal("Beeper default of 250 MiB", chatOff["label"], "the label follows the shared chat default")
	retries, ok := byKey["vector.embeddings.max_retries"]["validation"].(map[string]any)
	requirementsT.True(ok)
	assertionsT.Nil(retries["off"], "a zero that loading rewrites to the default cannot be an off state")

	embeddingEndpoint, ok := byKey["vector.embeddings.endpoint"]["validation"].(map[string]any)
	requirementsT.True(ok, "provider endpoints must publish safe input guidance")
	assertionsT.Equal(true, embeddingEndpoint["required"])
	assertionsT.Contains(embeddingEndpoint["hint"], "without credentials, query, or fragment")

	activitySchedule, ok := byKey["activity.schedule"]["validation"].(map[string]any)
	requirementsT.True(ok, "schedules must identify their accepted format")
	assertionsT.Equal("cron", activitySchedule["format"])
	assertionsT.NotEqual(true, activitySchedule["required"])
	enrichmentSchedule, ok := byKey["people.enrichment.schedule"]["validation"].(map[string]any)
	requirementsT.True(ok)
	assertionsT.Equal("cron", enrichmentSchedule["format"])
	assertionsT.Equal(true, enrichmentSchedule["required"])
	assertionsT.NotEqual(true, byKey["integrations.tasks.endpoint"]["testable"],
		"the daemon has no provider endpoint test operation")
}

func TestSettingsCatalogDoesNotPublishGenericMetadataFallbacks(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	for _, definition := range settingsCatalog {
		_, ok := settingsMetadata[definition.key]
		assertionsT.True(ok, "%s needs an intentional label and description", definition.key)
	}
}

func TestSettingsProviderCredentialsAreWriteOnlyOwnerOnlyAndETagProtected(t *testing.T) { //nolint:paralleltest // t.Setenv writes provider API key variables
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	t.Setenv("TEXT_EMBEDDING_KEY", "environment-secret-must-not-leak")
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_EMBEDDING_KEY"
model = "synthetic-model"
dimension = 8
`)

	first := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, first.Code, first.Body.String())
	assertionsT.NotContains(first.Body.String(), "environment-secret-must-not-leak")
	assertionsT.Equal(map[string]any{"configured": true, "source": "environment", "hint": "env…eak"},
		rawEmbeddingSecretState(t, first.Body.Bytes()))
	configETag := first.Header().Get("ETag")
	credentialETag := first.Header().Get("Credential-Etag")
	requirementsT.NotEmpty(configETag)
	requirementsT.NotEmpty(credentialETag)

	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.embeddings",
		[]byte(`{"value":"browser-secret-must-not-leak"}`), credentialETag, "")
	requirementsT.Equal(http.StatusOK, set.Code, set.Body.String())
	var setResponse ProviderCredentialResponse
	requirementsT.NoError(json.Unmarshal(set.Body.Bytes(), &setResponse))
	assertionsT.True(setResponse.PendingRestart)
	assertionsT.NotContains(set.Body.String(), "browser-secret-must-not-leak")
	assertionsT.NotContains(set.Body.String(), "environment-secret-must-not-leak")
	storedCredentialETag := set.Header().Get("ETag")
	assertionsT.NotEqual(credentialETag, storedCredentialETag)

	stored := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, stored.Code, stored.Body.String())
	assertionsT.Equal(map[string]any{"configured": true, "source": "stored", "hint": "bro…eak"},
		rawEmbeddingSecretState(t, stored.Body.Bytes()))
	assertionsT.Equal(configETag, stored.Header().Get("ETag"), "credential writes must not masquerade as config writes")
	assertionsT.Equal(storedCredentialETag, stored.Header().Get("Credential-Etag"))

	credentialPath := filepath.Join(srv.cfg.TokensDir(), "provider-credentials.json")
	info, err := os.Stat(credentialPath)
	requirementsT.NoError(err)
	if runtime.GOOS != "windows" {
		assertionsT.Equal(os.FileMode(0o600), info.Mode().Perm())
		dirInfo, statErr := os.Stat(srv.cfg.TokensDir())
		requirementsT.NoError(statErr)
		assertionsT.Equal(os.FileMode(0o700), dirInfo.Mode().Perm())
	}
	credentialBytes, err := os.ReadFile(credentialPath)
	requirementsT.NoError(err)
	assertionsT.Contains(string(credentialBytes), "browser-secret-must-not-leak")

	stale := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, credentialETag, "")
	assertionsT.Equal(http.StatusPreconditionFailed, stale.Code, stale.Body.String())

	clearResponseRecorder := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, storedCredentialETag, "")
	requirementsT.Equal(http.StatusOK, clearResponseRecorder.Code, clearResponseRecorder.Body.String())
	var clearResponse ProviderCredentialResponse
	requirementsT.NoError(json.Unmarshal(clearResponseRecorder.Body.Bytes(), &clearResponse))
	assertionsT.True(clearResponse.PendingRestart)
	cleared := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
	assertionsT.Equal(map[string]any{"configured": true, "source": "environment", "hint": "env…eak"},
		rawEmbeddingSecretState(t, cleared.Body.Bytes()))
	assertionsT.NotContains(cleared.Body.String(), "environment-secret-must-not-leak")
}

func TestPatchSettingsPersistsSafeScalarAndAttachmentPolicies(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
		{"key": "server.daemon_auto_start", "value": map[string]any{"boolean": false}},
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
	requirementsT.NoError(err)

	resp := patchSettings(t, srv, string(body))
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var updated SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &updated))
	assertionsT.Equal(&SettingValue{Boolean: new(false)}, settingsByKey(updated.Settings)["server.daemon_auto_start"].Value)
	loaded, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.False(loaded.Server.DaemonAutoStartEnabled())
	assertionsT.Equal(12, loaded.Sync.RateLimitQPS)
	assertionsT.Equal("debug", loaded.Log.Level)
	assertionsT.Equal(int64(250), loaded.Log.SQLSlowMs)
	assertionsT.Equal(3, loaded.Analytics.BuilderThreads)
	assertionsT.Equal("America/New_York", loaded.Activity.Timezone)
	assertionsT.Equal(7, loaded.Backup.ZstdLevel)
	assertionsT.Equal([]string{"signal"}, loaded.Beeper.Accounts)
	assertionsT.Equal([]string{"whatsapp"}, loaded.Beeper.ExcludeAccounts)
	assertionsT.InDelta(8.5, loaded.Beeper.RateLimitQPS, 0.001)
	assertionsT.False(loaded.Beeper.MediaEnabled())
	assertionsT.Equal("direct", loaded.Beeper.MediaScope)
	assertionsT.Equal(5, loaded.Beeper.MediaMaxParticipants)
	assertionsT.Equal(80, loaded.Beeper.MaxMediaMB)
	assertionsT.Equal("none", loaded.Slack.MediaScope)
	assertionsT.Equal(90, loaded.Slack.MaxMediaMB)
	assertionsT.Equal(10, loaded.Discord.MediaMaxParticipants)
	assertionsT.Equal(60, loaded.Teams.MaxMediaMB)
	assertionsT.False(loaded.Vector.Preprocess.StripQuotesEnabled())
	assertionsT.Equal(0, loaded.Vector.Search.MaxPageSizeHybridClamp())
}

func TestPatchSettingsRejectsInvalidAttachmentPolicy(t *testing.T) {
	t.Parallel()
	srv, _ := newSettingsTestServer(t, "")
	resp := patchSettings(t, srv,
		`{"updates":[{"key":"teams.media_max_participants","value":{"integer":-1}}]}`)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.NotContains(t, resp.Body.String(), "provider-credentials")
}

func TestPatchSettingsRejectsHostAndAuthSettings(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
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
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirementsT.NoError(err)
	requirementsT.Len(loaded.People.Enrichment.Providers, 2)
	assertionsT.Equal("exa-primary", loaded.People.Enrichment.Providers[0].Name)
	assertionsT.True(loaded.People.Enrichment.Providers[0].Enabled)
	assertionsT.Equal(int64(20), loaded.People.Enrichment.Providers[0].MaxRequestsPerRun)
	assertionsT.Equal("sixtyfour-primary", loaded.People.Enrichment.Providers[1].Name)
	assertionsT.Equal(int64(50), loaded.People.Enrichment.Providers[1].MaxRequestsPerDay)
}

func TestPatchSettingsFirstEnrichmentEnableGeneratesPrivateSuppressionKey(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.True(loaded.People.Enrichment.Enabled)
	assertionsT.NotEmpty(loaded.People.Enrichment.SuppressionKeyEnv)
	credentialBytes, err := os.ReadFile(filepath.Join(loaded.TokensDir(), "provider-credentials.json"))
	requirementsT.NoError(err)
	credentials, err := providercredentials.Read(loaded.TokensDir())
	requirementsT.NoError(err)
	suppressionKey, configured, err := credentials.ResolveSuppression()
	requirementsT.NoError(err)
	requirementsT.True(configured)
	assertionsT.NotContains(resp.Body.String(), suppressionKey)
	assertionsT.Greater(len(credentialBytes), 64)
}

func TestPatchSettingsFirstEnrichmentEnableUsesStoredSuppressionFirst(t *testing.T) { //nolint:paralleltest // process environment
	const storedKey = "stored-suppression-key-32bytes-123"
	const environmentKey = "environment-suppression-key-32bytes"
	for _, tc := range []struct {
		name, stored, environment string
		wantStatus                int
	}{
		{"stored with missing environment", storedKey, "", http.StatusOK},
		{"stored with short environment", storedKey, "short", http.StatusOK},
		{"environment fallback", "", environmentKey, http.StatusOK},
		{"missing environment without stored key", "", "", http.StatusUnprocessableEntity},
		{"short environment without stored key", "", "short", http.StatusUnprocessableEntity},
		{"invalid stored key with valid environment", "short", environmentKey, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertionsT := assert.New(t)
			requirementsT := require.New(t)
			const environmentName = "MSGVAULT_TEST_SETTINGS_SUPPRESSION_KEY"
			t.Setenv(environmentName, tc.environment)
			if tc.environment == "" {
				requirementsT.NoError(os.Unsetenv(environmentName))
			}
			srv, path := newSettingsTestServer(t, `[people.enrichment]
enabled = false
suppression_key_env = "MSGVAULT_TEST_SETTINGS_SUPPRESSION_KEY"

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
			credentials, err := providercredentials.Read(srv.cfg.TokensDir())
			requirementsT.NoError(err)
			if tc.stored != "" {
				credentials, err = providercredentials.PutSuppression(srv.cfg.TokensDir(), credentials.ETag, tc.stored)
				requirementsT.NoError(err)
			}

			resp := patchSettings(t, srv,
				`{"updates":[{"key":"people.enrichment.enabled","value":{"boolean":true}}]}`)
			requirementsT.Equal(tc.wantStatus, resp.Code, resp.Body.String())
			loaded, err := config.Load(path, "")
			requirementsT.NoError(err)
			assertionsT.Equal(tc.wantStatus == http.StatusOK, loaded.People.Enrichment.Enabled)
			if tc.wantStatus == http.StatusOK {
				wantEnvironment := environmentName
				if tc.stored != "" {
					wantEnvironment = providercredentials.StoredSuppressionEnvironment
				}
				assertionsT.Equal(wantEnvironment, loaded.People.Enrichment.SuppressionKeyEnv)
			}
			after, err := providercredentials.Read(srv.cfg.TokensDir())
			requirementsT.NoError(err)
			assertionsT.Equal(credentials.ETag, after.ETag, "enabling must not replace an existing key or store the environment fallback")
		})
	}
}

func TestPatchSettingsRejectedFirstEnrichmentEnableLeavesCredentialStoreUnchanged(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		editorErr  error
		wantStatus int
	}{
		{name: "invalid candidate", editorErr: config.ErrInvalidConfigCandidate, wantStatus: http.StatusUnprocessableEntity},
		{name: "stale config", editorErr: config.ErrConfigConflict, wantStatus: http.StatusPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirementsT := require.New(t)
			assertionsT := assert.New(t)
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
			assertionsT.Equal(test.wantStatus, resp.Code, resp.Body.String())
			credentials, err := providercredentials.Read(srv.cfg.TokensDir())
			requirementsT.NoError(err)
			_, configured, err := credentials.ResolveSuppression()
			requirementsT.NoError(err)
			assertionsT.False(configured)
		})
	}
}

func TestSettingsProviderCredentialStoreFailsClosedWhenUnsafeOrCorrupt(t *testing.T) { //nolint:paralleltest // t.Setenv writes provider API key variables
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	t.Setenv("TEXT_EMBEDDING_KEY", "environment-fallback-must-not-be-used")
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_EMBEDDING_KEY"
model = "synthetic-model"
dimension = 8
`)
	requirementsT.NoError(os.MkdirAll(srv.cfg.TokensDir(), 0o700))
	path := filepath.Join(srv.cfg.TokensDir(), "provider-credentials.json")
	requirementsT.NoError(os.WriteFile(path, []byte(`{"version":1,"credentials":`), 0o600))

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	assertionsT.Equal(http.StatusInternalServerError, resp.Code, resp.Body.String())
	assertionsT.NotContains(resp.Body.String(), "environment-fallback-must-not-be-used")
	assertionsT.NotContains(resp.Body.String(), "provider-credentials.json")
}

func TestSettingsStoredCredentialIsBoundToEndpointOrigin(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://first.example.test/v1"
api_key_env = "UNSET_TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/vector.embeddings",
		[]byte(`{"value":"origin-bound-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirementsT.Equal(http.StatusOK, set.Code, set.Body.String())

	changed := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://second.example.test/v1"}}]}`),
		get.Header().Get("ETag"), "")
	requirementsT.Equal(http.StatusOK, changed.Code, changed.Body.String())
	assertionsT.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, changed.Body.Bytes()))
	assertionsT.NotContains(changed.Body.String(), "origin-bound-secret")
}

func TestPatchSettingsHardensSecretBearingConfigFile(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows owner-only DACL coverage lives in platform-specific config tests")
	}
	srv, path := newSettingsTestServer(t, "[server]\napi_key = \"secret\"\n[web]\ntheme = \"system\"\n")
	requirementsT.NoError(os.Chmod(path, 0o644))

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "secret")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	patched := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`),
		resp.Header().Get("ETag"), "secret")
	requirementsT.Equal(http.StatusOK, patched.Code, patched.Body.String())
	info, err := os.Stat(path)
	requirementsT.NoError(err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPatchSettingsPreservesUntouchedMediaPointerAndOpaqueOverrides(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, `[discord]
media_scope = "all"
[discord.guilds.G01]
media = false
max_media_mb = 30
`)
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	var document struct {
		Settings []map[string]any `json:"settings"`
	}
	requirementsT.NoError(json.Unmarshal(get.Body.Bytes(), &document))
	for _, setting := range document.Settings {
		if setting["key"] == "discord.media" {
			assertionsT.Equal(true, setting["inherited"], "omitted provider policy must be identified as inherited/default")
			assertionsT.Contains(setting["description"], "future", "media policy copy must say it only affects future syncs")
		}
		if setting["key"] == "discord.max_media_mb" {
			validation, ok := setting["validation"].(map[string]any)
			requirementsT.True(ok, "discord.max_media_mb must publish its provider default as its off state")
			off, ok := validation["off"].(map[string]any)
			requirementsT.True(ok)
			assertionsT.Equal("Discord default of 50 MiB", off["label"])
		}
	}

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"discord.media_scope","value":{"string":"direct"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	loaded, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.Nil(loaded.Discord.Media, "untouched provider default must remain omitted")
	requirementsT.Contains(loaded.Discord.Guilds, "G01")
	assertionsT.NotNil(loaded.Discord.Guilds["G01"].Media)
	assertionsT.False(*loaded.Discord.Guilds["G01"].Media)
	assertionsT.Equal(30, loaded.Discord.Guilds["G01"].MaxMediaMB)
}

func TestSettingsRejectsAndRedactsCredentialBearingEmbeddingEndpoints(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://user:legacy-password@embeddings.example.test/v1"
api_key_env = "TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)

	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	assertionsT.Equal(http.StatusInternalServerError, get.Code, get.Body.String())
	assertionsT.NotContains(get.Body.String(), "legacy-password")
	assertionsT.NotContains(get.Body.String(), "user:")

	clean, _ := newSettingsTestServer(t, `[vector.embeddings]
endpoint = "https://embeddings.example.test/v1"
api_key_env = "TEXT_KEY"
model = "synthetic-model"
dimension = 8
`)
	first := performSettingsRequest(t, clean, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, first.Code, first.Body.String())
	patch := performSettingsRequest(t, clean, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://user:new-password@other.example.test/v1?api_key=query-secret#fragment-secret"}}]}`),
		first.Header().Get("ETag"), "")
	assertionsT.Equal(http.StatusUnprocessableEntity, patch.Code, patch.Body.String())
	for _, secret := range []string{"new-password", "query-secret", "fragment-secret", "user:"} {
		assertionsT.NotContains(patch.Body.String(), secret)
	}
}

func TestPatchSettingsExposesCompleteSemanticPersonOptInPolicy(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
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
	requirementsT.Equal(http.StatusOK, response.Code, response.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "people.enabled = true")
	assertionsT.Contains(string(got), `people.retention_posture = "zero_data_retention"`)
	assertionsT.Contains(string(got), `people.training_posture = "no_training"`)
}

func TestGetSettingsExposesReadOnlyCardDAVAccountStateWithoutCredential(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)

	srv, _ := newSettingsTestServer(t, `[carddav]
base_url = "https://contacts.example/dav"
username = "alice"
schedule = "0 3 * * *"
enabled = true
trusted_origin = "https://contacts.example"
trusted_addresses = ["10.1.2.3"]
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
	requirementsT.NoError(err)
	requirementsT.NoError(carddav.SaveCredential(srv.cfg.TokensDir(), carddav.Credential{
		Password: "must-not-cross-api", BaseURL: srv.cfg.CardDAV.BaseURL,
		Username: srv.cfg.CardDAV.Username, ConnectionGeneration: account.ConnectionGeneration,
	}))
	srv.cardDAV, err = NewCardDAVController(srv.cfg, st, slog.New(slog.DiscardHandler))
	requirementsT.NoError(err)

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	for _, key := range []string{"carddav.base_url", "carddav.username", "carddav.schedule", "carddav.enabled", "carddav.password"} {
		requirementsT.Contains(byKey, key)
		assertionsT.True(byKey[key].ReadOnly, key)
	}
	assertionsT.Equal(&SecretSettingState{Configured: true}, byKey["carddav.password"].Secret)
	assertionsT.NotContains(resp.Body.String(), "must-not-cross-api")
	assertionsT.NotContains(resp.Body.String(), "10.1.2.3")
	assertionsT.NotContains(byKey, "carddav.trusted_origin")
	assertionsT.NotContains(byKey, "carddav.trusted_addresses")

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"carddav.enabled","value":{"boolean":false}}]}`),
		resp.Header().Get("ETag"), "")
	assertionsT.Equal(http.StatusBadRequest, patch.Code, patch.Body.String())
}

func TestGetSettingsReportsStaleCardDAVCredentialAsNotConfigured(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)

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
	requirementsT.NoError(err)
	requirementsT.NoError(carddav.SaveCredential(srv.cfg.TokensDir(), carddav.Credential{
		Password: "stale-password", BaseURL: srv.cfg.CardDAV.BaseURL,
		Username: srv.cfg.CardDAV.Username, ConnectionGeneration: account.ConnectionGeneration,
	}))
	discovery.CredentialsChanged = true
	account, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	requirementsT.NoError(err)
	assertionsT.Equal(int64(2), account.ConnectionGeneration)
	srv.cardDAV, err = NewCardDAVController(srv.cfg, st, slog.New(slog.DiscardHandler))
	requirementsT.NoError(err)

	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertionsT.Equal(&SecretSettingState{Configured: false}, settingsByKey(body.Settings)["carddav.password"].Secret)
	assertionsT.NotContains(resp.Body.String(), "stale-password")
}

func TestPatchSettingsSelectsVoyageContextualEmbeddingFormat(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "[vector.embeddings]\n"+
		"endpoint = \"https://api.voyageai.com/v1\"\n"+
		"model = \"text-embedding-test\"\n"+
		"dimension = 1024\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.api_format","value":{"string":"voyage-contextual"}},{"key":"vector.embeddings.model","value":{"string":"voyage-context-4"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), `api_format = "voyage-contextual"`)
	assertionsT.Contains(string(got), `model = "voyage-context-4"`)
}

func TestGetSettingsExposesMultimodalPolicyWithoutCredentialState(t *testing.T) { //nolint:paralleltest // t.Setenv writes provider API key variables
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	t.Setenv("SYNTHETIC_VOYAGE_KEY", "synthetic-key-value")
	srv, _ := newSettingsTestServer(t, `[vector.multimodal]
enabled = true
api_key_env = "SYNTHETIC_VOYAGE_KEY"
include_images = false
include_video = true
`)
	resp := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirementsT.NotNil(byKey["vector.multimodal.enabled"].Value.Boolean)
	assertionsT.True(*byKey["vector.multimodal.enabled"].Value.Boolean)
	requirementsT.NotNil(byKey["vector.multimodal.include_images"].Value.Boolean)
	assertionsT.False(*byKey["vector.multimodal.include_images"].Value.Boolean)
	requirementsT.NotNil(byKey["vector.multimodal.include_video"].Value.Boolean)
	assertionsT.True(*byKey["vector.multimodal.include_video"].Value.Boolean)
	assertionsT.True(byKey["vector.multimodal.api_key_env"].ReadOnly)
	assertionsT.NotContains(resp.Body.String(), "synthetic-key-value")
}

func TestPatchSettingsRequiresMatchingETag(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")

	missing := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")
	assertionsT.Equal(http.StatusPreconditionRequired, missing.Code, missing.Body.String())

	mismatch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "\"sha256-stale\"", "")
	assertionsT.Equal(http.StatusPreconditionFailed, mismatch.Code, mismatch.Body.String())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assertionsT.Equal("[web]\ntheme = \"system\"\n", string(got))
}

func TestPatchSettingsPreservesFileAndReturnsNewETag(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "# operator comment\n[unknown]\nkeep = true\n\n"+
		"[web]\ntheme = \"system\" # display\n")
	if runtime.GOOS != "windows" {
		requirementsT.NoError(os.Chmod(path, 0o640))
	}
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	etag := get.Header().Get("ETag")

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), etag, "")
	requirementsT.Equal(http.StatusOK, patch.Code, patch.Body.String())
	assertionsT.NotEqual(etag, patch.Header().Get("ETag"))
	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Equal("# operator comment\n[unknown]\nkeep = true\n\n[web]\ntheme = \"dark\" # display\n", string(got))
	if runtime.GOOS != "windows" {
		// Settings publication hardens the entire secret-bearing config to
		// owner-only. Windows security lives in the DACL, which
		// the config package's own Windows tests verify; Stat mode bits there
		// are synthetic.
		info, err := os.Stat(path)
		requirementsT.NoError(err)
		assertionsT.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestPatchSettingsValidatesWholeCandidateAndRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	assertionsT := assert.New(t)
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

		assertionsT.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
		assertionsT.Contains(resp.Body.String(), "host-managed")
		assertionsT.NotContains(resp.Body.String(), "new-key")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assertionsT.Equal("[server]\napi_key = \"old-key\"\n", string(got))
	}
}

func TestPatchSettingsClearsTaskAPIKeyWhenEndpointOriginChanges(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertionsT.Contains(string(got), "api_key = \"\"")
	assertionsT.NotContains(string(got), "task-secret")

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	assertionsT.Equal(&SecretSettingState{Configured: false}, byKey["integrations.tasks.api_key"].Secret)
	assertionsT.True(body.PendingRestart)
}

func TestPatchSettingsKataConfig(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		replacement string
		wantKey     string
	}{
		{name: "new origin clears credential", endpoint: "https://other.example.com"},
		{name: "same origin keeps credential", endpoint: "https://kata.example.com/v2", wantKey: "kata-secret"},
		{name: "new origin with replacement", endpoint: "https://other.example.com", replacement: "new-kata-secret", wantKey: "new-kata-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertionsT := assert.New(t)
			requirementsT := require.New(t)
			srv, path := newSettingsTestServer(t,
				"[integrations.tasks]\napi_key = 'task-secret'\n"+
					"[integrations.kata]\nendpoint = 'https://kata.example.com'\napi_key = 'kata-secret'\n")
			updates := fmt.Sprintf(`{"key":"integrations.kata.endpoint","value":{"string":%q}},`+
				`{"key":"integrations.kata.enabled","value":{"boolean":true}},`+
				`{"key":"integrations.kata.default_project","value":{"string":"people"}}`, tt.endpoint)
			if tt.replacement != "" {
				updates += fmt.Sprintf(`,{"key":"integrations.kata.api_key","secret":{"action":"set","value":%q}}`, tt.replacement)
			}
			resp := patchSettings(t, srv, `{"updates":[`+updates+`]}`)
			requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
			cfg, err := config.Load(path, "")
			requirementsT.NoError(err)
			assertionsT.Equal(config.TaskIntegrationConfig{
				Enabled: true, Endpoint: tt.endpoint, APIKey: tt.wantKey, DefaultProject: "people",
			}, cfg.Integrations.Kata)
			assertionsT.Equal("task-secret", cfg.Integrations.Tasks.APIKey)
			var body SettingsResponse
			requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
			assertionsT.True(body.PendingRestart)
			assertionsT.NotContains(resp.Body.String(), "kata-secret")
			assertionsT.NotContains(resp.Body.String(), "task-secret")
		})
	}
}

func TestPatchSettingsKeepsNewTaskAPIKeyProvidedWithEndpointChange(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[`+
			`{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}},`+
			`{"key":"integrations.tasks.api_key","secret":{"action":"set","value":"rotated-secret"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertionsT.Contains(string(got), "api_key = \"rotated-secret\"")

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertionsT.Equal(&SecretSettingState{Configured: true, Hint: "rot…ret"},
		settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
}

func TestPatchSettingsRetainsTaskAPIKeyWhenEndpointOriginIsUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "identical endpoint", endpoint: "https://tasks.example.com/api"},
		{name: "same origin different path", endpoint: "https://tasks.example.com/v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertionsT := assert.New(t)
			requirementsT := require.New(t)
			srv, path := newSettingsTestServer(t,
				"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\napi_key = \"task-secret\"\n")

			resp := patchSettings(t, srv, fmt.Sprintf(
				`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":%q}}]}`, tt.endpoint))
			requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

			got, err := os.ReadFile(path)
			requirementsT.NoError(err)
			assertionsT.Contains(string(got), "api_key = \"task-secret\"")

			var body SettingsResponse
			requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
			assertionsT.Equal(&SecretSettingState{Configured: true},
				settingsByKey(body.Settings)["integrations.tasks.api_key"].Secret)
		})
	}
}

func TestPatchSettingsEndpointChangeWithoutStoredCredentialAddsNoKey(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[integrations.tasks]\nendpoint = \"https://tasks.example.com/api\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"integrations.tasks.endpoint","value":{"string":"https://elsewhere.example.net/api"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "endpoint = \"https://elsewhere.example.net/api\"")
	assertionsT.NotContains(string(got), "api_key")
}

func TestPatchSettingsRetainsEmbeddingsAPIKeyEnvWhenEndpointOriginChanges(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t,
		"[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://elsewhere.example.net/v1"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "endpoint = \"https://elsewhere.example.net/v1\"")
	assertionsT.Contains(string(got), "api_key_env = \"MSGVAULT_EMBED_API_KEY\"")

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	byKey := settingsByKey(body.Settings)
	requirementsT.NotNil(byKey["vector.embeddings.api_key_env"].Value)
	requirementsT.NotNil(byKey["vector.embeddings.api_key_env"].Value.String)
	assertionsT.Equal("MSGVAULT_EMBED_API_KEY", *byKey["vector.embeddings.api_key_env"].Value.String)
}

func TestPatchSettingsRetainsMultimodalAPIKeyEnvWhenEndpointOriginChanges(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t,
		"[vector.multimodal]\nendpoint = \"https://api.voyageai.com/v1\"\n"+
			"api_key_env = \"SYNTHETIC_VOYAGE_KEY\"\n")

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.multimodal.endpoint","value":{"string":"https://voyage.example.test/v1"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), `endpoint = "https://voyage.example.test/v1"`)
	assertionsT.Contains(string(got), `api_key_env = "SYNTHETIC_VOYAGE_KEY"`)

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	keySetting := settingsByKey(body.Settings)["vector.multimodal.api_key_env"]
	requirementsT.NotNil(keySetting.Value)
	requirementsT.NotNil(keySetting.Value.String)
	assertionsT.Equal("SYNTHETIC_VOYAGE_KEY", *keySetting.Value.String)
}

func TestPatchSettingsEditableChangeSucceedsWhileReadOnlySettingIsConfigured(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n"+
		"[vector.embeddings]\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n")

	resp := patchSettings(t, srv, `{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`)
	requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Contains(string(got), "theme = \"dark\"")
	assertionsT.Contains(string(got), "api_key_env = \"MSGVAULT_EMBED_API_KEY\"")

	var body SettingsResponse
	requirementsT.NoError(json.Unmarshal(resp.Body.Bytes(), &body))
	assertionsT.True(settingsByKey(body.Settings)["vector.embeddings.api_key_env"].ReadOnly)
}

func TestPatchSettingsRejectsEmbeddingsAPIKeyEnvUpdates(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	before := "[vector.embeddings]\nendpoint = \"https://embed.example.com/v1\"\napi_key_env = \"MSGVAULT_EMBED_API_KEY\"\n"
	srv, path := newSettingsTestServer(t, before)

	resp := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.api_key_env","value":{"string":"AWS_SECRET_ACCESS_KEY"}}]}`)
	requirementsT.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
	assertionsT.Contains(resp.Body.String(), "host-managed")

	got, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Equal(before, string(got))
}

func TestSettingsErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]}`), "", "")

	assert.Equal(t, http.StatusPreconditionRequired, resp.Code, resp.Body.String())
	assert.Equal(t, "no-store", resp.Header().Get("Cache-Control"))
}

func TestSettingsMiddlewareErrorsAreNotCached(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	srv, _ := newSettingsTestServer(t, "[web]\ntheme = \"system\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	resp := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"web.theme","value":{"string":"dark"}}]} {}`),
		get.Header().Get("ETag"), "")

	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

func TestPatchSettingsClassifiesFilesystemFailureAsServerError(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "[server]\ndaemon_idle_timeout = \"15m\"\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	srv.settingsConfigEditor = func(configPath, ifMatch string, edits []config.Edit) (config.ConfigFile, error) {
		requirementsT.Equal(path, configPath)
		requirementsT.Equal(get.Header().Get("ETag"), ifMatch)
		requirementsT.Len(edits, 1)
		requirementsT.NoError(os.WriteFile(path, []byte("[server]\ndaemon_idle_timeout = \"1h\"\n"), 0o600))
		return config.ConfigFile{}, fmt.Errorf("%w: cleanup failed", config.ErrConfigChanged)
	}

	patch := performSettingsRequest(t, srv, http.MethodPatch, settingsPath,
		[]byte(`{"updates":[{"key":"server.daemon_idle_timeout","value":{"string":"1h"}}]}`),
		get.Header().Get("ETag"), "")
	assertionsT.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())

	after := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, after.Code, after.Body.String())
	var persisted SettingsResponse
	requirementsT.NoError(json.Unmarshal(after.Body.Bytes(), &persisted))
	assertionsT.True(persisted.PendingRestart)
}

func TestPatchSettingsMarksRestartPendingBeforeLoadingCommittedSnapshot(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
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
	assertionsT.Equal(http.StatusInternalServerError, patch.Code, patch.Body.String())
	assertionsT.True(srv.settingsPendingRestart.Load())
}

func TestPatchSettingsPrefersChangedOutcomeOverConflictClassification(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	doc := OpenAPIDocument()
	requirementsT.NotNil(doc.Paths[settingsPath])
	get := doc.Paths[settingsPath].Get
	patch := doc.Paths[settingsPath].Patch
	requirementsT.NotNil(get)
	requirementsT.NotNil(patch)
	assertionsT.Contains(get.Responses["200"].Headers, "ETag")
	requirementsT.Len(patch.Parameters, 1)
	assertionsT.Equal("If-Match", patch.Parameters[0].Name)
	assertionsT.Equal("header", patch.Parameters[0].In)
	assertionsT.True(patch.Parameters[0].Required)
	for _, status := range []string{"400", "409", "412", "422", "428"} {
		assertionsT.Contains(patch.Responses, status)
	}
	assertionsT.Equal(APISchemaVersion, doc.Info.Version)

	settingValue := doc.Components.Schemas.Map()["SettingValue"]
	requirementsT.NotNil(settingValue)
	assertionsT.Len(settingValue.OneOf, 5)
	assertionsT.Empty(settingValue.Properties)
	for _, arm := range settingValue.OneOf {
		assertionsT.Len(arm.Required, 1)
		assertionsT.Equal([]string{arm.Required[0]}, arm.Required)
		assertionsT.Equal(false, arm.AdditionalProperties)
	}
	setting := doc.Components.Schemas.Map()["Setting"]
	requirementsT.NotNil(setting)
	assertionsT.ElementsMatch([]any{
		"browser", "server", "archive", "search", "sources", "attachments", "enrichment", "integrations",
		"sync", "logging", "activity", "backup",
	}, setting.Properties["group"].Enum)
	for _, group := range settingsGroups {
		assertionsT.NotContains(legacySettingsGroupIDs, group.ID, "legacy IDs are compatibility-only")
	}
	assertionsT.NotNil(setting.Properties["section"], "settings publish their section for sectioned groups")
	assertionsT.ElementsMatch([]any{"string", "integer", "number", "boolean", "string_array", "secret"}, setting.Properties["kind"].Enum)
	patchRequest := doc.Components.Schemas.Map()["SettingsPatchRequest"]
	requirementsT.NotNil(patchRequest)
	assertionsT.False(patchRequest.Properties["updates"].Nullable)
	settingsResponse := doc.Components.Schemas.Map()["SettingsResponse"]
	requirementsT.NotNil(settingsResponse)
	assertionsT.False(settingsResponse.Properties["groups"].Nullable)
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
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, settingsStoredVectorConfig)
	storeVectorEmbeddingsCredential(t, srv, "origin-bound-secret")

	samePath := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://first.example.test/v2"}}]}`)
	requirementsT.Equal(http.StatusOK, samePath.Code, samePath.Body.String())
	assertionsT.Equal(map[string]any{"configured": true, "source": "stored", "hint": "ori…ret"},
		rawEmbeddingSecretState(t, samePath.Body.Bytes()))
	retained, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.True(retained.Stored(providercredentials.VectorEmbeddingsID))

	moved := patchSettings(t, srv,
		`{"updates":[{"key":"vector.embeddings.endpoint","value":{"string":"https://second.example.test/v1"}}]}`)
	requirementsT.Equal(http.StatusOK, moved.Code, moved.Body.String())
	assertionsT.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, moved.Body.Bytes()))
	assertionsT.NotContains(moved.Body.String(), "origin-bound-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.False(severed.Stored(providercredentials.VectorEmbeddingsID))
	assertionsT.Equal(severed.ETag, moved.Header().Get("Credential-Etag"))

	after := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, after.Code, after.Body.String())
	assertionsT.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, after.Body.Bytes()))
	credentialBytes, err := os.ReadFile(filepath.Join(srv.cfg.TokensDir(), providercredentials.Filename))
	requirementsT.NoError(err)
	assertionsT.NotContains(string(credentialBytes), "origin-bound-secret")
}

func TestDeleteProviderCredentialOutlivesConfiguredProvider(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, settingsStoredVectorConfig)
	credentialETag := storeVectorEmbeddingsCredential(t, srv, "orphaned-secret")

	requirementsT.NoError(os.WriteFile(path, []byte("[analytics]\nengine = \"auto\"\n"), 0o600))

	deleted := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, credentialETag, "")
	requirementsT.Equal(http.StatusOK, deleted.Code, deleted.Body.String())
	var response ProviderCredentialResponse
	requirementsT.NoError(json.Unmarshal(deleted.Body.Bytes(), &response))
	assertionsT.Equal(SecretSettingState{Configured: false, Source: "none"}, response.State)
	remaining, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.False(remaining.Stored(providercredentials.VectorEmbeddingsID))
	assertionsT.Equal(remaining.ETag, deleted.Header().Get("ETag"))

	missing := performSettingsRequest(t, srv, http.MethodDelete,
		"/api/v1/settings/provider-credentials/vector.embeddings", nil, remaining.ETag, "")
	assertionsT.Equal(http.StatusNotFound, missing.Code, missing.Body.String())
	assertionsT.Contains(missing.Body.String(), "credential_not_found")
}

func TestPutPersonEnrichmentProviderSeversStoredCredentialWhenOriginChanges(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	credentialID := providercredentials.PersonEnrichmentID("exa-primary")
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(credentialID),
		[]byte(`{"value":"exa-origin-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirementsT.Equal(http.StatusOK, set.Code, set.Body.String())

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
	requirementsT.Equal(http.StatusOK, samePath.Code, samePath.Body.String())
	var retainedResponse SettingsResponse
	requirementsT.NoError(json.Unmarshal(samePath.Body.Bytes(), &retainedResponse))
	requirementsT.Len(retainedResponse.PersonEnrichmentProviders, 1)
	assertionsT.Equal(&SecretSettingState{Configured: true, Source: "stored", Hint: "exa…ret"},
		retainedResponse.PersonEnrichmentProviders[0].Credential)

	moved := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/person-enrichment/providers/exa-primary",
		providerUpdate("https://elsewhere.example/search"), samePath.Header().Get("ETag"), "")
	requirementsT.Equal(http.StatusOK, moved.Code, moved.Body.String())
	var severedResponse SettingsResponse
	requirementsT.NoError(json.Unmarshal(moved.Body.Bytes(), &severedResponse))
	requirementsT.Len(severedResponse.PersonEnrichmentProviders, 1)
	assertionsT.Equal(&SecretSettingState{Configured: false, Source: "none"},
		severedResponse.PersonEnrichmentProviders[0].Credential)
	assertionsT.NotContains(moved.Body.String(), "exa-origin-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.False(severed.Stored(credentialID))
	assertionsT.Equal(severed.ETag, moved.Header().Get("Credential-Etag"))
}

func TestPutPersonEnrichmentProviderRollsBackConfigWhenCredentialCleanupConflicts(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	credentialID := providercredentials.PersonEnrichmentID("exa-primary")
	set := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(credentialID),
		[]byte(`{"value":"origin-bound-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirementsT.Equal(http.StatusOK, set.Code, set.Body.String())
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
	assertionsT.Equal(http.StatusPreconditionFailed, conflict.Code, conflict.Body.String())

	written, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.Equal(before, string(written))
	credentials, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	value, state, err := credentials.Resolve(
		credentialID, "https://api.exa.example/search", "", nil,
	)
	requirementsT.NoError(err)
	assertionsT.Equal("origin-bound-secret", value)
	assertionsT.True(state.Configured)
	assertionsT.False(srv.settingsPendingRestart.Load())
}

func TestPatchSettingsRemovesStaleStoredCredentialOnLaterWrite(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, settingsStoredVectorConfig)
	storeVectorEmbeddingsCredential(t, srv, "stale-origin-secret")

	// The endpoint moves without the API severing the credential, as after an
	// interrupted cleanup or a host edit of config.toml.
	moved := strings.Replace(settingsStoredVectorConfig,
		"https://first.example.test/v1", "https://second.example.test/v1", 1)
	requirementsT.NoError(os.WriteFile(path, []byte(moved), 0o600))

	before := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, before.Code, before.Body.String())
	assertionsT.Equal(map[string]any{"configured": false, "source": "none"},
		rawEmbeddingSecretState(t, before.Body.Bytes()))
	stale, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	requirementsT.True(stale.Stored(providercredentials.VectorEmbeddingsID))

	unrelated := patchSettings(t, srv,
		`{"updates":[{"key":"log.level","value":{"string":"debug"}}]}`)
	requirementsT.Equal(http.StatusOK, unrelated.Code, unrelated.Body.String())
	assertionsT.NotContains(unrelated.Body.String(), "stale-origin-secret")
	severed, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.False(severed.Stored(providercredentials.VectorEmbeddingsID))
	assertionsT.Equal(severed.ETag, unrelated.Header().Get("Credential-Etag"))
}

func TestPatchSettingsCredentialConflictCanFollowCommittedSlackPolicy(t *testing.T) {
	t.Parallel()
	requirements, assertions := require.New(t), assert.New(t)
	initial := settingsStoredVectorConfig + "\n[slack]\ndms = true\n"
	srv, path := newSettingsTestServer(t, initial)
	credentialETag := storeVectorEmbeddingsCredential(t, srv, "stale-origin-secret")
	moved := strings.Replace(initial, "https://first.example.test/v1", "https://second.example.test/v1", 1)
	requirements.NoError(os.WriteFile(path, []byte(moved), 0o600))
	srv.settingsConfigEditor = func(configPath, ifMatch string, edits []config.Edit) (config.ConfigFile, error) {
		snapshot, err := config.EditConfigFile(configPath, ifMatch, edits)
		if err != nil {
			return snapshot, err
		}
		// Interleave a real credential write between config commit and cleanup.
		_, err = providercredentials.Put(srv.cfg.TokensDir(), credentialETag,
			providercredentials.VectorEmbeddingsID, "https://first.example.test/v1", "rotated-secret")
		requirements.NoError(err)
		return snapshot, nil
	}

	response := patchSettings(t, srv, `{"updates":[{"key":"slack.dms","value":{"boolean":false}}]}`)
	requirements.Equal(http.StatusPreconditionFailed, response.Code, response.Body.String())
	var failure map[string]any
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
	assertions.Equal("credential_conflict", failure["error"])
	loaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.False(loaded.Slack.DMsEnabled())
	credentials, err := providercredentials.Read(srv.cfg.TokensDir())
	requirements.NoError(err)
	value, _, err := credentials.Resolve(providercredentials.VectorEmbeddingsID, "https://first.example.test/v1", "", nil)
	requirements.NoError(err)
	assertions.Equal("rotated-secret", value)
}

func TestPatchSettingsRemovesStaleNamedProviderCredentialsAfterHostEdit(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	removedID := providercredentials.PersonEnrichmentID("removed-provider")
	movedID := providercredentials.PersonEnrichmentID("moved-provider")
	stored := performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(removedID),
		[]byte(`{"value":"removed-provider-secret"}`), get.Header().Get("Credential-Etag"), "")
	requirementsT.Equal(http.StatusOK, stored.Code, stored.Body.String())
	stored = performSettingsRequest(t, srv, http.MethodPut,
		"/api/v1/settings/provider-credentials/"+url.PathEscape(movedID),
		[]byte(`{"value":"moved-provider-secret"}`), stored.Header().Get("ETag"), "")
	requirementsT.Equal(http.StatusOK, stored.Code, stored.Body.String())

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
	requirementsT.NoError(os.WriteFile(path, []byte(hostEdited), 0o600))
	stale, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	requirementsT.True(stale.Stored(removedID))
	requirementsT.True(stale.Stored(movedID))

	unrelated := patchSettings(t, srv,
		`{"updates":[{"key":"log.level","value":{"string":"debug"}}]}`)
	requirementsT.Equal(http.StatusOK, unrelated.Code, unrelated.Body.String())
	cleaned, err := providercredentials.Read(srv.cfg.TokensDir())
	requirementsT.NoError(err)
	assertionsT.False(cleaned.Stored(removedID))
	assertionsT.False(cleaned.Stored(movedID))
	assertionsT.Equal(cleaned.ETag, unrelated.Header().Get("Credential-Etag"))
}

func TestPutPersonEnrichmentProviderRequiresValidKindEvenWhenDisabled(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
	srv, path := newSettingsTestServer(t, "[people.enrichment]\nenabled = false\n")
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())

	for _, kind := range []string{"", "other"} {
		body := []byte(`{"kind":"` + kind + `","enabled":false,"endpoint":"https://api.exa.example/search",` +
			`"allowed_identifiers":["name"],"target_keys":["attribute:bio"],` +
			`"allow_sensitive_targets":false,"retention_posture":"zero_retention",` +
			`"training_posture":"no_training","refresh_interval":"24h","request_timeout":"1m",` +
			`"max_retries":5,"max_requests_per_run":20,"max_requests_per_day":100}`)
		rejected := performSettingsRequest(t, srv, http.MethodPut,
			"/api/v1/settings/person-enrichment/providers/new-provider", body, get.Header().Get("ETag"), "")
		assertionsT.Equal(http.StatusUnprocessableEntity, rejected.Code, rejected.Body.String())
		assertionsT.Contains(rejected.Body.String(), "validation_failed")
	}
	written, err := os.ReadFile(path)
	requirementsT.NoError(err)
	assertionsT.NotContains(string(written), "new-provider")
}

func TestSettingsReadsAndRepairsInvalidDisabledPersonEnrichmentProvider(t *testing.T) {
	t.Parallel()
	requirementsT := require.New(t)
	assertionsT := assert.New(t)
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
	requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
	assertionsT.NotContains(get.Body.String(), "token=example-credential")
	var before SettingsResponse
	requirementsT.NoError(json.Unmarshal(get.Body.Bytes(), &before))
	requirementsT.Len(before.PersonEnrichmentProviders, 1)
	assertionsT.Empty(before.PersonEnrichmentProviders[0].Endpoint)
	assertionsT.Nil(before.PersonEnrichmentProviders[0].Credential)

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
	requirementsT.Equal(http.StatusOK, repaired.Code, repaired.Body.String())
	var after SettingsResponse
	requirementsT.NoError(json.Unmarshal(repaired.Body.Bytes(), &after))
	requirementsT.Len(after.PersonEnrichmentProviders, 1)
	assertionsT.Equal("https://api.exa.example/search", after.PersonEnrichmentProviders[0].Endpoint)
	assertionsT.NotNil(after.PersonEnrichmentProviders[0].Credential)
}

func TestSettingsSectionsAreConsistentWithTheirGroups(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	groupsByID := make(map[string]SettingGroup, len(settingsGroups))
	for _, group := range settingsGroups {
		groupsByID[group.ID] = group
	}
	populated := make(map[string]map[string]bool)
	for _, definition := range settingsCatalog {
		group, ok := groupsByID[definition.group]
		if !assertionsT.True(ok, "%s uses undeclared group %q", definition.key, definition.group) {
			continue
		}
		section := metadataForSetting(definition.key).section
		if len(group.Sections) == 0 {
			assertionsT.Empty(section, "%s names a section but group %q has none", definition.key, group.ID)
			continue
		}
		known := false
		for _, candidate := range group.Sections {
			known = known || candidate.ID == section
		}
		assertionsT.True(known, "%s names section %q that group %q does not declare", definition.key, section, group.ID)
		if populated[group.ID] == nil {
			populated[group.ID] = make(map[string]bool)
		}
		populated[group.ID][section] = true
	}
	for _, group := range settingsGroups {
		for _, section := range group.Sections {
			assertionsT.True(populated[group.ID][section.ID], "section %s/%s has no settings", group.ID, section.ID)
		}
	}
}

func TestSettingsOffValuesPassBoundsChecks(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	requirementsT.NoError(validateSettingBounds("backup.zstd_level", 0), "the off value sits outside the on range")
	requirementsT.NoError(validateSettingBounds("backup.zstd_level", 19))
	requirementsT.Error(validateSettingBounds("backup.zstd_level", 20))
	requirementsT.Error(validateSettingBounds("backup.zstd_level", -1))
	requirementsT.NoError(validateSettingBounds("discord.max_media_mb", 0))
	requirementsT.NoError(validateSettingBounds("beeper.rate_limit_qps", 0.0))
	requirementsT.Error(validateSettingBounds("beeper.rate_limit_qps", 0.05), "on values start at 0.1; PATCH raises smaller rates")
	requirementsT.NoError(validateSettingBounds("beeper.rate_limit_qps", 0.5))
	requirementsT.Error(validateSettingBounds("beeper.rate_limit_qps", -1.0))
	requirementsT.Error(validateSettingBounds("sync.rate_limit_qps", 0), "settings without an off value keep their minimum")
	for key, validation := range settingsValidation {
		if validation.Off == nil || validation.Off.OnMinimum == nil {
			continue
		}
		requirementsT.NotNil(validation.Minimum, "%s: minimum must stay published for clients that ignore off", key)
		off, err := strconv.ParseFloat(validation.Off.text(), 64)
		requirementsT.NoError(err, key)
		assertionsT.LessOrEqual(*validation.Minimum, off, "%s: minimum must include the off value", key)
		assertionsT.Greater(*validation.Off.OnMinimum, off, "%s: the on range must exclude the off value", key)
	}
	for key, validation := range settingsValidation {
		if validation.Off == nil {
			continue
		}
		assertionsT.NotEmpty(validation.Off.Label, "%s off state needs a label", key)
		assertionsT.NotContains(strings.ToLower(validation.Hint), " 0 ", "%s hint must not restate its off value", key)
	}
}

func TestSettingsPatchEnforcesRequiredAndCronFormat(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	srv, _ := newSettingsTestServer(t, "[people.enrichment]\nschedule = \"*/15 * * * *\"\n")

	empty := patchSettings(t, srv, `{"updates":[{"key":"people.enrichment.schedule","value":{"string":""}}]}`)
	assertionsT.Equal(http.StatusUnprocessableEntity, empty.Code, empty.Body.String())

	invalid := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"0 25 * * *"}}]}`)
	assertionsT.Equal(http.StatusUnprocessableEntity, invalid.Code, invalid.Body.String())

	off := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":""}}]}`)
	assertionsT.Equal(http.StatusOK, off.Code, off.Body.String())

	valid := patchSettings(t, srv, `{"updates":[{"key":"people.enrichment.schedule","value":{"string":"0 4 * * mon-fri"}}]}`)
	assertionsT.Equal(http.StatusOK, valid.Code, valid.Body.String())

	zoned := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"CRON_TZ=Europe/Berlin 0 4 * * *"}}]}`)
	assertionsT.Equal(http.StatusOK, zoned.Code, zoned.Body.String())

	emptyList := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"0 3 * * ,"}}]}`)
	assertionsT.Equal(http.StatusUnprocessableEntity, emptyList.Code, emptyList.Body.String())
}

func TestSettingsPatchTrimsCronSchedules(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "[beeper]\nschedule = \"0 2 * * *\"\n")

	padded := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"  0 3 * * *  "}}]}`)
	requirementsT.Equal(http.StatusOK, padded.Code, padded.Body.String())
	assertionsT.Equal("0 3 * * *", currentSettingString(t, srv, "beeper.schedule"))

	blank := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"   "}}]}`)
	requirementsT.Equal(http.StatusOK, blank.Code, blank.Body.String())
	assertionsT.Empty(currentSettingString(t, srv, "beeper.schedule"))

	zoneOnly := patchSettings(t, srv, `{"updates":[{"key":"beeper.schedule","value":{"string":"CRON_TZ=UTC"}}]}`)
	requirementsT.Equal(http.StatusOK, zoneOnly.Code, zoneOnly.Body.String())
	assertionsT.Empty(currentSettingString(t, srv, "beeper.schedule"), "a zone with no fields is no schedule")

	cfg, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.Empty(cfg.Beeper.Schedule)
}

func TestSettingsPatchRaisesRatesBelowTheOnMinimum(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	requirementsT := require.New(t)
	srv, path := newSettingsTestServer(t, "[beeper]\nrate_limit_qps = 5\n")

	small := patchSettings(t, srv, `{"updates":[{"key":"beeper.rate_limit_qps","value":{"number":0.05}}]}`)
	requirementsT.Equal(http.StatusOK, small.Code, small.Body.String())
	cfg, err := config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.InDelta(0.1, cfg.Beeper.RateLimitQPS, 1e-9, "a positive rate below the on minimum is raised, not rejected")

	off := patchSettings(t, srv, `{"updates":[{"key":"beeper.rate_limit_qps","value":{"number":0}}]}`)
	requirementsT.Equal(http.StatusOK, off.Code, off.Body.String())
	cfg, err = config.Load(path, "")
	requirementsT.NoError(err)
	assertionsT.Zero(cfg.Beeper.RateLimitQPS, "zero stays the off value")

	negative := patchSettings(t, srv, `{"updates":[{"key":"beeper.rate_limit_qps","value":{"number":-1}}]}`)
	assertionsT.Equal(http.StatusUnprocessableEntity, negative.Code, negative.Body.String())
}

func currentSettingString(t *testing.T, srv *Server, key string) string {
	t.Helper()
	get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
	require.Equal(t, http.StatusOK, get.Code, get.Body.String())
	var body SettingsResponse
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &body))
	setting, ok := settingsByKey(body.Settings)[key]
	require.True(t, ok, key)
	require.NotNil(t, setting.Value, key)
	require.NotNil(t, setting.Value.String, key)
	return *setting.Value.String
}

func TestSettingsBoundHintsLiveOnTheControl(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	for key, validation := range settingsValidation {
		hint := strings.ToLower(validation.Hint)
		for _, phrase := range []string{"at least", "or more", " to ", "between"} {
			assertionsT.NotContains(hint, phrase, "%s spells out a bound in text; use minimum, maximum, or off", key)
		}
		if validation.Format != "" {
			assertionsT.Empty(validation.Hint, "%s has a format; the control explains the syntax", key)
		}
	}
}

func TestSettingsHintsDoNotRepeatDescriptions(t *testing.T) {
	t.Parallel()
	assertionsT := assert.New(t)
	for key, validation := range settingsValidation {
		hint := strings.TrimSpace(validation.Hint)
		if hint == "" {
			continue
		}
		description := metadataForSetting(key).description
		assertionsT.NotContains(strings.ToLower(description), strings.ToLower(strings.TrimSuffix(hint, ".")),
			"%s repeats its hint inside its description", key)
	}
}

func TestSecretHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: ""},
		{name: "eleven characters is too short", value: "task-secret", want: ""},
		{name: "twelve characters", value: "test-api-key", want: "tes…key"},
		{name: "long key", value: "sk-live-0123456789abcdefx9Q", want: "sk-…x9Q"},
		{name: "multibyte characters count as one", value: "ééé-secret-ключ", want: "ééé…люч"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, secretHint(tc.value))
		})
	}
}

func TestSettingsPatchPreservesConversationPolicy(t *testing.T) {
	requirementsT := require.New(t)
	assertionsT := assert.New(t)

	for _, body := range []string{"", "[beeper]\nmax_participants=0\n[slack]\nmax_participants=17\n[discord]\nmax_participants=25\n[teams]\nmax_participants=31\n[conversation_policy]\nactivity_window_days=13\n[[conversation_policy.overrides]]\nsource_type='slack'\nsource_identifier='T_EXAMPLE:U_EXAMPLE'\nconversation_id='room'\ntier='cold'\n"} {
		srv, path := newSettingsTestServer(t, body)
		before, err := config.Load(path, "")
		requirementsT.NoError(err)
		get := performSettingsRequest(t, srv, http.MethodGet, settingsPath, nil, "", "")
		requirementsT.Equal(http.StatusOK, get.Code, get.Body.String())
		resp := patchSettings(t, srv, `{"updates":[{"key":"beeper.media_max_participants","value":{"integer":12}},{"key":"web.theme","value":{"string":"dark"}}]}`)
		requirementsT.Equal(http.StatusOK, resp.Code, resp.Body.String())
		after, err := config.Load(path, "")
		requirementsT.NoError(err)
		assertionsT.Equal(before.ConversationPolicySettings(), after.ConversationPolicySettings())
		if body == "" {
			assertionsT.Equal(20, after.Beeper.MaxParticipants)
			assertionsT.Equal(180, after.ConversationPolicy.ActivityWindowDays)
		}
	}
}
