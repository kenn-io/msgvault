package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/skills"
)

// skillInvocation is one `msgvault …` command line found in a skill.
type skillInvocation struct {
	Raw    string   // the matched command text, for error messages
	Tokens []string // whitespace-split tokens after "msgvault"
}

var inlineMsgvaultRe = regexp.MustCompile("`(msgvault [^`]+)`")

// extractSkillInvocations finds msgvault command lines in fenced code
// blocks (splitting pipelines on |) and inline code spans.
func extractSkillInvocations(content string) []skillInvocation {
	var out []skillInvocation
	add := func(text string) {
		text = strings.TrimSpace(text)
		if !strings.HasPrefix(text, "msgvault ") {
			return
		}
		fields := strings.Fields(text)
		out = append(out, skillInvocation{Raw: text, Tokens: fields[1:]})
	}
	inFence := false
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			for segment := range strings.SplitSeq(line, "|") {
				add(segment)
			}
			continue
		}
		for _, m := range inlineMsgvaultRe.FindAllStringSubmatch(line, -1) {
			add(m[1])
		}
	}
	return out
}

// validateSkillInvocation resolves the invocation's subcommand path
// and flags against the real command tree.
func validateSkillInvocation(root *cobra.Command, inv skillInvocation) error {
	if len(inv.Tokens) == 0 {
		return fmt.Errorf("bare msgvault invocation: %q", inv.Raw)
	}
	cmd := root
	depth := 0
	for _, token := range inv.Tokens {
		next := findSubcommand(cmd, token)
		if next == nil {
			break
		}
		cmd = next
		depth++
	}
	if depth == 0 {
		return fmt.Errorf("unknown command %q in %q", inv.Tokens[0], inv.Raw)
	}
	for _, token := range inv.Tokens[depth:] {
		if err := validateFlagToken(cmd, token, inv.Raw); err != nil {
			return err
		}
	}
	return nil
}

func findSubcommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == name || sub.HasAlias(name) {
			return sub
		}
	}
	return nil
}

func validateFlagToken(cmd *cobra.Command, token, raw string) error {
	switch {
	case strings.HasPrefix(token, "--"):
		name, _, _ := strings.Cut(strings.TrimPrefix(token, "--"), "=")
		if cmd.Flags().Lookup(name) == nil &&
			cmd.InheritedFlags().Lookup(name) == nil {
			return fmt.Errorf("command %q has no flag --%s (in %q)",
				cmd.Name(), name, raw)
		}
	case strings.HasPrefix(token, "-") && len(token) == 2 &&
		token[1] >= 'a' && token[1] <= 'z':
		shorthand := token[1:]
		if cmd.Flags().ShorthandLookup(shorthand) == nil &&
			cmd.InheritedFlags().ShorthandLookup(shorthand) == nil {
			return fmt.Errorf("command %q has no shorthand -%s (in %q)",
				cmd.Name(), shorthand, raw)
		}
	}
	return nil
}

func TestExtractSkillInvocations(t *testing.T) {
	content := "prose with `msgvault stats --json` inline\n" +
		"```bash\n" +
		"msgvault search from:alice@example.com --json |\n" +
		"  jq -r '.[].id'\n" +
		"msgvault show-message 12345 --json | jq '.attachments'\n" +
		"```\n" +
		"msgvault outside-fence-not-code is ignored\n"
	got := extractSkillInvocations(content)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"stats", "--json"}, got[0].Tokens)
	assert.Equal(t, "search", got[1].Tokens[0])
	assert.Equal(t, "show-message", got[2].Tokens[0])
}

func TestValidateSkillInvocation_CatchesDrift(t *testing.T) {
	bad := skillInvocation{
		Raw:    "msgvault no-such-command --json",
		Tokens: []string{"no-such-command", "--json"},
	}
	require.Error(t, validateSkillInvocation(rootCmd, bad))

	badFlag := skillInvocation{
		Raw:    "msgvault stats --no-such-flag",
		Tokens: []string{"stats", "--no-such-flag"},
	}
	require.Error(t, validateSkillInvocation(rootCmd, badFlag))

	good := skillInvocation{
		Raw:    "msgvault search from:alice@example.com --json -n 20",
		Tokens: []string{"search", "from:alice@example.com", "--json", "-n", "20"},
	}
	require.NoError(t, validateSkillInvocation(rootCmd, good))
}

// TestSkillsMatchCLI is the drift guard: every msgvault invocation in
// every rendered skill must resolve against the real command tree.
func TestSkillsMatchCLI(t *testing.T) {
	rendered, err := skills.Render("test")
	require.NoError(t, err)
	require.NotEmpty(t, rendered)
	total := 0
	for _, sk := range rendered {
		invocations := extractSkillInvocations(sk.Content)
		assert.NotEmpty(t, invocations,
			"skill %s should contain msgvault examples", sk.Name)
		for _, inv := range invocations {
			//nolint:testifylint // guarded assert+continue: report every invocation, not just the first
			assert.NoError(t, validateSkillInvocation(rootCmd, inv),
				"skill %s", sk.Name)
			total++
		}
	}
	t.Logf("validated %d msgvault invocations across %d skills",
		total, len(rendered))
}
