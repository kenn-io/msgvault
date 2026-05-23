package oauth

import (
	"log/slog"
	"testing"

	"golang.org/x/oauth2/google"
)

func TestHasEmbeddedCredentials(t *testing.T) {
	// Save and restore package vars around the test
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()

	tests := []struct {
		name   string
		id     string
		secret string
		want   bool
	}{
		{"both set", "id", "secret", true},
		{"id only", "id", "", false},
		{"secret only", "", "secret", false},
		{"neither", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oauthClientID = tc.id
			oauthClientSecret = tc.secret
			if got := HasEmbeddedCredentials(); got != tc.want {
				t.Errorf("HasEmbeddedCredentials() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmbeddedConfig(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	scopes := []string{"scope-a", "scope-b"}
	cfg := EmbeddedConfig(scopes)

	if cfg.ClientID != "test-client-id" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "test-client-id")
	}
	if cfg.ClientSecret != "test-client-secret" {
		t.Errorf("ClientSecret = %q, want %q", cfg.ClientSecret, "test-client-secret")
	}
	if len(cfg.Scopes) != 2 || cfg.Scopes[0] != "scope-a" || cfg.Scopes[1] != "scope-b" {
		t.Errorf("Scopes = %v, want %v", cfg.Scopes, scopes)
	}
	if cfg.Endpoint != google.Endpoint {
		t.Errorf("Endpoint = %v, want google.Endpoint", cfg.Endpoint)
	}
}

func TestNewEmbeddedManager(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	tokensDir := t.TempDir()
	mgr, err := NewEmbeddedManager(tokensDir, slog.Default(), ScopesEmbedded)
	if err != nil {
		t.Fatalf("NewEmbeddedManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewEmbeddedManager returned nil manager")
	}
	if mgr.tokensDir != tokensDir {
		t.Errorf("tokensDir = %q, want %q", mgr.tokensDir, tokensDir)
	}
	if !mgr.isEmbedded {
		t.Error("isEmbedded = false, want true")
	}
}

func TestNewEmbeddedManagerWithoutCredentials(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = ""
	oauthClientSecret = ""

	_, err := NewEmbeddedManager(t.TempDir(), slog.Default(), ScopesEmbedded)
	if err == nil {
		t.Fatal("NewEmbeddedManager: want error when credentials are empty, got nil")
	}
}
