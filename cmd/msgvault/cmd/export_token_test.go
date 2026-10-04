package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestSanitizeExportTokenPath(t *testing.T) {
	tokensDir := "/data/tokens"

	tests := []struct {
		name  string
		email string
		want  string
	}{
		{
			"normal email",
			"user@gmail.com",
			filepath.Join(tokensDir, "user@gmail.com.json"),
		},
		{
			"email with dots",
			"first.last@example.co.uk",
			filepath.Join(tokensDir, "first.last@example.co.uk.json"),
		},
		{
			"email with plus",
			"user+tag@gmail.com",
			filepath.Join(tokensDir, "user+tag@gmail.com.json"),
		},
		{
			"strips slashes",
			"user/evil@gmail.com",
			filepath.Join(tokensDir, "userevil@gmail.com.json"),
		},
		{
			"strips backslashes",
			"user\\evil@gmail.com",
			filepath.Join(tokensDir, "userevil@gmail.com.json"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeExportTokenPath(tokensDir, tt.email)
			assert.Equal(t, tt.want, got, "sanitizeExportTokenPath(%q)", tt.email)
		})
	}
}

func TestEmailValidation(t *testing.T) {
	tests := []struct {
		name    string
		email   string
		wantErr bool
	}{
		{"normal email", "user@gmail.com", false},
		{"dotted local", "first.last@example.com", false},
		{"dotted domain", "user@mail.example.co.uk", false},
		{"plus tag", "user+tag@gmail.com", false},
		{"missing @", "usergmail.com", true},
		{"missing dot", "user@localhost", true},
		{"path traversal slash", "user/@gmail.com", true},
		{"path traversal backslash", "user\\@gmail.com", true},
		{"double dot traversal", "user@../evil.com", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExportEmail(tt.email)
			if tt.wantErr {
				assert.Error(t, err, "validateExportEmail(%q)", tt.email)
			} else {
				assert.NoError(t, err, "validateExportEmail(%q)", tt.email)
			}
		})
	}
}

func TestResolveParam(t *testing.T) {
	tests := []struct {
		name      string
		flag      string
		envKey    string
		envVal    string
		configVal string
		want      string
	}{
		{"flag wins over all", "from-flag", "TEST_RESOLVE_1", "from-env", "from-config", "from-flag"},
		{"env wins over config", "", "TEST_RESOLVE_2", "from-env", "from-config", "from-env"},
		{"config as fallback", "", "TEST_RESOLVE_3", "", "from-config", "from-config"},
		{"all empty", "", "TEST_RESOLVE_4", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv(tt.envKey, tt.envVal)
			}
			got := resolveParam(tt.flag, tt.envKey, tt.configVal)
			assert.Equal(t, tt.want, got, "resolveParam(%q, %q, %q)",
				tt.flag, tt.envKey, tt.configVal)
		})
	}
}

func TestRunExportTokenUsesAndPreservesOrReplacesAPIKeyFile(t *testing.T) {
	keyFromFile := strings.Repeat("f", 64)
	for _, tc := range []struct {
		name           string
		flagAPIKey     string
		envAPIKey      string
		wantAPIKey     string
		wantAPIKeyFile bool
	}{
		{name: "file-backed fallback", wantAPIKeyFile: true},
		{name: "explicit key replaces file-backed key", flagAPIKey: "explicit-fixture-key", wantAPIKey: "explicit-fixture-key"},
		{name: "environment key replaces file-backed key", envAPIKey: "environment-fixture-key", wantAPIKey: "environment-fixture-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			oldTo, oldAPIKey, oldAllowInsecure := exportTokenTo, exportTokenAPIKey, exportAllowInsecure
			exportTokenTo, exportTokenAPIKey, exportAllowInsecure = "", tc.flagAPIKey, false
			t.Cleanup(func() {
				exportTokenTo, exportTokenAPIKey, exportAllowInsecure = oldTo, oldAPIKey, oldAllowInsecure
			})
			t.Setenv("MSGVAULT_REMOTE_URL", "")
			t.Setenv("MSGVAULT_REMOTE_API_KEY", tc.envAPIKey)

			home := t.TempDir()
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = home
			cfg.Data.DataDir = filepath.Join(home, "data")
			keyFile := filepath.Join(home, "remote-api-key")
			requirements.NoError(os.WriteFile(keyFile, []byte(keyFromFile), 0o600))
			cfg.Remote = config.RemoteConfig{URL: "", APIKeyFile: keyFile, AllowInsecure: true}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantKey := keyFromFile
				if tc.flagAPIKey != "" {
					wantKey = tc.flagAPIKey
				} else if tc.envAPIKey != "" {
					wantKey = tc.envAPIKey
				}
				if r.Header.Get("X-Api-Key") != wantKey {
					http.Error(w, "unexpected API key", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/api/v1/auth/token/user@example.test":
					w.WriteHeader(http.StatusCreated)
				case "/api/v1/accounts":
					w.WriteHeader(http.StatusCreated)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg.Remote.URL = server.URL
			requirements.NoError(cfg.Save())

			tokensDir := cfg.TokensDir()
			requirements.NoError(os.MkdirAll(tokensDir, 0o700))
			requirements.NoError(os.WriteFile(filepath.Join(tokensDir, "user@example.test.json"), []byte("{}"), 0o600))

			cmd := &cobra.Command{}
			cmd.SetContext(withTestConfig(t, cfg))
			requirements.NoError(runExportToken(cmd, []string{"user@example.test"}))

			if tc.wantAPIKeyFile {
				assertions.Empty(cfg.Remote.APIKey)
				assertions.Equal(keyFile, cfg.Remote.APIKeyFile)
			} else {
				assertions.Equal(tc.wantAPIKey, cfg.Remote.APIKey)
				assertions.Empty(cfg.Remote.APIKeyFile)
			}

			loaded, err := config.Load(cfg.ConfigFilePath(), "")
			requirements.NoError(err)
			assertions.Equal(cfg.Remote.APIKey, loaded.Remote.APIKey)
			assertions.Equal(cfg.Remote.APIKeyFile, loaded.Remote.APIKeyFile)
		})
	}
}

// newTestExporter creates a tokenExporter backed by the given httptest server.
func newTestExporter(srv *httptest.Server, tokensDir string) *tokenExporter {
	return &tokenExporter{
		httpClient: srv.Client(),
		tokensDir:  tokensDir,
		stdout:     io.Discard,
		stderr:     io.Discard,
	}
}

// writeTestToken writes a fake token file for the test account.
func writeTestToken(t *testing.T, tokensDir, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	path := filepath.Join(tokensDir, "user@gmail.com.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600), "write token")
}

func TestExport_UploadSuccess(t *testing.T) {
	assert := assert.New(t)
	var gotPath string
	var gotBody []byte
	var gotAPIKey string

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/"):
			gotPath = r.URL.Path
			gotAPIKey = r.Header.Get("X-Api-Key")
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/api/v1/accounts":
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{"token":"secret"}`)

	e := newTestExporter(srv, tokensDir)
	result, err := e.export("user@gmail.com", srv.URL, "my-key", false)
	require.NoError(t, err, "export")

	// httptest decodes percent-encoding in r.URL.Path, so we see the
	// decoded form even though url.PathEscape encodes @ on the wire.
	assert.Equal("/api/v1/auth/token/user@gmail.com", gotPath, "path")

	// Verify API key header
	assert.Equal("my-key", gotAPIKey, "X-API-Key")

	// Verify token body
	assert.JSONEq(`{"token":"secret"}`, string(gotBody), "body")

	// Verify result
	assert.Equal(srv.URL, result.remoteURL, "result.remoteURL")
	assert.Equal("my-key", result.apiKey, "result.apiKey")
}

func TestExport_UploadFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{"token":"secret"}`)

	e := newTestExporter(srv, tokensDir)
	_, err := e.export("user@gmail.com", srv.URL, "key", false)
	require.Error(t, err, "export should fail on 500")
	require.ErrorContains(t, err, "500")
	assert.ErrorContains(t, err, "server error")
}

func TestExport_MissingToken(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.Fail(t, "server should not be called when token is missing")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := newTestExporter(srv, t.TempDir())
	_, err := e.export("nobody@gmail.com", srv.URL, "key", false)
	require.Error(t, err, "export should fail with missing token")
	assert.ErrorContains(t, err, "no token found")
}

func TestExport_HTTPSRequired(t *testing.T) {
	e := &tokenExporter{
		httpClient: http.DefaultClient,
		tokensDir:  t.TempDir(),
		stdout:     io.Discard,
		stderr:     io.Discard,
	}

	_, err := e.export("user@gmail.com", "http://nas:8080", "key", false)
	require.Error(t, err, "export should reject http:// without allowInsecure")
	assert.ErrorContains(t, err, "HTTPS required")
}

func TestExport_HTTPAllowedWithInsecure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{"token":"data"}`)

	e := &tokenExporter{
		httpClient: srv.Client(),
		tokensDir:  tokensDir,
		stdout:     io.Discard,
		stderr:     io.Discard,
	}

	result, err := e.export("user@gmail.com", srv.URL, "key", true)
	require.NoError(t, err, "export")
	assert.True(t, result.allowInsecure, "result.allowInsecure should be true")
}

func TestExport_HTTPWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{"token":"data"}`)

	var stderr bytes.Buffer
	e := &tokenExporter{
		httpClient: srv.Client(),
		tokensDir:  tokensDir,
		stdout:     io.Discard,
		stderr:     &stderr,
	}

	_, err := e.export("user@gmail.com", srv.URL, "key", true)
	require.NoError(t, err, "export")
	assert.Contains(t, stderr.String(), "WARNING", "stderr should contain HTTP warning")
}

func TestExport_InvalidEmail(t *testing.T) {
	e := &tokenExporter{
		httpClient: http.DefaultClient,
		tokensDir:  t.TempDir(),
		stdout:     io.Discard,
		stderr:     io.Discard,
	}

	_, err := e.export("not-an-email", "https://nas:8080", "key", false)
	require.Error(t, err, "export should reject invalid email")
	assert.ErrorContains(t, err, "invalid email")
}

func TestExport_AccountPostSuccess(t *testing.T) {
	var accountEmail string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/"):
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/api/v1/accounts":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if email, ok := body["email"].(string); ok {
				accountEmail = email
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{}`)

	e := newTestExporter(srv, tokensDir)
	_, err := e.export("user@gmail.com", srv.URL, "key", false)
	require.NoError(t, err, "export")

	assert.Equal(t, "user@gmail.com", accountEmail, "account email")
}

func TestExport_AccountPostFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		// Account POST fails
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("db error"))
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{}`)

	var stderr bytes.Buffer
	e := &tokenExporter{
		httpClient: srv.Client(),
		tokensDir:  tokensDir,
		stdout:     io.Discard,
		stderr:     &stderr,
	}

	// Should succeed — account POST is best-effort
	result, err := e.export("user@gmail.com", srv.URL, "key", false)
	require.NoError(t, err, "export should succeed even when account POST fails")
	require.NotNil(t, result, "result should not be nil")
	assert.Contains(t, stderr.String(), "Warning", "stderr should warn about account POST failure")
}

func TestExport_AllowInsecureFromConfig(t *testing.T) {
	// Regression: when config has allow_insecure=true for an HTTP URL,
	// export should succeed even without the --allow-insecure flag.
	// This simulates the resolution in runExportToken:
	//   allowInsecure := exportAllowInsecure || cfg.Remote.AllowInsecure
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/token/") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	tokensDir := t.TempDir()
	writeTestToken(t, tokensDir, `{"token":"data"}`)

	e := &tokenExporter{
		httpClient: srv.Client(),
		tokensDir:  tokensDir,
		stdout:     io.Discard,
		stderr:     io.Discard,
	}

	// Simulate: CLI flag is false, but config had allow_insecure=true
	cliFlag := false
	configAllowInsecure := true
	allowInsecure := cliFlag || configAllowInsecure

	result, err := e.export("user@gmail.com", srv.URL, "key", allowInsecure)
	require.NoError(t, err, "export should succeed with config allow_insecure=true")
	assert.True(t, result.allowInsecure, "result.allowInsecure should be true")
}

func TestExport_InvalidScheme(t *testing.T) {
	e := &tokenExporter{
		httpClient: http.DefaultClient,
		tokensDir:  t.TempDir(),
		stdout:     io.Discard,
		stderr:     io.Discard,
	}

	_, err := e.export("user@gmail.com", "ftp://nas:8080", "key", false)
	require.Error(t, err, "export should reject ftp:// scheme")
	assert.ErrorContains(t, err, "http or https")
}
