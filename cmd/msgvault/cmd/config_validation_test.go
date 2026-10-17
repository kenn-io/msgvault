package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScopedConfigExportAndServe(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, tc := range []struct{ name, content, env, section string }{
		{name: "kata", content: "[integrations.kata]\nendpoint = '://invalid'\n", section: "integrations.kata"},
		{name: "docbank", content: "[integrations.docbank]\nattachment_folder = '../escape'\n", section: "integrations.docbank"},
		{name: "server environment", env: "invalid", section: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			if tc.env != "" {
				t.Setenv("MSGVAULT_API_PORT", tc.env)
			}
			st := testutil.NewTestStore(t)
			root := newRootCommand()
			root.AddCommand(newExportMessagesLocalCmd(exportMessagesDeps{openStore: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil }}))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs([]string{"--home", home, "export-messages", "--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"})
			require.NoError(root.Execute())
			assert.Contains(out.String(), `"record_type":"complete"`)
			assert.Contains(stderr.String(), "Warning:")
			assert.Contains(stderr.String(), tc.section)
			if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}

			server := newRootCommand()
			// Root startup must reject the section before the server can listen.
			server.AddCommand(&cobra.Command{Use: "serve", RunE: runServe})
			server.SetErr(&stderr)
			server.SetArgs([]string{"--home", home, "serve"})
			err := server.Execute()
			require.Error(err)
			if tc.env != "" {
				assert.Contains(err.Error(), "MSGVAULT_API_PORT")
			} else {
				assert.Contains(err.Error(), tc.section)
			}
		})
	}
}

func TestScopedConfigParseErrorsRemainGlobal(t *testing.T) { //nolint:paralleltest // root startup changes the process logger
	for _, content := range []string{"[server\n", "[chatwoot]\nunknown = true\n"} {
		for _, name := range []string{"export-messages", "serve"} {
			t.Run(name+content, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				home := t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				root := newRootCommand()
				ran := false
				root.AddCommand(&cobra.Command{Use: name, Run: func(*cobra.Command, []string) { ran = true }})
				root.SetErr(&bytes.Buffer{})
				root.SetArgs([]string{"--home", home, name})
				require.Error(root.Execute())
				assert.False(ran)
			})
		}
	}
}

func TestScopedConfigRequiredSections(t *testing.T) { //nolint:paralleltest // root startup changes process logging
	for _, tc := range []struct{ group, leaf, content, section string }{
		{"backup", "create", "[backup]\nzstd_level = 20", "backup"},
		{"", "sync-beeper", "[beeper]\nmax_media_mb = -1", "beeper"},
		{"multimodal", "probe", "[vector]\nenabled = true", "vector"},
		{"embeddings", "build", "[vector]\nenabled = true", "vector"},
		{"documents", "probe-mistral", "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{"daemon", "start", "[integrations.kata]\nendpoint = '://invalid'", "integrations.kata"},
	} {
		t.Run(tc.group+tc.leaf, func(t *testing.T) {
			if tc.group == "backup" || tc.group == "embeddings" || tc.leaf == "sync-beeper" {
				t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			}
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			root := newRootCommand()
			leaf := &cobra.Command{Use: tc.leaf, Run: func(*cobra.Command, []string) { assert.Fail("invalid required section reached command") }}
			args := []string{"--home", home}
			if tc.group != "" {
				parent := &cobra.Command{Use: tc.group}
				parent.AddCommand(leaf)
				root.AddCommand(parent)
				args = append(args, tc.group)
			} else {
				root.AddCommand(leaf)
			}
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(append(args, tc.leaf))
			err := root.Execute()
			require.Error(err)
			assert.Contains(err.Error(), tc.section)
		})
	}
}

func TestScopedConfigHTTPReads(t *testing.T) { //nolint:paralleltest // root startup changes process logging
	for _, tc := range []struct {
		path             []string
		content, section string
		semantic         bool
	}{
		{[]string{"backup", "create"}, "[backup]\nzstd_level = 20", "backup", false},
		{[]string{"activity", "build"}, "[activity]\ntimezone = 'Local'", "activity", false},
		{[]string{"sync-beeper"}, "[beeper]\nmax_media_mb = -1", "beeper", false},
		{[]string{"repair-dates"}, "[analytics]\nengine = 'invalid'", "analytics", false},
		{[]string{"documents", "search"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents", false},
		{[]string{"documents", "status"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents", false},
		{[]string{"person", "brief", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep", false},
		{[]string{"person", "scoring", "status"}, "[people.identity_scoring]\nbatch_size = 0", "people.identity_scoring", false},
		{[]string{"person", "provider", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep", true},
	} {
		t.Run(tc.section+tc.path[len(tc.path)-1], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			root := newRootCommand()
			parent := root
			for _, name := range tc.path[:len(tc.path)-1] {
				next := &cobra.Command{Use: name}
				parent.AddCommand(next)
				parent = next
			}
			reached := false
			leaf := &cobra.Command{Use: tc.path[len(tc.path)-1], Run: func(*cobra.Command, []string) { reached = true }}
			leaf.Flags().Bool("semantic-embeddings", false, "")
			parent.AddCommand(leaf)
			args := append([]string{"--home", home}, tc.path...)
			if tc.semantic {
				args = append(args, "--semantic-embeddings")
			}
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			root.SetArgs(args)
			require.NoError(root.Execute())
			assert.True(reached)
			assert.Contains(stderr.String(), tc.section)
			if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
				inv.logResult.Close()
			}
		})
	}
}

func TestScopedConfigMCPStatus(t *testing.T) { //nolint:paralleltest // root invocation state
	require := require.New(t)
	assert := assert.New(t)
	home := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte("[integrations.kata]\nendpoint = '://invalid'"), 0600))
	root := newRootCommand()
	mcp := &cobra.Command{Use: "mcp"}
	mcp.AddCommand(newMCPStatusCommand())
	root.AddCommand(mcp)
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--home", home, "mcp", "status"})
	require.NoError(root.Execute())
	assert.Contains(out.String(), "No HTTP MCP listeners")
	assert.Contains(stderr.String(), "integrations.kata")
}

func TestScopedConfigPolicySnapshots(t *testing.T) { //nolint:paralleltest // snapshot helpers share command setup
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[integrations.kata]\nendpoint = '://invalid'"), 0600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	_, err = personProviderConfigFromSnapshot(personProviderCommandDeps{}, snapshot)
	require.NoError(err)
	_, err = loadSetupConfig(snapshot, home)
	require.NoError(err)
	cfg, err := config.LoadForSections(path, home, config.RuntimeOverrides{})
	require.NoError(err)
	_, err = currentSemanticPersonVectorConfigSource(&invocation{cfg: cfg})()
	require.NoError(err)
	// Preparing a replacement daemon must fail before stopping a healthy one.
	require.ErrorContains(prepareServeConfig(cfg), "integrations.kata")
}

// The HTTP peer implements the daemon wire protocol so these regressions run
// real CLI startup and forwarding without making provider network requests.
func TestScopedConfigForwardingPeer(t *testing.T) { //nolint:paralleltest // command flags and root logging use process state
	resetSyncFullFlagsForTest(t)
	oldJSON, oldWatch, oldSource := embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource
	oldFull, oldYes, oldBackstop := embedFullRebuild, embedYes, embedBackstop
	oldAccounts, oldCollections := embedAccounts, embedCollections
	t.Cleanup(func() {
		embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource = oldJSON, oldWatch, oldSource
		embedFullRebuild, embedYes, embedBackstop = oldFull, oldYes, oldBackstop
		embedAccounts, embedCollections = oldAccounts, oldCollections
	})
	embeddingsStatusJSON, embeddingsStatusWatch, embeddingsStatusSource = true, false, 0
	for _, tc := range []struct {
		args             []string
		content, section string
	}{
		{[]string{"sync", "user@example.com"}, "[oauth]\nclient_secrets_command = []", "oauth"},
		{[]string{"sync-full", "user@example.com"}, "[oauth]\nclient_secrets_command = []", "oauth"},
		{[]string{"add-account", "user@example.com"}, "[oauth]\nclient_secrets_command = []", "oauth"},
		{[]string{"add-calendar", "user@example.com"}, "[oauth]\nclient_secrets_command = []", "oauth"},
		{[]string{"add-synctech-sms-drive", "synthetic", "--owner-phone", "+12025550123", "--folder-id", "synthetic", "--google-account", "user@example.com"}, "[oauth]\nclient_secrets_command = []", "oauth"},
		{[]string{"person", "provider", "check"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep"},
		{[]string{"add-beeper"}, "[beeper]\nmax_media_mb = -1", "beeper"},
		{[]string{"add-slack"}, "[slack]\nmax_media_mb = -1", "slack"},
		{[]string{"add-discord"}, "[discord]\nmax_media_mb = -1", "discord"},
		{[]string{"add-teams", "user@example.com"}, "[teams]\nmax_media_mb = -1", "teams"},
		{[]string{"embeddings", "build", "--yes"}, "[vector]\nenabled = true", "vector"},
		{[]string{"embeddings", "status"}, "[vector]\nenabled = true", "vector"},
		{[]string{"person", "provider", "status"}, "[people.sweep]\nwork_batch_size = -1", "people.sweep"},
		{[]string{"person", "provider", "status", "--semantic-embeddings"}, "[vector]\nenabled = true", "vector"},
		{[]string{"documents", "consent-docling", "--yes"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{[]string{"documents", "retire", "synthetic-profile", "--yes"}, "[attachments.documents]\nprovider = 'invalid'", "attachments.documents"},
		{[]string{"carddav", "connections"}, "[carddav]\nprovider = 'invalid'", "carddav"},
		{[]string{"sync-carddav"}, "[carddav_connections.default]\nprovider = 'google'", "carddav_connections"},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var requests atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, &requests, nil))
			for _, path := range []string{"/api/v1/cli/sync", "/api/v1/cli/sync-full"} {
				mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = fmt.Fprintln(w, `{"type":"complete"}`)
				})
			}
			mux.HandleFunc("/api/v1/cli/add-calendar/plan", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"needs_scope_escalation":false}`)
			})
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, embeddingsStatusMinAPISchemaVersion)
			})
			for path, response := range map[string]string{
				"/api/v1/embeddings/status":   `{"pending":0}`,
				"/api/v1/carddav/connections": `{"connections":[]}`,
				"/api/v1/carddav/sync":        `{"books":0,"created":0,"updated":0,"removed":0}`,
			} {
				mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, response)
				})
			}
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			home := t.TempDir()
			content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n%s", server.URL, tc.content)
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
			root := newRootCommand()
			embeddings := &cobra.Command{Use: embeddingsCommandName}
			embeddings.AddCommand(newEmbeddingsBuildCmd("build"), &cobra.Command{Use: statusValue, RunE: embeddingsStatusCmd.RunE})
			person := &cobra.Command{Use: "person"}
			person.AddCommand(newPersonProviderCommand(defaultPersonProviderCommandDeps()))
			root.AddCommand(embeddings, person, newDocumentsCmd(defaultDocumentsCommandDeps()), newCardDAVCmd(), newSyncCardDAVCmd(),
				&cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE},
				&cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE},
				newAddAccountCmd(), newAddCalendarCmd(), newAddSynctechSMSDriveCmd(), newAddBeeperCmd(), newAddSlackCmd(), newAddDiscordCmd(discordCommandDeps{}), newAddTeamsCmd())
			t.Setenv("MSGVAULT_BEEPER_TOKEN", "synthetic-token")
			t.Setenv("MSGVAULT_SLACK_TOKEN", "xoxb-synthetic")
			root.SetIn(strings.NewReader("synthetic-discord-token\n"))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"--home", home}, tc.args...))
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})
			require.NoError(root.Execute())
			assert.Equal(int32(1), requests.Load(), "the real command reached the responding daemon")
			assert.Contains(stderr.String(), "Warning: unused config section ["+tc.section+"]")
		})
	}
}

func TestScopedConfigLocalControlsIgnoreRemote(t *testing.T) { //nolint:paralleltest // runtime environment and root logging
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "invalid")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "local daemon controls must not contact the remote", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	t.Run("remote consumer rejects invalid remote section", func(t *testing.T) {
		require := require.New(t)
		home := t.TempDir()
		content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n", server.URL)
		require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
		root := newRootCommand()
		root.AddCommand(newSyncCardDAVCmd())
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"--home", home, "sync-carddav"})
		require.ErrorContains(root.Execute(), "MSGVAULT_REMOTE_ALLOW_INSECURE")
		assert.Zero(t, requests.Load())
	})
	for _, group := range []string{"daemon", "serve"} {
		for _, action := range []string{statusValue, "stop"} {
			t.Run(group+action, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				home := t.TempDir()
				content := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n", server.URL)
				require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				root := newRootCommand()
				if group == "daemon" {
					root.AddCommand(newDaemonCommand())
				} else {
					serve := &cobra.Command{Use: "serve"}
					serve.AddCommand(newLifecycleCommand(statusValue, true), newLifecycleCommand("stop", true))
					root.AddCommand(serve)
				}
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				root.SetArgs([]string{"--home", home, group, action})
				t.Cleanup(func() {
					if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
						inv.logResult.Close()
					}
				})
				require.NoError(root.Execute())
				assert.Zero(requests.Load())
				assert.Contains(stderr.String(), "Warning: unused config section [remote]")
			})
		}
	}
}

func TestScopedConfigTokenUploadRequiresRemote(t *testing.T) { //nolint:paralleltest // command flags and runtime environment
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "falsee")
	oldTo, oldKey, oldAllow, oldUpload := exportTokenTo, exportTokenAPIKey, exportAllowInsecure, exportTokenUploadOnly
	t.Cleanup(func() {
		exportTokenTo, exportTokenAPIKey, exportAllowInsecure, exportTokenUploadOnly = oldTo, oldKey, oldAllow, oldUpload
	})
	for _, destination := range []string{"config", "flag"} {
		for _, override := range []string{"", "true", "false"} {
			t.Run(fmt.Sprint(destination, override), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				var uploads atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal("/api/v1/auth/token/user@example.com", r.URL.Path)
					uploads.Add(1)
					w.WriteHeader(http.StatusCreated)
				}))
				t.Cleanup(server.Close)
				home := t.TempDir()
				content := "[remote]\nallow_insecure = true\n"
				if destination == "config" {
					content += fmt.Sprintf("url = %q\n", server.URL)
				}
				require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				writeTestToken(t, filepath.Join(home, "tokens"), `{"token":"synthetic"}`)
				root := newRootCommand()
				token := &cobra.Command{Use: exportTokenCmd.Use, Args: exportTokenCmd.Args, RunE: exportTokenCmd.RunE}
				token.Flags().StringVar(&exportTokenTo, "to", "", "")
				token.Flags().StringVar(&exportTokenAPIKey, "api-key", "", "")
				token.Flags().BoolVar(&exportAllowInsecure, "allow-insecure", false, "")
				token.Flags().BoolVar(&exportTokenUploadOnly, "upload-only", false, "")
				root.AddCommand(token)
				root.SetErr(&bytes.Buffer{})
				args := []string{"--home", home, "export-token", "user@example.com", "--api-key", "synthetic-key", "--upload-only"}
				if destination == "flag" {
					args = append(args, "--to", server.URL)
				}
				if override != "" {
					args = append(args, "--allow-insecure="+override)
				}
				root.SetArgs(args)
				t.Cleanup(func() {
					if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
						inv.logResult.Close()
					}
				})
				err := root.Execute()
				switch override {
				case "true":
					require.NoError(err)
					assert.Equal(int32(1), uploads.Load())
				case "false":
					require.ErrorContains(err, "HTTPS required")
					assert.NotContains(err.Error(), "MSGVAULT_REMOTE_ALLOW_INSECURE")
					assert.Zero(uploads.Load())
				default:
					require.ErrorContains(err, "MSGVAULT_REMOTE_ALLOW_INSECURE")
					assert.Zero(uploads.Load())
				}
			})
		}
	}
}

func TestScopedConfigMuesliHookRequiresSource(t *testing.T) { //nolint:paralleltest // command flags and root logger
	oldInstall := muesliHookInstall
	t.Cleanup(func() { muesliHookInstall = oldInstall })
	for _, install := range []bool{false, true} {
		t.Run(strconv.FormatBool(install), func(t *testing.T) {
			if install && runtime.GOOS == "windows" {
				t.Skip("native hook installation requires symlinks")
			}
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			content := fmt.Sprintf("[remote]\nurl = 'http://127.0.0.1:1'\nallow_insecure = true\n[[muesli]]\nidentifier = 'synthetic'\naccount_email = 'user@example.com'\nenabled = true\ndb_path = %q\nphone_country_code = '1234'\n", filepath.Join(home, "missing.db"))
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
			root := newRootCommand()
			hook := &cobra.Command{Use: muesliHookCmd.Use, Args: muesliHookCmd.Args, RunE: muesliHookCmd.RunE}
			hook.Flags().StringVar(&muesliHookInstall, "install", "", "")
			root.AddCommand(hook)
			root.SetIn(strings.NewReader(`{"schemaVersion":1,"event":"meeting.completed","kind":"meeting","id":42,"completedAt":"2026-09-01T15:00:00Z"}`))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			args := []string{"--home", home, "muesli-hook"}
			if install {
				args = append(args, "--install", filepath.Join(home, "launchers"))
			}
			root.SetArgs(args)
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})
			err := root.Execute()
			if install {
				require.NoError(err)
				assert.Contains(stderr.String(), "Warning: unused config section [muesli]")
			} else {
				require.ErrorContains(err, "phone_country_code")
			}
		})
	}
}

func TestScopedConfigLocalInspectionIgnoresRemote(t *testing.T) { //nolint:paralleltest // runtime environment and root logger
	t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "invalid")
	for _, args := range [][]string{{"setup", "status"}, {"documents", "probe-mistral", "--fixtures", "synthetic-fixtures", "--validate-only"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte("[remote]\nurl = 'http://127.0.0.1:1'\n"), 0600))
			root := newRootCommand()
			setup := &cobra.Command{Use: "setup"}
			setup.AddCommand(newSetupStatusCommand(defaultSetupStatusDeps()))
			deps := defaultDocumentsCommandDeps()
			validated := false
			deps.validateProbeFixtures = func(_ context.Context, _ mistral.Policy, fixtures mistral.ProbeFixtureConfig) error {
				assert.Equal("synthetic-fixtures", fixtures.FixtureDirectory)
				validated = true
				return nil
			}
			root.AddCommand(setup, newDocumentsCmd(deps))
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(append([]string{"--home", home}, args...))
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})
			require.NoError(root.Execute())
			assert.Contains(stderr.String(), "Warning: unused config section [remote]")
			assert.Equal(args[0] == "documents", validated)
		})
	}
}

func TestScopedConfigLocalRoutePolicies(t *testing.T) { //nolint:paralleltest // root logging and environment
	for _, tc := range []struct {
		path             []string
		content, section string
		flags            map[string]string
		worker, rejects  bool
	}{
		{path: []string{"multimodal", "probe"}, content: "[remote]\nurl = 'http://127.0.0.1:1'", section: "remote"},
		{path: []string{"multimodal", "status"}, content: "[remote]\nurl = 'http://127.0.0.1:1'", section: "remote", rejects: true},
		{path: []string{"multimodal", "status"}, content: "[vector]\nenabled = true", section: "vector"},
		{path: []string{"eval"}, content: "[vector]\nenabled = true", section: "vector", flags: map[string]string{"modes": "fts"}},
		{path: []string{"eval"}, content: "[vector]\nenabled = true", section: "vector", flags: map[string]string{"modes": "fts", "rerank-jev": "synthetic"}, rejects: true},
		{path: []string{"person", "provider", "login"}, content: "[remote]\nurl = 'http://127.0.0.1:1'", section: "remote"},
		{path: []string{"person", "provider", "models"}, content: "[remote]\nurl = 'http://127.0.0.1:1'", section: "remote"},
		{path: []string{"fake-vault"}, content: "[remote]\nurl = 'http://127.0.0.1:1'", section: "remote"},
		{path: []string{"setup", "status"}, content: "[oauth]\nclient_secrets_command = []", section: "oauth"},
		{path: []string{"add-account"}, content: "[oauth]\nclient_secrets_command = []", section: "oauth", flags: map[string]string{"headless": "true"}},
		{path: []string{"add-calendar"}, content: "[oauth]\nclient_secrets_command = []", section: "oauth", flags: map[string]string{"headless": "true"}},
		{path: []string{"add-account"}, content: "[oauth]\nclient_secrets_command = []", section: "oauth", flags: map[string]string{"headless": "true"}, worker: true, rejects: true},
		{path: []string{"documents", "probe-mistral"}, content: "[vector]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true", section: "vector"},
		{path: []string{"documents", "retire"}, content: "[attachments.documents]\nprovider = 'invalid'", section: "attachments.documents", worker: true},
		{path: []string{"documents", "purge-derived"}, content: "[attachments.documents]\nprovider = 'invalid'", section: "attachments.documents", worker: true},
		{path: []string{"documents", "vectors", "retry"}, content: "[vector]\nenabled = true\n[attachments.documents.index.embeddings]\nenabled = true", section: "vector", worker: true},
		{path: []string{"repair-encoding"}, content: "[vector]\nenabled = true", section: "vector", worker: true, rejects: true},
	} {
		t.Run(fmt.Sprint(tc.path, tc.section, tc.flags), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			if tc.section == "remote" {
				t.Setenv("MSGVAULT_REMOTE_ALLOW_INSECURE", "invalid")
			}
			if tc.worker {
				t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			}
			home := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.content), 0600))
			root := newRootCommand()
			parent := root
			for _, name := range tc.path[:len(tc.path)-1] {
				next := &cobra.Command{Use: name}
				parent.AddCommand(next)
				parent = next
			}
			reached := false
			leaf := &cobra.Command{Use: tc.path[len(tc.path)-1], Run: func(*cobra.Command, []string) { reached = true }}
			for flag, value := range tc.flags {
				if flag == "headless" {
					leaf.Flags().Bool(flag, value == "true", "")
				} else {
					leaf.Flags().String(flag, value, "")
				}
			}
			parent.AddCommand(leaf)
			root.SetArgs(append([]string{"--home", home}, tc.path...))
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})
			err := root.Execute()
			if tc.rejects {
				require.Error(err)
				assert.Contains(strings.ToLower(err.Error()), tc.section)
				assert.False(reached)
			} else {
				require.NoError(err)
				assert.True(reached)
				assert.Contains(stderr.String(), "Warning: unused config section ["+tc.section+"]")
			}
		})
	}
}

func TestScopedConfigSourceConsumers(t *testing.T) { //nolint:paralleltest // store ownership and invocation state
	requirements := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[oauth]\nclient_secrets_command = []"), 0600))
	cfg, err := config.LoadForSections(path, home, config.RuntimeOverrides{})
	requirements.NoError(err)
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	t.Run("Drive source", func(t *testing.T) {
		require := require.New(t)
		_, err := newSynctechSMSDriveClient(ctx, config.SynctechSMSSource{OAuthApp: "default", GoogleAccount: "user@example.com"})
		require.ErrorContains(err, "client_secrets_command")
	})
	t.Run("Gmail repair client", func(t *testing.T) {
		require := require.New(t)
		_, err := buildAPIClient(ctx, &store.Source{SourceType: sourceTypeGmail, Identifier: "user@example.com"}, oauthManagerCache(invocationFromContext(ctx)), nil)
		require.ErrorContains(err, "client_secrets_command")
	})
	t.Run("Gmail removal before mutation", func(t *testing.T) {
		require := require.New(t)
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		require.NoError(st.InitSchema())
		src, err := st.GetOrCreateSource(sourceTypeGmail, "user@example.com")
		require.NoError(err)
		require.NoError(st.Close())
		command := newRemoveAccountLocalTestCmd()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--source-id", strconv.FormatInt(src.ID, 10), "--yes"})
		require.ErrorContains(command.ExecuteContext(ctx), "client_secrets_command")
		st, err = store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		remaining, err := st.GetSourceByID(src.ID)
		require.NoError(err)
		assert.NotNil(t, remaining)
	})
}

func TestScopedConfigSyncIMAPDoesNotValidateUnusedOAuth(t *testing.T) { //nolint:paralleltest // process output and daemon environment
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	for _, tc := range []struct {
		name string
		runE func(*cobra.Command, []string) error
	}{
		{name: "sync", runE: syncIncrementalCmd.RunE},
		{name: "sync-full", runE: syncFullCmd.RunE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			home := t.TempDir()
			configPath := filepath.Join(home, "config.toml")
			require.NoError(os.WriteFile(configPath, []byte("[oauth]\nclient_secrets_command = []"), 0600))
			cfg, err := config.LoadForSections(configPath, home, config.RuntimeOverrides{})
			require.NoError(err)
			st, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			require.NoError(st.InitSchema())
			_, err = st.GetOrCreateSource(sourceTypeIMAP, "imap@example.test")
			require.NoError(err)
			require.NoError(st.Close())

			root := newRootCommand()
			root.AddCommand(addManualSyncCacheFlags(&cobra.Command{
				Use:  tc.name,
				Args: cobra.MaximumNArgs(1),
				RunE: tc.runE,
			}))
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			root.SetArgs([]string{"--home", home, tc.name})
			t.Cleanup(func() {
				if inv := invocationFromCommand(root); inv != nil && inv.logResult != nil {
					inv.logResult.Close()
				}
			})

			getOutput := captureStdout(t)
			err = root.Execute()
			output := getOutput()
			require.ErrorContains(err, "no accounts are ready to sync")
			assert.NotContains(err.Error(), "client_secrets_command")
			assert.Contains(output, "Skipping imap@example.test")
			assert.Contains(stderr.String(), "Warning: unused config section [oauth]")
		})
	}
}
