package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex"
)

func TestDocumentMutationsRouteSafelyWithConfiguredRemote(t *testing.T) {
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
			configureRemoteDaemonForTest(t, server.URL)
			documentsConfig := documentindex.DefaultDocumentsConfig()
			documentsConfig.APIKeyEnv = apiKeyEnv
			cfg.Attachments.Documents = documentsConfig
			t.Setenv(apiKeyEnv, apiKey)

			root := &cobra.Command{Use: "msgvault"}
			root.AddCommand(newDocumentsCmd(documentsCommandDeps{}))
			root.SetArgs(test.args)

			err := root.ExecuteContext(t.Context())
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
