package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/msgvault/internal/skills"
)

var agentCmd = &cobra.Command{Use: "agent", Short: "Set up agents"}

func agentSetupItems(agent string) ([]agentsetup.Item, error) {
	server := agentmcp.Server{Command: "msgvault", Args: []string{"mcp"}}
	mcp := agentsetup.MCP(agent, "msgvault", server)
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	root, ok := skills.Root(home, agent)
	if !ok {
		return []agentsetup.Item{mcp}, nil
	}
	rendered, err := skills.Render(Version)
	if err != nil {
		return nil, err
	}
	item := agentsetup.Item{Name: "skills", Install: func(context.Context) error {
		_, err := skills.Install(root, rendered, false)
		return err
	}, Check: func(context.Context) (agentsetup.Status, error) {
		result, err := skills.Check(root, rendered)
		if err != nil {
			return agentsetup.Status{}, err
		}
		status := agentsetup.Status{State: agentsetup.StateOK}
		switch result.State {
		case skills.CheckCurrent:
			status.State = agentsetup.StateOK
		case skills.CheckModified:
			return agentsetup.Status{State: agentsetup.StateModified, Reason: result.Skill + " has no msgvault marker", NextStep: "Move " + filepath.Join(root, result.Skill, "SKILL.md") + " aside; then run msgvault agent setup --agent " + agent + " (overwrites other marked skills)"}, nil
		case skills.CheckMissing:
			status.State = agentsetup.StateMissing
		case skills.CheckOutdated:
			status.State = agentsetup.StateOutdated
		}
		return status, nil
	}}
	return []agentsetup.Item{item, mcp}, nil
}

func checkAgentSetup(ctx context.Context) (agentsetup.Report, error) {
	if !agentCLIAvailable() {
		return agentsetup.Report{}, ctx.Err()
	}
	return runAgentCheck(ctx)
}

func agentCLIAvailable() bool {
	for _, name := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

func runAgentCheck(ctx context.Context) (agentsetup.Report, error) {
	opts := agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems}
	opts.CheckOnly = true
	report, err := agentsetup.Run(ctx, opts)
	if err != nil {
		return report, fmt.Errorf("check agent setup: %w", err)
	}
	return report, nil
}

func init() {
	agentCmd.AddCommand(agentsetup.NewCommand(agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems}))
	rootCmd.AddCommand(agentCmd)
}
