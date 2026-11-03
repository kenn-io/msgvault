package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
)

const broadcastCreateSubcommand = "create"

func init() { rootCmd.AddCommand(newBroadcastCommand()) }

func newBroadcastCommand() *cobra.Command {
	group := &cobra.Command{Use: "broadcast", Short: "Send Messenger broadcasts"}
	descriptions := map[string]string{
		"subscriptions": "List subscriptions with token fingerprints", "sync-subscriptions": "Refresh consent from a complete token-list walk", broadcastCreateSubcommand: "Create a broadcast from selected subscriptions or participants", "plan": "Freeze consent decisions and show spend bounds", "show": "Show a broadcast and its recipient receipts", "list": "List a profile's broadcasts", "preview": "Request a Meta preview of the saved message", "send": "Send a confirmed revision and plan digest", "adopt-campaign": "Resolve an uncertain campaign by checking its Page", "cancel": "Cancel local dispatch without deleting the Meta campaign", "unsubscribe": "Suppress locally before requesting Meta unsubscribe",
	}
	for _, sub := range api.BroadcastSubcommands {
		positional := "<broadcast-id>"
		switch sub {
		case "subscriptions", "sync-subscriptions", broadcastCreateSubcommand, "list", "unsubscribe":
			positional = "<profile>"
		}
		command := &cobra.Command{Use: sub + " " + positional, Short: descriptions[sub], Args: cobra.ExactArgs(1), RunE: runBroadcastCommand}
		var intent api.BroadcastIntent
		_ = api.BindBroadcastFlags(command.Flags(), sub, &intent)
		group.AddCommand(command)
	}
	return group
}

func runBroadcastCommand(cmd *cobra.Command, args []string) error {
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	if _, err = api.ParseCLIRunBroadcast(api.CLIRunRequest{Args: runArgs}); err != nil {
		return broadcastArgumentError(err)
	}
	err = runDaemonCLICommandHTTPWithEnv(cmd, runArgs, nil, false, false)
	return classifyBroadcastStreamError(err)
}
