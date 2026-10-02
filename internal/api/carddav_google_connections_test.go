package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

// Compose real saved bindings, token selection, startup and rollback. Discovery
// uses the existing local DAV server when a Google binding is replaced, so this
// regression requires no real Google authorization or live provider requests.
func TestCardDAVGoogleConnectionsRestartRollbackAndSchedules(t *testing.T) {
	assertions, required := assert.New(t), require.New(t)
	root, baseURL := multipleCardDAVController(t)
	factory := root.factory
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data = root.cfg.HomeDir, root.cfg.Data
	cfg.CardDAV = config.CardDAVConfig{BaseURL: "https://contacts.example/dav", Username: "default", Enabled: true}
	_, _, err := root.store.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		PrincipalURL: "https://contacts.example/principal/", HomeURL: "https://contacts.example/books/",
	})
	required.NoError(err)
	required.NoError(carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		BaseURL: cfg.CardDAV.BaseURL, Username: "default", Password: "default-synthetic-secret", ConnectionGeneration: 1,
	}))
	cfg.OAuth.Apps = map[string]config.OAuthApp{}
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{}
	for _, name := range []string{"personal", "work"} {
		secrets := filepath.Join(cfg.HomeDir, name+"-client.json")
		required.NoError(os.WriteFile(secrets, []byte(`{"web":{"client_id":"`+name+`-client","client_secret":"synthetic-secret","redirect_uris":["https://archive.example/"]}}`), 0600))
		cfg.OAuth.Apps[name] = config.OAuthApp{ClientSecrets: secrets}
		cfg.CardDAVConnections[name] = config.CardDAVConfig{Provider: "google", BaseURL: carddav.GoogleDiscoveryURL, Username: name + "@example.com", OAuthApp: name, Enabled: true, Schedule: "0 3 * * *"}
		_, _, err = root.store.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: name, BaseURL: carddav.GoogleDiscoveryURL, Username: name + "@example.com", PrincipalURL: "https://www.googleapis.com/principal/", HomeURL: "https://www.googleapis.com/books/"})
		required.NoError(err)
		dir, err := carddav.ConnectionTokenDir(cfg.TokensDir(), name)
		required.NoError(err)
		required.NoError(carddav.SaveCredential(dir, carddav.Credential{Google: true, BaseURL: carddav.GoogleDiscoveryURL, Username: name + "@example.com", OAuthApp: name, ConnectionGeneration: 1}))
		manager, err := carddav.NewGoogleOAuthManager(secrets, cfg.TokensDir(), name, name+"@example.com", testLogger())
		required.NoError(err)
		token, err := json.Marshal(map[string]any{"access_token": name + "-synthetic-access", "client_id": name + "-client", "scopes": []string{oauth.ScopeCardDAV}})
		required.NoError(err)
		required.NoError(os.MkdirAll(filepath.Dir(manager.TokenPath(name+"@example.com")), 0700))
		required.NoError(os.WriteFile(manager.TokenPath(name+"@example.com"), token, 0600))
	}
	required.NoError(cfg.Save())
	loaded, err := config.LoadConfigFile(mustReadMultipleConfig(t, cfg.ConfigFilePath()), cfg.HomeDir)
	required.NoError(err)
	root, err = NewCardDAVController(loaded, root.store, testLogger())
	required.NoError(err)
	for _, name := range []string{"default", "personal", "work"} {
		status, err := root.Status(t.Context(), name)
		required.NoError(err)
		assertions.True(status.Available, name)
		assertions.True(status.CredentialConfigured, name)
		if name != "default" {
			selected, err := root.Select(name, false)
			required.NoError(err)
			dir, err := carddav.ConnectionTokenDir(loaded.TokensDir(), name)
			required.NoError(err)
			binding, err := carddav.LoadCredential(dir)
			required.NoError(err)
			token, err := selected.googleBearerToken(t.Context(), binding)
			required.NoError(err)
			assertions.Equal(binding.OAuthApp+"-synthetic-access", token)
		}
	}
	var scheduled []string
	root.SetConnectionScheduleReconciler(func(name string, _ config.CardDAVConfig, service CardDAVOperations) error {
		assertions.NotNil(service)
		scheduled = append(scheduled, name)
		return nil
	})
	required.NoError(root.reconcileGoogleSchedules(cardDAVGoogleAuthorization{connection: "work", email: "work@example.com", oauthApp: "work"}))
	assertions.ElementsMatch([]string{"work"}, scheduled)
	work, err := root.Select("work", false)
	required.NoError(err)
	beforeConfig, err := os.ReadFile(loaded.ConfigFilePath())
	required.NoError(err)
	beforeBindings := map[string][]byte{}
	for _, name := range []string{"default", "personal", "work"} {
		dir, err := carddav.ConnectionTokenDir(loaded.TokensDir(), name)
		required.NoError(err)
		data, err := os.ReadFile(filepath.Join(dir, "carddav.json"))
		required.NoError(err)
		beforeBindings[name] = data
	}
	work.factory = factory
	work.persistDiscovery = func(context.Context, cardDAVCandidate, string, string, carddav.Discovery, bool) error {
		return errors.New("synthetic storage failure")
	}
	_, err = work.Save(t.Context(), CardDAVAccountRequest{BaseURL: baseURL, Username: "work", Password: "work-synthetic-secret", Enabled: new(true)})
	required.ErrorIs(err, errCardDAVStorage)
	afterConfig, err := os.ReadFile(loaded.ConfigFilePath())
	required.NoError(err)
	assertions.Equal(beforeConfig, afterConfig)
	for name, want := range beforeBindings {
		dir, err := carddav.ConnectionTokenDir(loaded.TokensDir(), name)
		required.NoError(err)
		got, err := os.ReadFile(filepath.Join(dir, "carddav.json"))
		required.NoError(err)
		assertions.Equal(want, got, name)
	}
	restarted, err := NewCardDAVController(loaded, root.store, testLogger())
	required.NoError(err)
	for _, name := range []string{"default", "personal", "work"} {
		status, err := restarted.Status(t.Context(), name)
		required.NoError(err)
		assertions.True(status.CredentialConfigured, name)
	}
}
