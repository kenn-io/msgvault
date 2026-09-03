package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/tui"
)

func TestTUISettingsBackendLoadsSelfDescribingCatalog(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/settings", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"config-1"`)
		_, _ = io.WriteString(w, `{
  "groups":[{"id":"archive","label":"Archive"}],
  "settings":[{
    "key":"analytics.engine","group":"archive","label":"Analytics engine",
    "description":"Select the aggregate query engine.","kind":"string",
    "value":{"string":"auto"},"options":["auto","sql","duckdb"],
    "restart_required":true,"read_only":false,
    "validation":{"hint":"Choose a supported engine.","required":true}
  }],
  "credential_etag":"\"credentials-1\"",
  "pending_restart":true
}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	snapshot, err := backend.LoadSettings(context.Background())
	requirements.NoError(err)
	assertions.Equal(`"config-1"`, snapshot.ETag)
	assertions.Equal(`"credentials-1"`, snapshot.CredentialETag)
	assertions.True(snapshot.PendingRestart)
	requirements.Len(snapshot.Groups, 1)
	assertions.Equal("Archive", snapshot.Groups[0].Label)
	requirements.Len(snapshot.Fields, 1)
	field := snapshot.Fields[0]
	assertions.Equal("analytics.engine", field.Key)
	assertions.Equal("Analytics engine", field.Label)
	assertions.Equal(tui.SettingKindString, field.Kind)
	assertions.True(field.RestartRequired)
	assertions.Equal("Choose a supported engine.", field.Validation.Hint)
	requirements.NotNil(field.Value)
	requirements.NotNil(field.Value.String)
	assertions.Equal("auto", *field.Value.String)
}

func TestTUISettingsBackendSeparatesConfigAndCredentialWrites(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const (
		providerSecret = "provider-secret-only-in-credential-request"
		configSecret   = "legacy-config-secret-only-in-settings-patch"
	)
	var patchBody, credentialBody string
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/settings":
			patchBody = string(body)
			assert.Equal(t, `"config-old"`, r.Header.Get("If-Match"))
			w.Header().Set("ETag", `"config-new"`)
			w.Header().Set("Credential-Etag", `"credentials-after-patch"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"settings":[],"groups":[],"pending_restart":true}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/settings/provider-credentials/vector.embeddings":
			credentialBody = string(body)
			assert.Equal(t, `"credentials-after-patch"`, r.Header.Get("If-Match"))
			w.Header().Set("ETag", `"credentials-new"`)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings":
			w.Header().Set("ETag", `"config-new"`)
			w.Header().Set("Credential-Etag", `"credentials-new"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
  "groups":[{"id":"search","label":"Search"}],
  "settings":[{"key":"vector.embeddings.api_key","group":"search","label":"Embedding API key","description":"Write-only credential.","kind":"secret","secret":{"configured":true,"source":"stored"},"restart_required":true}],
  "pending_restart":true
}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
	value := false

	snapshot, err := backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag:     `"config-old"`,
		CredentialETag: `"credentials-old"`,
		Updates: []tui.SettingUpdate{{
			Key: "analytics.auto_build_cache", Value: &tui.SettingValue{Boolean: &value},
		}},
		ConfigSecrets: []tui.ConfigSecretUpdate{{
			Key: "integrations.tasks.api_key", Action: "set", Value: configSecret,
		}},
		Credentials: []tui.CredentialUpdate{{
			Key: "vector.embeddings.api_key", CredentialID: "vector.embeddings",
			Action: "set", Value: providerSecret,
		}},
	})
	requirements.NoError(err)
	assertions.Equal([]string{
		"PATCH /api/v1/settings",
		"PUT /api/v1/settings/provider-credentials/vector.embeddings",
		"GET /api/v1/settings",
	}, methods)
	assertions.NotContains(patchBody, providerSecret)
	assertions.Contains(patchBody, configSecret)
	assertions.Contains(patchBody, `"secret":{"action":"set"`)
	assertions.NotContains(patchBody, "provider-credentials")
	assertions.NotContains(credentialBody, configSecret)
	assertions.Contains(credentialBody, providerSecret)
	assertions.Equal(`"config-new"`, snapshot.ETag)
	assertions.Equal(`"credentials-new"`, snapshot.CredentialETag)
	requirements.Len(snapshot.Fields, 1)
	requirements.NotNil(snapshot.Fields[0].Secret)
	assertions.Equal("stored", snapshot.Fields[0].Secret.Source)
}

func TestTUISettingsBackendClassifiesStaleCredentialETag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, `"credentials-stale"`, r.Header.Get("If-Match"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"error":"settings_conflict","message":"The credential store changed"}`)
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	_, err := backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag:     `"config-current"`,
		CredentialETag: `"credentials-stale"`,
		Credentials: []tui.CredentialUpdate{{
			Key: "vector.multimodal.api_key", CredentialID: "vector.multimodal", Action: "clear",
		}},
	})
	var conflict *tui.SettingsConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, tui.SettingsConflictCredentials, conflict.Scope)
}

func TestTUISettingsBackendUsesDaemonCredentialIDsForNamedProviders(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var credentialPaths []string
	credentialETag := `"credentials-start"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"config-current"`)
			w.Header().Set("Credential-Etag", credentialETag)
			_, _ = io.WriteString(w, `{
  "groups":[{"id":"enrichment","label":"Person enrichment"}],
  "settings":[],
  "person_enrichment_providers":[
	    {"name":"exa-primary","kind":"exa","enabled":true,"credential_id":"people.enrichment/exa-primary","credential":{"configured":false,"source":"none"}},
	    {"name":"sixtyfour-primary","kind":"sixtyfour","enabled":false,"credential_id":"people.enrichment/sixtyfour-primary","credential":{"configured":false,"source":"none"}}
  ],
  "pending_restart":false
}`)
		case http.MethodPut:
			credentialPaths = append(credentialPaths, r.URL.EscapedPath())
			assert.Equal(t, credentialETag, r.Header.Get("If-Match"))
			credentialETag = `"credentials-next-` + string(rune('0'+len(credentialPaths))) + `"`
			w.Header().Set("ETag", credentialETag)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"configured":true,"source":"stored"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
	loaded, err := backend.LoadSettings(context.Background())
	requirements.NoError(err)
	requirements.Len(loaded.Fields, 2)
	assertions.Equal("exa-primary API key", loaded.Fields[0].Label)
	assertions.Contains(loaded.Fields[0].Description, "Kind: exa")
	assertions.Contains(loaded.Fields[0].Description, "Status: enabled")
	assertions.Equal("people.enrichment/exa-primary", loaded.Fields[0].CredentialID)
	assertions.Contains(loaded.Fields[1].Description, "Kind: sixtyfour")
	assertions.Contains(loaded.Fields[1].Description, "Status: disabled")
	assertions.Equal("people.enrichment/sixtyfour-primary", loaded.Fields[1].CredentialID)

	_, err = backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag: loaded.ETag, CredentialETag: loaded.CredentialETag,
		Credentials: []tui.CredentialUpdate{
			{
				Key: loaded.Fields[0].Key, CredentialID: loaded.Fields[0].CredentialID,
				Action: "set", Value: "exa-test-secret",
			},
			{
				Key: loaded.Fields[1].Key, CredentialID: loaded.Fields[1].CredentialID,
				Action: "set", Value: "sixtyfour-test-secret",
			},
		},
	})
	requirements.NoError(err)
	assertions.Equal([]string{
		"/api/v1/settings/provider-credentials/people.enrichment%2Fexa-primary",
		"/api/v1/settings/provider-credentials/people.enrichment%2Fsixtyfour-primary",
	}, credentialPaths)
}

func TestTUISettingsBackendWritesEncodedNamedProviderIDThroughRegisteredRouter(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const secret = "registered-router-secret-must-not-render"
	const providerName = "exa:primary..v1"
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(configPath, []byte(fmt.Sprintf(`
[[people.enrichment.providers]]
name = %q
kind = "exa"
enabled = false
`, providerName)), 0o600))
	cfg, err := config.Load(configPath, "")
	requirements.NoError(err)
	apiServer := api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	server := httptest.NewServer(apiServer.Router())
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	loaded, err := backend.LoadSettings(context.Background())
	requirements.NoError(err)
	requirements.NotEmpty(loaded.CredentialETag)
	var credentialField tui.SettingField
	for _, field := range loaded.Fields {
		if field.CredentialID == "people.enrichment/"+providerName {
			credentialField = field
			break
		}
	}
	requirements.Equal("people.enrichment/"+providerName, credentialField.CredentialID)
	assertions.Contains(credentialField.Description, "Kind: exa")
	stored, err := backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag: loaded.ETag, CredentialETag: loaded.CredentialETag,
		Credentials: []tui.CredentialUpdate{{
			Key: credentialField.Key, CredentialID: credentialField.CredentialID,
			Action: "set", Value: secret,
		}},
	})
	requirements.NoError(err)
	assertions.NotEqual(loaded.CredentialETag, stored.CredentialETag)

	cleared, err := backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag: stored.ETag, CredentialETag: stored.CredentialETag,
		Credentials: []tui.CredentialUpdate{{
			Key: credentialField.Key, CredentialID: credentialField.CredentialID,
			Action: "clear",
		}},
	})
	requirements.NoError(err)
	assertions.NotEqual(stored.CredentialETag, cleared.CredentialETag)
}

func TestTUISettingsBackendMapsValidationMetadataFromRegisteredRouter(t *testing.T) {
	requirements := require.New(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(configPath, nil, 0o600))
	cfg, err := config.Load(configPath, "")
	requirements.NoError(err)
	apiServer := api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler))
	server := httptest.NewServer(apiServer.Router())
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))

	loaded, err := backend.LoadSettings(context.Background())
	requirements.NoError(err)
	var activityBatch tui.SettingField
	for _, field := range loaded.Fields {
		if field.Key == "activity.batch_size" {
			activityBatch = field
			break
		}
	}
	requirements.Equal("activity.batch_size", activityBatch.Key)
	requirements.NotNil(activityBatch.Validation.Minimum)
	requirements.NotNil(activityBatch.Validation.Maximum)
	assert.InDelta(t, float64(1), *activityBatch.Validation.Minimum, 0)
	assert.InDelta(t, float64(10_000), *activityBatch.Validation.Maximum, 0)
}

func TestTUISettingsBackendReportsPartialSaveWithoutSecretLeak(t *testing.T) {
	const secret = "partial-provider-secret-must-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.Header().Set("ETag", `"config-new"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"settings":[],"groups":[],"pending_restart":true}`)
		case http.MethodPut:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"credential_write_failed","message":"Could not store credential"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	backend := newTUISettingsBackend(newTUISettingsDaemonClient(t, server))
	value := true

	_, err := backend.SaveSettings(context.Background(), tui.SettingsSaveRequest{
		ConfigETag:     `"config-old"`,
		CredentialETag: `"credentials-old"`,
		Updates: []tui.SettingUpdate{{
			Key: "analytics.auto_build_cache", Value: &tui.SettingValue{Boolean: &value},
		}},
		Credentials: []tui.CredentialUpdate{{
			Key: "vector.embeddings.api_key", CredentialID: "vector.embeddings",
			Action: "set", Value: secret,
		}},
	})
	var partial *tui.SettingsPartialSaveError
	require.ErrorAs(t, err, &partial)
	assert.Equal(t, []string{"analytics.auto_build_cache"}, partial.SavedKeys)
	assert.NotContains(t, err.Error(), secret)
}

func newTUISettingsDaemonClient(t *testing.T, server *httptest.Server) *daemonclient.Client {
	t.Helper()
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, AllowInsecure: true, HTTPClient: server.Client(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}
