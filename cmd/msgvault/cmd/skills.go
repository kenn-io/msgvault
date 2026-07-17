package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/skills"
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage agent skills for coding assistants",
	Long: `Manage msgvault agent skills (SKILL.md files, per the open
agent-skills standard) that teach coding agents such as Claude Code
and Codex the msgvault search, attachment, and analytics workflows.`,
}

var skillsInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install msgvault agent skills",
	Long: `Install msgvault agent skills into detected agent skill
directories (~/.claude/skills and ~/.codex/skills), or into an
explicit directory with --dir.

Previously installed skills are updated in place. Files without the
msgvault generation marker (e.g. hand-edited copies) are skipped
unless --force is given.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		agents, err := cmd.Flags().GetStringSlice("agent")
		if err != nil {
			return fmt.Errorf("read --agent flag: %w", err)
		}
		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return fmt.Errorf("read --dir flag: %w", err)
		}
		force, err := cmd.Flags().GetBool("force")
		if err != nil {
			return fmt.Errorf("read --force flag: %w", err)
		}
		return runSkillsInstall(cmd.OutOrStdout(), agents, dir, force)
	},
}

var skillsUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove installed msgvault agent skills",
	Long: `Remove msgvault-* skill directories previously written by
'msgvault skills install'. Skill files without the msgvault generation
marker are left untouched.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		agents, err := cmd.Flags().GetStringSlice("agent")
		if err != nil {
			return fmt.Errorf("read --agent flag: %w", err)
		}
		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return fmt.Errorf("read --dir flag: %w", err)
		}
		return runSkillsUninstall(cmd.OutOrStdout(), agents, dir)
	},
}

// resolveSkillsRoots picks the target skills roots: an explicit --dir,
// or the detected agent directories under the user's home.
func resolveSkillsRoots(agents []string, dir string) ([]skills.AgentDir, error) {
	if dir != "" {
		return []skills.AgentDir{{Agent: "custom", Dir: dir}}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	roots, err := skills.DetectAgents(home, agents)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, errors.New(
			"no supported agents detected (looked for ~/.claude and ~/.codex); " +
				"use --dir to choose a skills directory")
	}
	return roots, nil
}

func runSkillsInstall(out io.Writer, agents []string, dir string, force bool) error {
	roots, err := resolveSkillsRoots(agents, dir)
	if err != nil {
		return err
	}
	rendered, err := skills.Render(Version)
	if err != nil {
		return err
	}
	for _, root := range roots {
		results, err := skills.Install(root.Dir, rendered, force)
		if err != nil {
			return fmt.Errorf("install skills for %s: %w", root.Agent, err)
		}
		for _, r := range results {
			if r.Status == skills.StatusSkipped {
				_, _ = fmt.Fprintf(out,
					"%s: skipped %s (no msgvault marker; hand-edited? use --force to overwrite)\n",
					root.Agent, r.Path)
				continue
			}
			_, _ = fmt.Fprintf(out, "%s: %s %s\n", root.Agent, r.Status, r.Path)
		}
	}
	return nil
}

func runSkillsUninstall(out io.Writer, agents []string, dir string) error {
	roots, err := resolveSkillsRoots(agents, dir)
	if err != nil {
		return err
	}
	for _, root := range roots {
		removed, err := skills.Uninstall(root.Dir)
		if err != nil {
			return fmt.Errorf("uninstall skills for %s: %w", root.Agent, err)
		}
		if len(removed) == 0 {
			_, _ = fmt.Fprintf(out, "%s: no msgvault skills found\n", root.Agent)
			continue
		}
		for _, path := range removed {
			_, _ = fmt.Fprintf(out, "%s: removed %s\n", root.Agent, path)
		}
	}
	return nil
}

func init() {
	skillsInstallCmd.Flags().StringSlice(
		"agent", nil, "restrict to specific agents (claude, codex)")
	skillsInstallCmd.Flags().String(
		"dir", "", "install into this skills directory instead of detected agents")
	skillsInstallCmd.Flags().Bool(
		"force", false, "overwrite skill files that lack the msgvault marker")
	skillsInstallCmd.MarkFlagsMutuallyExclusive("agent", "dir")

	skillsUninstallCmd.Flags().StringSlice(
		"agent", nil, "restrict to specific agents (claude, codex)")
	skillsUninstallCmd.Flags().String(
		"dir", "", "uninstall from this skills directory instead of detected agents")
	skillsUninstallCmd.MarkFlagsMutuallyExclusive("agent", "dir")

	skillsCmd.AddCommand(skillsInstallCmd)
	skillsCmd.AddCommand(skillsUninstallCmd)
	rootCmd.AddCommand(skillsCmd)
}
