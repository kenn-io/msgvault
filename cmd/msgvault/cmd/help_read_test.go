package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// Malformed selectors must fail before opening the archive and tell the caller
// where the required IDs or revisions can be obtained.
func TestReadAreaValidationRecovery(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
		code int
		hint string
	}{
		{"message ID", func() error { _, err := resolveMessageIDArg("1.5"); return err }, 3, "msgvault search"},
		{"person revision", func() error { return positivePersonCLIRevision(&cobra.Command{Use: "merge"}, 0, "survivor") }, 3, "msgvault person get"},
		{"participant ID", func() error { return validatePersonCLIParticipants(&cobra.Command{Use: "split"}, []int64{0}) }, 3, "participants"},
		{"collection accounts", func() error { _, err := collectionAccountsFromFlag(&cobra.Command{Use: "create"}, ""); return err }, 2, "--accounts"},
		{"export date", func() error { _, _, err := parseMessageExportBounds("yesterday", "2026-10-01T00:00:00Z"); return err }, 3, "2026-09-01T00:00:00Z"},
		{"match token", func() error { cmd := newIdentityMatchesDecisionCommand("accept"); return cmd.RunE(cmd, []string{"1"}) }, 2, "msgvault identity matches show 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			require.Error(t, err)
			classified := classifyCLIError(nil, err)
			assert.Equal(t, tt.code, classified.ExitCode)
			assert.Contains(t, classified.Hint, tt.hint)
		})
	}
}

func TestReadAreaHelpPathsResolve(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	seen := make(map[string]bool)
	assert := assert.New(t)
	for _, help := range readCommandHelp {
		cmd, rest, err := rootCmd.Find(strings.Fields(help.path))
		require.NoError(t, err, help.path)
		assert.Empty(rest, help.path)
		assert.Equal(help.path, helpPath(cmd))
		assert.False(cmd.Hidden, help.path)
		assert.False(seen[help.path], "duplicate help definition for %s", help.path)
		seen[help.path] = true
	}
}

func TestReadAreaTUIRequiresTerminal(t *testing.T) {
	cmd := &cobra.Command{Use: "tui"}
	cmd.SetIn(strings.NewReader(""))
	err := tuiCmd.RunE(cmd, nil)
	require.ErrorContains(t, err, "interactive terminal")
	classified := classifyCLIError(cmd, err)
	assert.Contains(t, classified.Hint, "msgvault search")
}

func TestReadAreaAPIResourceRecovery(t *testing.T) {
	tests := []struct {
		path   string
		status int
		hint   string
	}{
		{"person get", 404, "msgvault person list --json"},
		{"person merge", 409, "msgvault person get"},
		{"organization show", 404, "msgvault organization list --json"},
		{"employment show", 404, "msgvault employment list --person"},
		{"identity matches accept", 409, "msgvault identity matches show"},
		{"carddav conflicts show", 404, "msgvault carddav conflicts list"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			root := &cobra.Command{Use: "msgvault"}
			cmd := root
			for name := range strings.FieldsSeq(tt.path) {
				child := &cobra.Command{Use: name}
				cmd.AddCommand(child)
				cmd = child
			}
			err := &daemonclient.APIError{Status: tt.status, Code: "resource_error", Message: "synthetic failure"}
			diagnostic := classifyCLIError(cmd, err)
			assert.Contains(t, diagnostic.Hint, tt.hint)
			assert.ErrorIs(t, diagnostic, err)
		})
	}
}

func TestReadAreaScoringConsentRecovery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeDaemonTestHealth(t, w)
			return
		}
		assert.Equal(t, "/api/v1/identity/scoring/run", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"results":[],"processed":0,"error":{"code":"consent_required","message":"Scoring stopped because consent was withdrawn"}}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	root := newRootCommand()
	root.PersistentPreRunE = nil
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonScoringCommand())
	root.AddCommand(person)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"person", "scoring", "run"})
	err := root.ExecuteContext(ctx)
	require.ErrorContains(t, err, "consent_required")
	diagnostic := classifyCLIError(nil, err)
	assert.Contains(t, diagnostic.Hint, "msgvault person scoring status --json")
	assert.Contains(t, diagnostic.Hint, "msgvault person scoring consent")
}
