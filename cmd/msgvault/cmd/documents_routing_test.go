package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex"
)

func TestDoclingMutationsRouteWithoutCapabilityFiles(t *testing.T) {
	for _, test := range []struct {
		name           string
		args, wantArgs []string
		forwardKey     bool
	}{
		{"consent", []string{"documents", "consent-docling", "--yes"}, []string{"documents", "consent-docling", "--yes"}, false},
		{"build", []string{"documents", "build", "--yes"}, []string{"documents", "build", "--yes"}, true},
		{"resume", []string{"documents", "resume", "--yes"}, []string{"documents", "resume", "--yes"}, true},
		{"retry", []string{"documents", "retry", "--hash", "synthetic-hash"}, []string{"documents", "retry", "--hash=synthetic-hash"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(t, test.wantArgs, req.Args)
				if test.forwardKey {
					assert.Equal(t, map[string]string{"SYNTHETIC_DOCLING_KEY": "synthetic-key"}, req.Env)
				} else {
					assert.Empty(t, req.Env)
				}
			}, `{"type":"complete"}`)
			ctx := configureRemoteDaemonForTest(t, server.URL)
			cfg := invocationFromContext(ctx).cfg
			cfg.Attachments.Documents.Provider = documentindex.ProviderDocling
			cfg.Attachments.Documents.Endpoint = "https://docling.example.com"
			cfg.Attachments.Documents.ApplyConfiguredProviderDefaults(func(string) bool { return false })
			cfg.Attachments.Documents.APIKeyEnv = "SYNTHETIC_DOCLING_KEY"
			t.Setenv("SYNTHETIC_DOCLING_KEY", "synthetic-key")
			t.Setenv("MISTRAL_API_KEY", "synthetic-unused-key")
			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newDocumentsCmd(documentsCommandDeps{}))
			root.SetArgs(test.args)
			require.NoError(t, root.ExecuteContext(ctx))
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestDocumentVectorCommandsRouteWithConfiguredRemote(t *testing.T) {
	cfg := testConfigValue()

	const (
		apiKeyEnv = "MSGVAULT_VECTOR_TEST_KEY"
		apiKey    = "synthetic-vector-key"
	)
	tests := []struct {
		name       string
		args       []string
		wantArgs   []string
		wantAPIKey bool
	}{
		{name: "consent", args: []string{"documents", "vectors", "consent", "--yes"}, wantArgs: []string{"documents", "vectors", "consent", "--yes"}},
		{name: "build", args: []string{"documents", "vectors", "build", "--limit", "5"}, wantArgs: []string{"documents", "vectors", "build", "--limit=5"}, wantAPIKey: true},
		{name: "resume", args: []string{"documents", "vectors", "resume", "--generation-id", "7", "--limit", "5"}, wantArgs: []string{"documents", "vectors", "resume", "--generation-id=7", "--limit=5"}, wantAPIKey: true},
		{name: "retry", args: []string{"documents", "vectors", "retry", "--generation-id", "7"}, wantArgs: []string{"documents", "vectors", "retry", "--generation-id=7"}},
		{name: "rebuild", args: []string{"documents", "vectors", "rebuild", "--generation-id", "7", "--yes"}, wantArgs: []string{"documents", "vectors", "rebuild", "--generation-id=7", "--yes"}, wantAPIKey: true},
		{name: "retire", args: []string{"documents", "vectors", "retire", "--generation-id", "7", "--yes"}, wantArgs: []string{"documents", "vectors", "retire", "--generation-id=7", "--yes"}},
		{name: "status", args: []string{"documents", "vectors", "status", "--json"}, wantArgs: []string{"documents", "vectors", "status", "--json"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(test.wantArgs, req.Args)
				if test.wantAPIKey {
					assert.Equal(map[string]string{apiKeyEnv: apiKey}, req.Env)
				} else {
					assert.Empty(req.Env)
				}
			}, `{"type":"complete"}`)
			testCtx := configureRemoteDaemonForTest(t, server.URL)
			cfg = invocationFromContext(testCtx).cfg
			cfg.Vector.Embeddings.APIKeyEnv = apiKeyEnv
			t.Setenv(apiKeyEnv, apiKey)

			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newDocumentsCmd(documentsCommandDeps{}))
			root.SetContext(testCtx)
			root.SetArgs(test.args)

			require.NoError(t, root.ExecuteContext(testCtx))
			assert.Equal(1, int(requests.Load()))
		})
	}
}

func TestDocumentMutationsRouteSafelyWithConfiguredRemote(t *testing.T) {
	cfg := testConfigValue()

	const (
		apiKeyEnv = "MSGVAULT_DOCUMENT_TEST_KEY"
		apiKey    = "synthetic-document-key"
	)
	tests := []struct {
		name       string
		args       []string
		wantArgs   []string
		wantAPIKey bool
		localFile  bool
	}{
		{
			name: "consent",
			args: []string{"documents", "consent-mistral", "--capabilities", "manifest.json", "--yes"},
			wantArgs: []string{
				"documents", "consent-mistral", "--capabilities=manifest.json", "--yes",
			},
			localFile: true,
		},
		{
			name: "build",
			args: []string{"documents", "build", "--capabilities", "manifest.json", "--limit", "5", "--yes"},
			wantArgs: []string{
				"documents", "build", "--capabilities=manifest.json", "--limit=5", "--yes",
			},
			wantAPIKey: true,
			localFile:  true,
		},
		{
			name: "resume",
			args: []string{"documents", "resume", "--capabilities", "manifest.json", "--limit", "5", "--yes"},
			wantArgs: []string{
				"documents", "resume", "--capabilities=manifest.json", "--limit=5", "--yes",
			},
			wantAPIKey: true,
			localFile:  true,
		},
		{
			name: "retry",
			args: []string{"documents", "retry", "--capabilities", "manifest.json", "--hash", "abc"},
			wantArgs: []string{
				"documents", "retry", "--capabilities=manifest.json", "--hash=abc",
			},
			localFile: true,
		},
		{
			name: "retire",
			args: []string{"documents", "retire", "profile-test", "--yes"},
			wantArgs: []string{
				"documents", "retire", "--yes", "profile-test",
			},
		},
		{
			name: "purge",
			args: []string{"documents", "purge-derived", "--hash", "abc", "--yes"},
			wantArgs: []string{
				"documents", "purge-derived", "--hash=abc", "--yes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(test.wantArgs, req.Args)
				if test.wantAPIKey {
					assert.Equal(apiKey, req.Env[apiKeyEnv])
				} else {
					assert.Empty(req.Env)
				}
			}, `{"type":"complete"}`)
			testCtx := configureRemoteDaemonForTest(t, server.URL)
			cfg = invocationFromContext(testCtx).cfg
			documentsConfig := documentindex.DefaultDocumentsConfig()
			documentsConfig.APIKeyEnv = apiKeyEnv
			cfg.Attachments.Documents = documentsConfig
			t.Setenv(apiKeyEnv, apiKey)

			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newDocumentsCmd(documentsCommandDeps{}))
			root.SetContext(testCtx)
			root.SetArgs(test.args)

			err := root.ExecuteContext(testCtx)
			if test.localFile {
				require.ErrorContains(err, "run it on the daemon host with --local")
				assert.Equal(0, int(requests.Load()))
				return
			}
			require.NoError(err)
			assert.Equal(1, int(requests.Load()))
		})
	}
}
