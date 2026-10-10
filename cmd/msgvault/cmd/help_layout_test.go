package cmd

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "Update CLI help golden files")

// Help mutates Cobra's registered tree. Isolate the real registry so tests
// that attach global commands to their own roots do not inherit our groups,
// wrappers, parent pointers, parsed help flags, or output writers.
func helpTestSubprocess(t *testing.T) bool {
	t.Helper()
	requirements := require.New(t)
	if os.Getenv("MSGVAULT_HELP_TEST") == t.Name() {
		return false
	}
	executable, err := os.Executable()
	requirements.NoError(err)
	args := []string{"-test.run=^" + regexp.QuoteMeta(t.Name()) + "$", "-test.count=1"}
	if *updateGolden {
		args = append(args, "-update")
	}
	command := exec.CommandContext(t.Context(), executable, args...)
	command.Env = append(os.Environ(), "MSGVAULT_HELP_TEST="+t.Name(), "MSGVAULT_HOME="+t.TempDir())
	output, err := command.CombinedOutput()
	requirements.NoError(err, "%s", output)
	return true
}

var tierOneHelpPaths = []string{
	"", "list-accounts", "search", "show-message", "stats", "export-eml",
	"export-attachments", "export-attachment", "export-messages", "list-senders",
	"list-domains", "list-labels", "query", "person", "person list", "person get",
	"person identities", "person search", "person promote", "daemon", "daemon status",
	"sync", "meetings", "meetings context", "setup", "setup status", "calendar",
	"calendar create", "calendar update", "calendar delete", "calendar move",
	"calendar respond", "calendar freebusy", "calendar conflicts", "quickstart",
}

// PR #1127 owns this copy. Remove the show-message/quickstart exceptions
// when it merges; search examples remain owned by the search help change.
func deferredHelpCopy(path string) bool {
	return path == "search" || path == "show-message" || path == "quickstart"
}

func walkVisibleHelp(command *cobra.Command, visit func(*cobra.Command)) {
	if command.Hidden || command.Deprecated != "" || command.Name() == "help" ||
		command.Name() == "completion" || command.Name() == embeddingsOptimizeWorkerName {
		return
	}
	visit(command)
	for _, child := range command.Commands() {
		walkVisibleHelp(child, visit)
	}
}

func helpPath(command *cobra.Command) string {
	return strings.TrimSpace(strings.TrimPrefix(command.CommandPath(), "msgvault"))
}

func TestHelpTreeLint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	ensureHelpLayout(rootCmd) // Repeated enrollment must not duplicate groups or wrap handlers.
	assertions.Len(rootCmd.Groups(), 10)
	completion, _, err := rootCmd.Find([]string{"completion"})
	requirements.NoError(err)
	assertions.Equal("ops", completion.GroupID, "app-provided completion must be grouped too")
	debt, err := os.ReadFile("testdata/help_example_gaps.txt")
	requirements.NoError(err)
	gaps := make(map[string]bool)
	paths := []string{}
	debtText := strings.ReplaceAll(string(debt), "\r\n", "\n")
	for line := range strings.SplitSeq(strings.TrimSpace(debtText), "\n") {
		if line == "" {
			continue
		}
		requirements.False(gaps[line], "duplicate example gap: %s", line)
		gaps[line] = true
		paths = append(paths, line)
	}
	assertions.True(slices.IsSorted(paths), "example gaps must be sorted")
	groups := make(map[string]bool)
	for _, group := range rootCmd.Groups() {
		groups[group.ID] = true
	}
	seenGaps := make(map[string]bool)
	seenTierOne := make(map[string]bool)
	walkVisibleHelp(rootCmd, func(command *cobra.Command) {
		path := helpPath(command)
		t.Run(command.CommandPath(), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			requirements.NotEmpty(command.Short)
			first, _ := utf8.DecodeRuneInString(command.Short)
			assertions.True(unicode.IsUpper(first), "Short must start uppercase")
			assertions.False(strings.HasSuffix(command.Short, "."), "Short must not end with a period")
			limit := 80
			if command.Parent() == rootCmd {
				limit = 60
				assertions.NotEmpty(command.GroupID, "top-level command must choose a group")
				assertions.True(groups[command.GroupID], "group %q must be registered", command.GroupID)
			}
			assertions.LessOrEqual(utf8.RuneCountInString(command.Short), limit)
			if parent := command.Parent(); parent != nil {
				name := parent.Name()
				assertions.NotEqual(strings.ToUpper(name[:1])+name[1:]+" "+command.Name(), command.Short,
					"Short must describe the action")
			}
			// The pre-existing show-message tab belongs to PR #1127.
			if path != "show-message" {
				for _, text := range []string{command.Long, command.Example} {
					for line := range strings.SplitSeq(text, "\n") {
						assertions.False(strings.HasPrefix(line, "\t"), "help lines use spaces")
						assertions.False(strings.HasPrefix(line, " msgvault"), "examples use two spaces")
					}
				}
			}
			for line := range strings.SplitSeq(command.Example, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					assertions.True(strings.HasPrefix(line, "msgvault ") ||
						strings.HasPrefix(line, "MSGVAULT_") || strings.HasPrefix(line, "#"), "invalid example: %s", line)
				}
			}
			tierOne := slices.Contains(tierOneHelpPaths, path)
			if tierOne {
				seenTierOne[path] = true
				assertions.NotEmpty(command.Long, "Tier-1 help needs context")
				// Root's start-here block is intentionally in Long.
				if path != "" && !deferredHelpCopy(path) {
					assertions.NotEmpty(command.Example, "Tier-1 help needs an example")
				}
			}
			if gaps[path] {
				seenGaps[path] = true
				assertions.Empty(command.Example, "remove %s from help_example_gaps.txt", path)
				assertions.False(tierOne, "Tier-1 commands cannot be example debt")
				assertions.True(command.Runnable() && !isHelpOnlyGroup(command), "only runnable commands are example debt")
			} else if command.Runnable() && !isHelpOnlyGroup(command) && !tierOne {
				assertions.NotEmpty(command.Example, "add an example (existing debt is recorded in help_example_gaps.txt)")
			}
		})
	})
	for _, path := range tierOneHelpPaths {
		assertions.True(seenTierOne[path], "missing Tier-1 command: %s", path)
	}
	for path := range gaps {
		assertions.True(seenGaps[path], "remove unavailable command %s from help_example_gaps.txt", path)
	}
	accounts, _, err := rootCmd.Find([]string{"list-accounts"})
	requirements.NoError(err)
	assertions.Contains(accounts.Long, "--source <type>:<email>")
}

func TestGroupCommandRejectsUnknownSubcommand(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	groupPaths := [][]string{}
	walkVisibleHelp(rootCmd, func(command *cobra.Command) {
		if command != rootCmd && command.HasSubCommands() &&
			(!command.Runnable() || isHelpOnlyGroup(command)) {
			groupPaths = append(groupPaths, strings.Fields(helpPath(command)))
		}
	})
	requirements.NotEmpty(groupPaths)
	for _, path := range groupPaths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			for _, unknown := range []bool{true, false} {
				home := filepath.Join(t.TempDir(), "untouched")
				var output bytes.Buffer
				rootCmd.SetOut(&output)
				rootCmd.SetErr(&output)
				args := append([]string{"--config", filepath.Join(t.TempDir(), "invalid.toml"), "--home", home}, path...)
				requirements.NoError(os.WriteFile(args[1], []byte("invalid = ["), 0o600))
				if unknown {
					args = append(args, "bogus-subcmd")
				}
				rootCmd.SetArgs(args)
				err := executeRootContext(t.Context(), rootCmd)
				if unknown {
					requirements.ErrorContains(err, `unknown command "bogus-subcmd"`)
					assertions.Contains(output.String(), "Usage:")
				} else {
					requirements.NoError(err)
					assertions.Contains(output.String(), "Usage:")
					assertions.NotContains(output.String(), "[flags]\n  "+"msgvault "+strings.Join(path, " ")+" [command]")
				}
				_, err = os.Stat(home)
				assertions.ErrorIs(err, os.ErrNotExist, "group help must not create the archive")
			}
		})
	}
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"person", "proomte"})
	requirements.ErrorContains(executeRootContext(t.Context(), rootCmd), "Did you mean this?")
	assertions.Contains(output.String(), "promote")
}

// Group help reads nothing, so agent-delegated mode prints it like owner mode
// while still refusing the group's commands that agents may not run.
func TestAgentDelegatedGroupHelpBypassesCapabilityGate(t *testing.T) {
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	tokenFile := filepath.Join(t.TempDir(), "agent-token")
	requirements.NoError(os.WriteFile(tokenFile, []byte("synthetic-token\n"), 0o600))
	for _, test := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "bare group", args: []string{"person"}},
		{name: "unknown subcommand", args: []string{"person", "proomte"}, wantErr: `unknown command "proomte"`},
		{name: "gated subcommand", args: []string{"person", "list"}, wantErr: "list is not available in agent-delegated mode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var output bytes.Buffer
			rootCmd.SetOut(&output)
			rootCmd.SetErr(&output)
			rootCmd.SetArgs(append([]string{"--agent-url", "http://127.0.0.1:9", "--agent-token-file", tokenFile}, test.args...))
			err := executeRootContext(t.Context(), rootCmd)
			if test.wantErr == "" {
				requirements.NoError(err)
				assertions.Contains(output.String(), "msgvault person [command]")
			} else {
				requirements.ErrorContains(err, test.wantErr)
			}
		})
	}
}

func TestRootHelpGolden(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"--help"})
	requirements.NoError(executeRootContext(t.Context(), rootCmd))
	file := "testdata/help/root.golden"
	if *updateGolden {
		requirements.NoError(os.MkdirAll(filepath.Dir(file), 0o755))
		requirements.NoError(os.WriteFile(file, output.Bytes(), 0o644))
	}
	want, err := os.ReadFile(file)
	requirements.NoError(err)
	expected := strings.ReplaceAll(string(want), "\r\n", "\n")
	actual := strings.ReplaceAll(output.String(), "\r\n", "\n")
	assertions.Equal(expected, actual)
	assertions.LessOrEqual(output.Len(), 11500, "shorten start-here comments if root exceeds its help budget")
	assertions.Contains(output.String(), "Start here:")
	_, startHere, found := strings.Cut(rootCmd.Long, "Start here:\n")
	requirements.True(found)
	startHere, _, _ = strings.Cut(startHere, "\nOutput and exit status:")
	commands := 0
	for line := range strings.SplitSeq(startHere, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "msgvault ") {
			commands++
		}
	}
	assertions.LessOrEqual(commands, 11, "start-here must stay a short route through common tasks")
	assertions.NotContains(output.String(), "/home/")
	assertions.NotContains(output.String(), "/Users/")
}

func TestSubcommandHelpHasCompactGlobalFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"list-accounts", "--help"})
	requirements.NoError(executeRootContext(t.Context(), rootCmd))
	assertions.Contains(output.String(), "Global Flags: --")
	assertions.NotContains(output.String(), "--log-sql-slow-ms int")
	assertions.Contains(output.String(), "see 'msgvault --help'")
	assertions.NotContains(output.String(), "Inherited Flags:")
}

// Root help documents only root flags, so flags a parent group defines, such
// as calendar's required --account, must keep their descriptions.
func TestSubcommandHelpDescribesParentGroupFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"calendar", "create", "--help"})
	requirements.NoError(executeRootContext(t.Context(), rootCmd))
	inherited, global, found := strings.Cut(output.String(), "Global Flags: ")
	requirements.True(found, "%s", output.String())
	_, inherited, found = strings.Cut(inherited, "Inherited Flags:\n")
	requirements.True(found, "%s", output.String())
	assertions.Contains(inherited, "--account string")
	assertions.Contains(inherited, "configured calendar source name or OAuth account (required)")
	assertions.Contains(inherited, "--json")
	global, _, _ = strings.Cut(global, "\n")
	assertions.NotContains(global, "--account")
	assertions.NotContains(global, "--json")
	assertions.Contains(global, "-v/--verbose")
}

func TestInheritedHelpFlagsPreserveVisibilityAndOverrides(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	root := &cobra.Command{Use: "msgvault"}
	root.PersistentFlags().BoolP("verbose", "v", false, "logging")
	root.PersistentFlags().Bool("hidden", false, "hidden")
	requirements.NoError(root.PersistentFlags().MarkHidden("hidden"))
	root.PersistentFlags().String("scope", "root", "root scope")
	root.PersistentFlags().String("config", "", "config file")
	group := &cobra.Command{Use: "group"}
	group.PersistentFlags().Bool("json", false, "structured output")
	group.PersistentFlags().Bool("group-hidden", false, "hidden group flag")
	requirements.NoError(group.PersistentFlags().MarkHidden("group-hidden"))
	group.PersistentFlags().Bool("verbose", false, "group verbosity")
	child := &cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}}
	child.Flags().String("scope", "local", "local override")
	root.AddCommand(group)
	group.AddCommand(child)
	child.SetUsageTemplate(compactHelpUsageTemplate)
	// This test can run alone; template functions are normally enrolled at root execution.
	registerHelpTemplateFunctions()
	var output bytes.Buffer
	child.SetOut(&output)
	child.SetErr(&output)
	requirements.NoError(child.Help())
	assertions.Contains(output.String(), "--scope string")
	assertions.Contains(output.String(), "Inherited Flags:\n      --json      structured output\n      --verbose   group verbosity\n")
	assertions.Contains(output.String(), "Global Flags: --config (see 'msgvault --help')")
	assertions.NotContains(output.String(), "hidden")
}
