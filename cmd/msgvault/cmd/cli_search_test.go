package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
	"hegel.dev/go/hegel"
)

var cliSearchTasks = []struct {
	query, command string
}{
	{"find email by sender", "msgvault search"},
	{"read a message by id", "msgvault show-message"},
	{"read a conversation thread", "msgvault show-thread"},
	{"export a message as eml", "msgvault export-eml"},
	{"list archived accounts", "msgvault list-accounts"},
	{"search extracted document attachments", "msgvault documents search"},
	{"find words in transcripts", "msgvault media search"},
	{"search attachment pixels with a query image", "msgvault multimodal search"},
	{"search contacts semantically by free text", "msgvault person search"},
	{"log a note about a person", "msgvault person notes append"},
	{"read daemon logs", "msgvault logs"},
	{"show daemon status", "msgvault daemon status"},
	{"import a maildir folder", "msgvault import-maildir"},
	{"show optional feature readiness", "msgvault setup status"},
}

// Keep the oracle independent of the production exclusion predicate.
var cliSearchExcluded = []string{
	"msgvault", "msgvault cli", "msgvault cli search", "msgvault tui", "msgvault setup",
	"msgvault add-account", "msgvault add-calendar", "msgvault add-o365", "msgvault add-circleback",
	"msgvault add-inline", "msgvault add-teams", "msgvault add-plaud", "msgvault add-synctech-sms-drive",
	"msgvault carddav authorize-google", "msgvault carddav authorize-microsoft",
	"msgvault person provider login", "msgvault person provider enroll-codex",
}

func executeCLIDiscovery(tb testing.TB, args ...string) (string, string, error) {
	tb.Helper()
	cli, _, err := rootCmd.Find([]string{"cli"})
	require.NoError(tb, err)
	for _, command := range append(cli.Commands(), cli) {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			require.NoError(tb, flag.Value.Set(flag.DefValue))
			flag.Changed = false
		})
	}
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(args)
	err = executeRootContext(tb.Context(), rootCmd)
	return stdout.String(), stderr.String(), err
}

func skipHegelWithoutEmbeddedRuntime(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		t.Skip("Hegel does not bundle libhegel for darwin/amd64")
	}
}

// Discovery must not inherit the archive or delegated-client lifecycle.
func TestCLISearchWithoutRuntime(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("MSGVAULT_HOME", filepath.Join(home, "archive"))
	config := filepath.Join(t.TempDir(), "invalid.toml")
	requirements.NoError(os.WriteFile(config, []byte("invalid = ["), 0o600))
	ensureHelpLayout(rootCmd)
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"--config", config, "--home", filepath.Join(home, "missing"),
		"--agent-url", "https://127.0.0.1:1", "--agent-token-file", filepath.Join(home, "missing-token"),
		"cli", "search", "extracted", "document", "attachments", "--json"})
	requirements.NoError(executeRootContext(t.Context(), rootCmd), "%s", stderr.String())
	var response struct {
		Query   string           `json:"query"`
		Results []map[string]any `json:"results"`
	}
	requirements.NoError(json.Unmarshal(stdout.Bytes(), &response))
	assertions.Equal("extracted document attachments", response.Query)
	requirements.NotEmpty(response.Results)
	for _, result := range response.Results {
		assertions.Len(result, 3)
		assertions.NotEmpty(result["command"])
		assertions.NotEmpty(result["summary"])
		assertions.NotEmpty(result["usage"])
	}
	assertions.Empty(stderr.String())
	files, err := os.ReadDir(home)
	requirements.NoError(err)
	assertions.Empty(files, "discovery must leave HOME untouched")
}

func TestCLISearchContract(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("MSGVAULT_HOME", filepath.Join(home, "archive"))
	config := filepath.Join(t.TempDir(), "invalid.toml")
	requirements.NoError(os.WriteFile(config, []byte("invalid = ["), 0o600))
	ensureHelpLayout(rootCmd)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing query", []string{"cli", "search"}},
		{"blank query", []string{"cli", "search", " \t\n "}},
		{"zero limit", []string{"cli", "search", "message", "--limit", "0"}},
		{"negative limit", []string{"cli", "search", "message", "--limit", "-1"}},
		{"large limit", []string{"cli", "search", "message", "--limit", "21"}},
		{"unknown child", []string{"cli", "not-a-command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			args := append([]string{"--config", config, "--log-level", "invalid"}, tc.args...)
			out, stderr, err := executeCLIDiscovery(t, args...)
			requirements.Error(err)
			assertions.Contains(out+stderr, "Usage:")
			assertions.NotContains(out+stderr, "load config")
			files, err := os.ReadDir(home)
			requirements.NoError(err)
			assertions.Empty(files, "invalid discovery must leave HOME untouched")
		})
	}
	query := "zzzxqvzzzxqv"
	for _, asJSON := range []bool{false, true} {
		args := []string{"cli", "search", query}
		if asJSON {
			args = append(args, "--json")
		}
		out, stderr, err := executeCLIDiscovery(t, args...)
		requirements.NoError(err)
		assertions.Empty(stderr)
		if asJSON {
			assertions.JSONEq(fmt.Sprintf(`{"query":%q,"results":[]}`, query), out)
		} else {
			assertions.Contains(out, "msgvault --help")
		}
	}
	out, stderr, err := executeCLIDiscovery(t, "cli")
	requirements.NoError(err)
	assertions.Contains(out, "search")
	assertions.Empty(stderr)
	out, stderr, err = executeCLIDiscovery(t, "cli", "search", "export", "eml")
	requirements.NoError(err)
	assertions.Contains(out, "msgvault export-eml")
	assertions.Contains(out, "--help")
	assertions.Empty(stderr)
}

func TestCLISearchTasks(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	for _, task := range cliSearchTasks {
		t.Run(task.query, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			out, stderr, err := executeCLIDiscovery(t, "cli", "search", task.query, "--json")
			requirements.NoError(err, "%s", stderr)
			var response clisearch.Response
			requirements.NoError(json.Unmarshal([]byte(out), &response))
			requirements.NotEmpty(response.Results)
			assertions.Equal(task.command, response.Results[0].Command, "%+v", response.Results)
			for _, result := range response.Results {
				assertions.NotContains(cliSearchExcluded, result.Command)
			}
		})
	}
}

func TestCLISearchKeepsMatrixEligibleWithCredentialFileOrStdin(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	ensureHelpLayout(rootCmd)
	query := "add Matrix account with a password file or a password from stdin"
	out, stderr, err := executeCLIDiscovery(t, "cli", "search", query, "--limit", "20", "--json")
	requirements.NoError(err, "%s", stderr)
	var response clisearch.Response
	requirements.NoError(json.Unmarshal([]byte(out), &response))
	assertions.Equal(query, response.Query)
	assertions.Empty(stderr)
	matrixEligible := false
	for _, result := range response.Results {
		if result.Command == "msgvault add-matrix" {
			matrixEligible = true
			break
		}
	}
	assertions.True(matrixEligible, "Matrix account setup should remain discoverable when credential files or stdin avoid interactive login: %+v", response.Results)
}

func TestCLISearchExcludesInlineBrowserAuthorization(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	out, stderr, err := executeCLIDiscovery(t, "cli", "search", "authorize Inline account", "--limit", "20", "--json")
	require.NoError(t, err, "%s", stderr)
	var response clisearch.Response
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	for _, result := range response.Results {
		assert.NotEqual(t, "msgvault add-inline", result.Command)
	}
}

func TestCLISearchSkipsInteractiveCommandsAndHelpGroups(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, layout := range []bool{false, true} {
		if layout {
			ensureHelpLayout(rootCmd)
		}
		queries := []string{"authorize teams", "authorize plaud", "configure drive sms", "enroll codex", "terminal setup", "login", "authorize google microsoft"}
		for _, task := range cliSearchTasks {
			queries = append(queries, task.query)
		}
		for _, query := range queries {
			for _, result := range clisearch.Search(rootCmd, query, 20, cliSearchOptions()) {
				assertions.NotContains(cliSearchExcluded, result.Command)
				command, _, err := rootCmd.Find(strings.Fields(strings.TrimPrefix(result.Command, "msgvault ")))
				requirements.NoError(err)
				assertions.False(isHelpOnlyGroup(command), "%s", result.Command)
			}
		}
	}
	results := clisearch.Search(rootCmd, "show daemon status", 5, cliSearchOptions())
	requirements.NotEmpty(results)
	assertions.Equal("msgvault daemon status", results[0].Command)
	results = clisearch.Search(rootCmd, "optional features", 20, cliSearchOptions())
	assertions.True(slices.ContainsFunc(results, func(result clisearch.Result) bool { return result.Command == "msgvault setup status" }))
}

type cliSearchFailWriter struct {
	remaining int
}

func (w *cliSearchFailWriter) Write(p []byte) (int, error) {
	n := min(w.remaining, len(p))
	w.remaining -= n
	if n < len(p) {
		return n, io.ErrClosedPipe
	}
	return n, nil
}

func TestCLISearchOutputErrors(t *testing.T) {
	skipHegelWithoutEmbeddedRuntime(t)
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		asJSON := hegel.Draw(ht, hegel.Booleans())
		query := "zzzxqvzzzxqv " + hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			query += " message"
		}
		args := []string{"cli", "search"}
		if asJSON {
			args = append(args, "--json")
		}
		args = append(args, "--", query)
		complete, diagnostic, err := executeCLIDiscovery(ht, args...)
		require.NoError(ht, err, "%s", diagnostic)
		require.NotEmpty(ht, complete)
		cutoff := hegel.Draw(ht, hegel.Integers(0, len(complete)-1))
		rootCmd.SetOut(&cliSearchFailWriter{remaining: cutoff})
		var stderr bytes.Buffer
		rootCmd.SetErr(&stderr)
		rootCmd.SetArgs(args)
		require.ErrorIs(ht, executeRootContext(t.Context(), rootCmd), io.ErrClosedPipe)
		assert.NotContains(ht, stderr.String(), "Usage:")
	})
}

func TestCLISearchHelpLayoutStable(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	// Layout mutates the global registry once, so compare the frozen task corpus
	// on either side rather than reusing an already-mutated tree in a property.
	before := make([][]clisearch.Result, len(cliSearchTasks))
	for i, task := range cliSearchTasks {
		before[i] = clisearch.Search(rootCmd, task.query, 20, cliSearchOptions())
	}
	ensureHelpLayout(rootCmd)
	for i, task := range cliSearchTasks {
		assert.Equal(t, before[i], clisearch.Search(rootCmd, task.query, 20, cliSearchOptions()), "%s", task.query)
	}
}

func TestCLISearchResultEligibility(t *testing.T) {
	skipHegelWithoutEmbeddedRuntime(t)
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		task := cliSearchTasks[hegel.Draw(ht, hegel.Integers(0, len(cliSearchTasks)-1))]
		query := task.query + " " + hegel.Draw(ht, hegel.Text())
		limit := hegel.Draw(ht, hegel.Integers(1, 20))
		results := clisearch.Search(rootCmd, query, limit, cliSearchOptions())
		require.NotEmpty(ht, results)
		for _, result := range results {
			assert.NotContains(ht, cliSearchExcluded, result.Command)
			command, _, err := rootCmd.Find(strings.Fields(strings.TrimPrefix(result.Command, "msgvault ")))
			require.NoError(ht, err)
			assert.False(ht, isHelpOnlyGroup(command))
		}
	})
}

func TestCLISearchDoesNotBypassMessageSearchConfig(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	config := filepath.Join(t.TempDir(), "invalid.toml")
	require.NoError(t, os.WriteFile(config, []byte("invalid = ["), 0o600))
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--config", config, "--home", t.TempDir(), "search", "synthetic", "--json"})
	require.ErrorContains(t, executeRootContext(t.Context(), rootCmd), "load config")
}
