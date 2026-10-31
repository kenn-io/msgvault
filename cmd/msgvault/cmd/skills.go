package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/skills"
)

var skillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage agent skills for coding assistants",
	Long: `Manage msgvault agent skills (SKILL.md files, per the open
agent-skills standard) that teach coding agents such as Claude Code
and Codex the msgvault search, timeline, attachment, and analytics workflows.`,
}

var skillsInstallCmd = newSkillsInstallCommand()

func newSkillsInstallCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "install",
		Short: "Install msgvault agent skills",
		Long: `Install msgvault agent skills into detected agent skill
directories (~/.claude/skills and ~/.codex/skills), or into an
explicit directory with --dir.

Previously installed skills are updated in place. Files without the
msgvault generation marker (e.g. hand-edited copies) are skipped
unless --force is given.

--briefing also adds the opt-in command 'msgvault timeline --brief' as a
SessionStart hook for the selected agents. Claude uses ~/.claude/settings.json;
Codex uses ~/.codex/hooks.json. With --dir, both targets are under that
directory (.claude/settings.json and .codex/hooks.json). Skills still go
directly into the supplied skills directory. Existing settings are preserved.
Review and trust a new Codex hook through /hooks before it can run.`,
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
			briefing, err := cmd.Flags().GetBool("briefing")
			if err != nil {
				return fmt.Errorf("read --briefing flag: %w", err)
			}
			return runSkillsInstallWithBriefing(cmd.OutOrStdout(), agents, dir, force, briefing)
		},
	}
	command.Flags().StringSlice("agent", nil, "restrict to specific agents (claude, codex)")
	command.Flags().String("dir", "", "install into this skills directory instead of detected agents")
	command.Flags().Bool("force", false, "overwrite skill files that lack the msgvault marker")
	command.Flags().Bool("briefing", false, "add an opt-in timeline briefing at session start")
	command.MarkFlagsMutuallyExclusive("agent", "dir")
	return command
}

var skillsUninstallCmd = newSkillsUninstallCommand()

func newSkillsUninstallCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove installed msgvault agent skills",
		Long: `Remove msgvault-* skill directories previously written by
'msgvault skills install'. Skill files without the msgvault generation
marker are left untouched. Also remove exact 'msgvault timeline --brief'
SessionStart handlers from the selected agents' settings, preserving other
settings and hooks. With --dir, use that directory's .claude/settings.json
and .codex/hooks.json.`,
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
	command.Flags().StringSlice("agent", nil, "restrict to specific agents (claude, codex)")
	command.Flags().String("dir", "", "uninstall from this skills directory instead of detected agents")
	command.MarkFlagsMutuallyExclusive("agent", "dir")
	return command
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
	return runSkillsInstallWithBriefing(out, agents, dir, force, false)
}

func runSkillsInstallWithBriefing(out io.Writer, agents []string, dir string, force, briefing bool) error {
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
	if briefing {
		return updateSkillsBriefing(out, roots, dir, true)
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
	return updateSkillsBriefing(out, roots, dir, false)
}

func updateSkillsBriefing(out io.Writer, roots []skills.AgentDir, dir string, install bool) error {
	type target struct{ agent, path string }
	var targets []target
	if dir != "" {
		targets = []target{
			{"claude", filepath.Join(dir, ".claude", "settings.json")},
			{"codex", filepath.Join(dir, ".codex", "hooks.json")},
		}
	} else {
		for _, root := range roots {
			name := "settings.json"
			if root.Agent == "codex" {
				name = "hooks.json"
			}
			targets = append(targets, target{root.Agent, filepath.Join(filepath.Dir(root.Dir), name)})
		}
	}
	for _, target := range targets {
		if install {
			if err := skills.InstallBriefing(target.path); err != nil {
				return fmt.Errorf("install briefing for %s: %w", target.agent, err)
			}
			_, _ = fmt.Fprintf(out, "%s: session briefing configured in %s\n", target.agent, target.path)
			if target.agent == "codex" {
				_, _ = fmt.Fprintln(out, "codex: review and trust the briefing hook with /hooks before it runs")
			}
		} else if err := skills.UninstallBriefing(target.path); err != nil {
			return fmt.Errorf("uninstall briefing for %s: %w", target.agent, err)
		}
	}
	return nil
}

func init() {
	skillsCmd.AddCommand(skillsInstallCmd)
	skillsCmd.AddCommand(skillsUninstallCmd)
	rootCmd.AddCommand(skillsCmd)
}
