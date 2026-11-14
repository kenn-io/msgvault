package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/msgvault/internal/skills"
)

var agentCmd = &cobra.Command{Use: "agent", Short: "Set up agents"}

func agentSetupItems(agent string) ([]agentsetup.Item, error) {
	opts := prepareInvocation(rootCmd).options
	envHome := os.Getenv("MSGVAULT_HOME")
	for _, path := range []*string{&opts.cfgFile, &opts.homeDir, &envHome} {
		if *path != "" && !strings.HasPrefix(*path, "~") {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return nil, fmt.Errorf("resolve path %s: %w", *path, err)
			}
			*path = absolute
		}
	}
	args := invocationGlobalArgs(invocationOptions{cfgFile: opts.cfgFile, homeDir: opts.homeDir})
	server := agentmcp.Server{Command: "msgvault", Args: append(args, "mcp")}
	if envHome != "" {
		server.Env = map[string]string{"MSGVAULT_HOME": envHome}
	}
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
		results, err := skills.Check(root, rendered)
		if err != nil {
			return agentsetup.Status{}, err
		}
		status := agentsetup.Status{State: agentsetup.StateOK}
		for _, result := range results {
			switch result.State {
			case skills.CheckModified:
				return agentsetup.Status{State: agentsetup.StateModified, Reason: result.Skill + " has no msgvault marker", NextStep: "msgvault skills install --force --agent " + agent}, nil
			case skills.CheckMissing:
				status.State = agentsetup.StateMissing
			case skills.CheckOutdated:
				if status.State == agentsetup.StateOK {
					status.State = agentsetup.StateOutdated
				}
			}
		}
		return status, nil
	}}
	return []agentsetup.Item{item, mcp}, nil
}

func checkAgentSetup(ctx context.Context) (agentsetup.Report, error) {
	return agentsetup.Run(ctx, agentsetup.Options{CheckOnly: true, SetupCommand: "msgvault agent setup", Items: agentSetupItems})
}

func init() {
	agentCmd.AddCommand(agentsetup.NewCommand(agentsetup.Options{SetupCommand: "msgvault agent setup", Items: agentSetupItems}))
	rootCmd.AddCommand(agentCmd)
}
