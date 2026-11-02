package cmd

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestVerificationDelegatedCapabilitiesRequireCurrentDaemon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		version  string
		wantRead bool
	}{
		{name: "legacy", version: "3.9.0"},
		{name: "before verification support", version: "3.38.0"},
		{name: "verification minimum", version: "3.39.0", wantRead: true},
		{name: "current", version: api.APISchemaVersion, wantRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": tc.version}))
				case "/api/v1/agent-tokens/self":
					assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-grant", "permissions": []string{"message.read"}}))
				default:
					assertions.Fail("unexpected request", "%s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			opts, err := delegatedMCPServeOptions(t.Context(), client)
			requirements.NoError(err)
			assertions.True(opts.DelegatedOnly)
			if tc.wantRead {
				assertions.NotNil(opts.Engine)
				assertions.NotNil(opts.AttachmentReader)
			} else {
				assertions.Nil(opts.Engine, "old delegated daemons must not expose unprotected reads")
				assertions.Nil(opts.AttachmentReader)
				assertions.NotEmpty(opts.DraftCommands)
			}
		})
	}
}

func TestVerificationUnmaskFlagRejectsDelegatedMode(t *testing.T) {
	requirements := require.New(t)

	ctx := testInvocationContext(t.Context(), &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}, invocationOptions{
		agentURL: "https://daemon.example", agentTokenFile: "synthetic-token-file",
		agentURLChanged: true, agentTokenChanged: true,
	})
	command := &cobra.Command{}
	command.Flags().Bool("unmask-verification", true, "")
	command.SetContext(ctx)
	err := mcpCmd.RunE(command, nil)
	requirements.ErrorContains(err, "--unmask-verification is unavailable with delegated credentials")
}
