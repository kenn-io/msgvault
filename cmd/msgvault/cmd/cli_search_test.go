package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	"msgvault add-teams", "msgvault add-plaud", "msgvault add-synctech-sms-drive",
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

func TestCLIDiscoveryHelp(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"--help"})
	requirements.NoError(executeRootContext(t.Context(), rootCmd))
	first, _, _ := strings.Cut(output.String(), "\n")
	assertions.Equal(`Agents: find the right command with: msgvault cli search "<what you want to do>"`, first)
	assertions.Contains(output.String(), "Start here:\n  msgvault cli search")
	command, rest, err := rootCmd.Find([]string{"cli", "search"})
	requirements.NoError(err)
	assertions.Empty(rest)
	assertions.Equal("msgvault cli search", command.CommandPath())
	assertions.True(command.Runnable())
}

func TestCLISearchContract(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MSGVAULT_HOME", filepath.Join(t.TempDir(), "untouched"))
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
		{"invalid limit", []string{"cli", "search", "message", "--limit", "many"}},
		{"missing limit", []string{"cli", "search", "message", "--limit"}},
		{"overflowing limit", []string{"cli", "search", "message", "--limit", "99999999999999999999999"}},
		{"invalid bool", []string{"cli", "search", "message", "--json=invalid"}},
		{"unknown flag", []string{"cli", "search", "message", "--not-a-flag"}},
		{"unknown child", []string{"cli", "not-a-command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			out, stderr, err := executeCLIDiscovery(t, tc.args...)
			requirements.Error(err)
			assertions.Contains(out+stderr, "Usage:")
			assertions.NotContains(out+stderr, "load config")
		})
	}
	for _, limit := range []string{"1", "5", "20"} {
		t.Run("limit "+limit, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			out, stderr, err := executeCLIDiscovery(t, "cli", "search", "message", "--limit", limit, "--json")
			requirements.NoError(err, "%s", stderr)
			var response clisearch.Response
			requirements.NoError(json.Unmarshal([]byte(out), &response))
			switch limit {
			case "1":
				assertions.Len(response.Results, 1)
			case "5":
				assertions.Len(response.Results, 5)
			case "20":
				assertions.Len(response.Results, 20)
			}
			assertions.Empty(stderr)
		})
	}
	for _, query := range []string{"zzzxqvzzzxqv", "!!!", "the"} {
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
				assertions.True(command.Runnable())
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

func TestCLISearchEvalBaseline(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	// This subprocess measures the untuned live tree without domain keywords.
	var clearKeywords func(*cobra.Command)
	clearKeywords = func(command *cobra.Command) {
		clisearch.SetKeywords(command)
		for _, child := range command.Commands() {
			clearKeywords(child)
		}
	}
	clearKeywords(rootCmd)
	var first, third int
	for _, task := range cliSearchTasks {
		results := clisearch.Search(rootCmd, task.query, 5, clisearch.Options{Skip: skipCLICommand})
		for rank, result := range results {
			if result.Command == task.command {
				if rank == 0 {
					first++
				}
				if rank < 3 {
					third++
				}
			}
		}
		t.Logf("%q: %+v", task.query, results)
	}
	t.Logf("baseline hit@1=%d/%d hit@3=%d/%d", first, len(cliSearchTasks), third, len(cliSearchTasks))
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

func TestCLISearchDeterministic(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		limit := hegel.Draw(ht, hegel.Integers(1, 20))
		want := clisearch.Search(rootCmd, query, limit, cliSearchOptions())
		assert.Equal(ht, want, clisearch.Search(rootCmd, query, limit, cliSearchOptions()))
	})
}

func TestCLISearchQueryEcho(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	hegel.Test(t, func(ht *hegel.T) {
		words := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		// Bound the materialized argv, retaining the full character domain.
		words = words[:min(len(words), 8)]
		words = append(words, "message")
		args := append([]string{"cli", "search", "--json", "--"}, words...)
		out, stderr, err := executeCLIDiscovery(ht, args...)
		require.NoError(ht, err, "%s", stderr)
		var response clisearch.Response
		require.NoError(ht, json.Unmarshal([]byte(out), &response))
		assert.Equal(ht, strings.Join(words, " "), response.Query)
	})
}

func TestCLISearchJSONStructure(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	hegel.Test(t, func(ht *hegel.T) {
		query := "message " + hegel.Draw(ht, hegel.Text())
		out, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--json", "--", query)
		require.NoError(ht, err, "%s", stderr)
		var response map[string]any
		require.NoError(ht, json.Unmarshal([]byte(out), &response))
		require.Len(ht, response, 2)
		assert.IsType(ht, "", response["query"])
		results, ok := response["results"].([]any)
		require.True(ht, ok, "%+v", response)
		require.NotEmpty(ht, results)
		for _, result := range results {
			fields, ok := result.(map[string]any)
			require.True(ht, ok)
			require.Len(ht, fields, 3)
			for _, key := range []string{"command", "summary", "usage"} {
				assert.IsType(ht, "", fields[key])
			}
		}
	})
}

func TestCLISearchLimitPreservesPrefix(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			query += " " + cliSearchTasks[hegel.Draw(ht, hegel.Integers(0, len(cliSearchTasks)-1))].query
		}
		first := hegel.Draw(ht, hegel.Integers(1, 20))
		second := hegel.Draw(ht, hegel.Integers(1, 20))
		longer := clisearch.Search(rootCmd, query, max(first, second), cliSearchOptions())
		shorter := clisearch.Search(rootCmd, query, min(first, second), cliSearchOptions())
		assert.Equal(ht, longer[:min(len(longer), first, second)], shorter)
	})
}

func TestCLISearchDefaultLimit(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	hegel.Test(t, func(ht *hegel.T) {
		query := "message " + hegel.Draw(ht, hegel.Text())
		want, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--json", "--limit", "5", "--", query)
		require.NoError(ht, err, "%s", stderr)
		got, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--json", "--", query)
		require.NoError(ht, err, "%s", stderr)
		assert.Equal(ht, want, got)
	})
}

func TestCLISearchBlankQueriesRejected(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	// These are all Unicode White_Space code points accepted by TrimSpace.
	whitespace := strings.Split("\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000", "")
	hegel.Test(t, func(ht *hegel.T) {
		query := strings.Join(hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(whitespace))), "")
		out, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--", query)
		require.Error(ht, err)
		assert.Contains(ht, out+stderr, "Usage:")
	})
}

func TestCLISearchInvalidLimitsRejected(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	hegel.Test(t, func(ht *hegel.T) {
		limit := hegel.Draw(ht, hegel.OneOf(hegel.Integers(math.MinInt, 0), hegel.Integers(21, math.MaxInt)))
		out, stderr, err := executeCLIDiscovery(ht, "cli", "search", "message", "--limit", strconv.Itoa(limit))
		require.Error(ht, err)
		assert.Contains(ht, out+stderr, "Usage:")
	})
}

func TestCLISearchLeavesHomeUnchanged(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	home := t.TempDir()
	config := filepath.Join(t.TempDir(), "invalid.toml")
	require.NoError(t, os.WriteFile(config, []byte("invalid = ["), 0o600))
	hegel.Test(t, func(ht *hegel.T) {
		// Initialize Hegel's native-library cache before observing the CLI home.
		// Setting HOME before hegel.Test would count the harness's own writes.
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		t.Setenv("XDG_DATA_HOME", home)
		t.Setenv("MSGVAULT_HOME", filepath.Join(home, "archive"))
		query := hegel.Draw(ht, hegel.Text())
		limit := hegel.Draw(ht, hegel.OneOf(hegel.Integers(1, 20), hegel.Integers(math.MinInt, math.MaxInt)))
		// Both successful responses and usage errors must be side-effect free.
		_, _, _ = executeCLIDiscovery(ht, "--config", config, "--log-level", "invalid", "cli", "search",
			"--json", "--limit", strconv.Itoa(limit), "--", query)
		files, err := os.ReadDir(home)
		require.NoError(ht, err)
		assert.Empty(ht, files)
	})
}

func TestCLISearchTextAgreesWithJSON(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	hegel.Test(t, func(ht *hegel.T) {
		query := "zzzxqvzzzxqv " + hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			query += " " + cliSearchTasks[hegel.Draw(ht, hegel.Integers(0, len(cliSearchTasks)-1))].query
		}
		limit := strconv.Itoa(hegel.Draw(ht, hegel.Integers(1, 20)))
		encoded, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--json", "--limit", limit, "--", query)
		require.NoError(ht, err, "%s", stderr)
		var response clisearch.Response
		require.NoError(ht, json.Unmarshal([]byte(encoded), &response))
		text, stderr, err := executeCLIDiscovery(ht, "cli", "search", "--limit", limit, "--", query)
		require.NoError(ht, err, "%s", stderr)
		var want strings.Builder
		hint := "msgvault"
		for i, result := range response.Results {
			fmt.Fprintf(&want, "%s  %s\n", result.Command, result.Summary)
			if i == 0 {
				hint = result.Command
			}
		}
		fmt.Fprintf(&want, "Run '%s --help' for flags and examples.\n", hint)
		assert.Equal(ht, want.String(), text)
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

func TestCLISearchResultsBounded(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		task := cliSearchTasks[hegel.Draw(ht, hegel.Integers(0, len(cliSearchTasks)-1))]
		query := task.query + " " + hegel.Draw(ht, hegel.Text())
		limit := hegel.Draw(ht, hegel.Integers(1, 20))
		assert.LessOrEqual(ht, len(clisearch.Search(rootCmd, query, limit, cliSearchOptions())), limit)
	})
}

func TestCLISearchResultEligibility(t *testing.T) {
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
			command, rest, err := rootCmd.Find(strings.Fields(strings.TrimPrefix(result.Command, "msgvault ")))
			require.NoError(ht, err)
			assert.Empty(ht, rest)
			assert.Equal(ht, result.Command, command.CommandPath())
			assert.True(ht, command.Runnable())
			assert.False(ht, isHelpOnlyGroup(command))
			for ancestor := command; ancestor != nil; ancestor = ancestor.Parent() {
				assert.False(ht, ancestor.Hidden)
				assert.Empty(ht, ancestor.Deprecated)
			}
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
