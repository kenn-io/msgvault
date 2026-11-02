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
	tests := []struct {
		name                   string
		version                string
		wantProtectedReadDraft bool
		wantArrivals           bool
	}{
		{name: "much older", version: "3.9.0"},
		{name: "older", version: "3.34.0"},
		{name: "before protected reads", version: "3.35.0"},
		{name: "protected reads and drafts", version: "3.36.0", wantProtectedReadDraft: true},
		{name: "arrivals", version: "3.37.0", wantProtectedReadDraft: true, wantArrivals: true},
		{name: "current API", version: api.APISchemaVersion, wantProtectedReadDraft: true, wantArrivals: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					assertions.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": test.version}))
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
			assertions.Equal(test.wantArrivals, opts.Verifications != nil, "arrival capability")
			assertions.Equal(test.wantProtectedReadDraft, opts.Engine != nil, "protected reads")
			assertions.Equal(test.wantProtectedReadDraft, opts.AttachmentReader != nil, "protected attachments")
			assertions.Equal(test.wantProtectedReadDraft, opts.Drafts != nil, "protected drafts")
			assertions.Equal(test.wantProtectedReadDraft, len(opts.DraftCommands) > 0, "protected draft commands")
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
