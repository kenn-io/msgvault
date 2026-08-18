package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex"
)

func TestDocumentMutationsUseDaemonRunner(t *testing.T) {
	const (
		apiKeyEnv = "MSGVAULT_DOCUMENT_TEST_KEY"
		apiKey    = "synthetic-document-key"
	)
	tests := []struct {
		name       string
		args       []string
		wantArgs   []string
		wantAPIKey bool
	}{
		{
			name: "consent",
			args: []string{"documents", "consent-mistral", "--capabilities", "manifest.json", "--yes"},
			wantArgs: []string{
				"documents", "consent-mistral", "--capabilities=manifest.json", "--yes",
			},
		},
		{
			name: "build",
			args: []string{"documents", "build", "--capabilities", "manifest.json", "--limit", "5", "--yes"},
			wantArgs: []string{
				"documents", "build", "--capabilities=manifest.json", "--limit=5", "--yes",
			},
			wantAPIKey: true,
		},
		{
			name: "resume",
			args: []string{"documents", "resume", "--capabilities", "manifest.json", "--limit", "5", "--yes"},
			wantArgs: []string{
				"documents", "resume", "--capabilities=manifest.json", "--limit=5", "--yes",
			},
			wantAPIKey: true,
		},
		{
			name: "retry",
			args: []string{"documents", "retry", "--capabilities", "manifest.json", "--hash", "abc"},
			wantArgs: []string{
				"documents", "retry", "--capabilities=manifest.json", "--hash=abc",
			},
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
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(t, test.wantArgs, req.Args)
				if test.wantAPIKey {
					assert.Equal(t, apiKey, req.Env[apiKeyEnv])
				} else {
					assert.Empty(t, req.Env)
				}
			}, `{"type":"complete"}`)
			configureRemoteDaemonForTest(t, server.URL)
			documentsConfig := documentindex.DefaultDocumentsConfig()
			documentsConfig.APIKeyEnv = apiKeyEnv
			cfg.Attachments.Documents = documentsConfig
			t.Setenv(apiKeyEnv, apiKey)

			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newDocumentsCmd(documentsCommandDeps{}))
			root.SetArgs(test.args)

			require.NoError(t, root.ExecuteContext(t.Context()))
			assert.Equal(t, 1, int(requests.Load()))
		})
	}
}
