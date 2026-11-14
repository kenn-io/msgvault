package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/skills"
)

var agentCmd = &cobra.Command{Use: "agent", Short: "Set up agents"}

func archiveArgs() ([]string, error) {
	opts := prepareInvocation(rootCmd).options
	if opts.cfgFile == "" && opts.homeDir == "" {
		opts.homeDir = os.Getenv("MSGVAULT_HOME")
	}
	for _, path := range []*string{&opts.cfgFile, &opts.homeDir} {
		if *path != "" && *path != "~" && !strings.HasPrefix(*path, "~/") && !strings.HasPrefix(*path, "~"+string(filepath.Separator)) {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return nil, fmt.Errorf("resolve path %s: %w", *path, err)
			}
			*path = absolute
		}
	}
	return invocationGlobalArgs(invocationOptions{cfgFile: opts.cfgFile, homeDir: opts.homeDir}), nil
}

func agentSetupCommand() string {
	args, _ := archiveArgs()
	var command strings.Builder
	command.WriteString("msgvault agent setup")
	for i := 0; i < len(args); i += 2 {
		command.WriteString(" " + args[i] + " " + oauth.ShellQuote(args[i+1]))
	}
	return command.String()
}

func agentSetupOptions() agentsetup.Options {
	return agentsetup.Options{SetupCommand: agentSetupCommand(), Items: agentSetupItems}
}

func agentSetupItems(agent string) ([]agentsetup.Item, error) {
	args, err := archiveArgs()
	if err != nil {
		return nil, err
	}
	server := agentmcp.Server{Command: "msgvault", Args: append(args, "mcp")}
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
			return agentsetup.Status{State: agentsetup.StateModified, Reason: result.Skill + " has no msgvault marker", NextStep: "msgvault skills install --force --agent " + agent}, nil
		case skills.CheckNewer:
			return agentsetup.Status{State: agentsetup.StateModified, Reason: result.Skill + " was written by a newer msgvault", NextStep: "msgvault skills install --force --agent " + agent}, nil
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
	opts := agentSetupOptions()
	opts.CheckOnly = true
	report, err := agentsetup.Run(ctx, opts)
	if err != nil {
		return report, fmt.Errorf("check agent setup: %w", err)
	}
	return report, nil
}

func refreshAgentSetup(cmd *cobra.Command, _ []string) error {
	fresh := agentsetup.NewCommand(agentSetupOptions())
	var copyErr error
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if copyErr != nil || fresh.Flags().Lookup(flag.Name) == nil {
			return
		}
		if values, ok := flag.Value.(pflag.SliceValue); ok {
			target, ok := fresh.Flags().Lookup(flag.Name).Value.(pflag.SliceValue)
			if !ok {
				copyErr = fmt.Errorf("copy flag %s: expected a slice", flag.Name)
				return
			}
			copyErr = target.Replace(values.GetSlice())
		} else {
			copyErr = fresh.Flags().Set(flag.Name, flag.Value.String())
		}
	})
	cmd.RunE = fresh.RunE
	if copyErr != nil {
		return fmt.Errorf("copy setup flags: %w", copyErr)
	}
	return nil
}

func init() {
	setup := agentsetup.NewCommand(agentSetupOptions())
	setup.PreRunE = refreshAgentSetup
	agentCmd.AddCommand(setup)
	rootCmd.AddCommand(agentCmd)
}
