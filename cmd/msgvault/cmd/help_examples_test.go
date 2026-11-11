package cmd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every parse gets a fresh production registry: cloned definitions retain
// pflag.Value objects, including custom/slice state.
func TestAdvertisedExamplesParse(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	type advertised struct{ source, line string }
	var examples []advertised
	counts := map[string]int{}
	collect := func(source, text string, prose bool) {
		for _, line := range advertisedCommandLines(text, prose) {
			examples = append(examples, advertised{source, line})
			counts[source]++
		}
	}
	walkVisibleHelp(rootCmd, func(command *cobra.Command) {
		collect(command.CommandPath()+" examples", command.Example, false)
		collect(command.CommandPath()+" help", command.Long, true)
	})
	collect("quickstart", quickstartText, true)
	require := require.New(t)
	require.Greater(len(examples), 300, "the production command inventory must be checked")
	require.Positive(counts["quickstart"])
	require.Positive(counts["msgvault help"])
	executable, err := os.Executable()
	require.NoError(err)
	// Identical argv advertised in multiple places needs one independent parse.
	// Keep the full source inventory above, then deduplicate only exact tokens.
	seen := make(map[string]bool)
	for _, example := range examples {
		words, err := splitAdvertisedWords(example.line)
		require.NoError(err)
		key := strings.Join(words, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		t.Run(example.source+"/"+example.line, func(t *testing.T) {
			process := exec.CommandContext(t.Context(), executable, "-test.run=^TestAdvertisedExampleProcess$", "-test.count=1")
			process.Env = append(os.Environ(), "MSGVAULT_PARSE_EXAMPLE="+base64.StdEncoding.EncodeToString([]byte(example.line)))
			output, err := process.CombinedOutput()
			assert.NoError(t, err, "%s: %s\n%s", example.source, example.line, output)
		})
	}
}

func TestAdvertisedExampleProcess(t *testing.T) {
	encoded := os.Getenv("MSGVAULT_PARSE_EXAMPLE")
	if encoded == "" {
		return
	}
	line, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	ensureHelpLayout(rootCmd)
	require.NoError(t, validateAdvertisedExample(rootCmd, string(line)))
}

func TestAdvertisedExampleRejectsStale(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	// These fail before mutating any flag value. Each full advertised example
	// above is isolated, including required-flag and group validation.
	for _, line := range []string{
		"msgvault show-message 1", "msgvault search --bogus words", "msgvault export eml", "msgvault account typo", `msgvault show "unfinished`,
	} {
		assert.Error(t, validateAdvertisedExample(rootCmd, line), line)
	}
}

func validateAdvertisedExample(root *cobra.Command, line string) error {
	args, err := splitAdvertisedWords(line)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] != "msgvault" {
		return errors.New("expected msgvault command")
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	command, rest, err := root.Find(args[1:])
	if err != nil {
		return fmt.Errorf("find %s: %w", line, err)
	}
	for parent := command; parent != nil; parent = parent.Parent() {
		if parent.Hidden || parent.Deprecated != "" {
			return fmt.Errorf("hidden or legacy command: %s", parent.CommandPath())
		}
	}
	command.InitDefaultHelpFlag()
	if err := command.ParseFlags(rest); err != nil {
		return fmt.Errorf("parse flags for %s: %w", line, err)
	}
	help, _ := command.Flags().GetBool("help")
	if help {
		return nil
	}
	if command == root || isHelpOnlyGroup(command) || !command.Runnable() {
		return fmt.Errorf("example needs an operation or --help: %s", command.CommandPath())
	}
	if err := command.ValidateArgs(command.Flags().Args()); err != nil {
		return fmt.Errorf("validate args for %s: %w", line, err)
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return fmt.Errorf("validate required flags for %s: %w", line, err)
	}
	if err := command.ValidateFlagGroups(); err != nil {
		return fmt.Errorf("validate flag groups for %s: %w", line, err)
	}
	return nil
}
