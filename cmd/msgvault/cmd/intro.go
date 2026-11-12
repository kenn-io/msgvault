package cmd

import (
	"github.com/spf13/cobra"
)

func init() { rootCmd.AddCommand(newIntroCommand(runDaemonCLICommandHTTPFromCobra)) }

// newIntroCommand shares its grammar between the client and the daemon.
func newIntroCommand(execute func(*cobra.Command, []string) error) *cobra.Command {
	root := &cobra.Command{Use: "intro", Short: "Prepare local introductions"}
	prepare := &cobra.Command{Use: "prepare", Short: "Prepare an introduction locally", Args: cobra.NoArgs, RunE: execute}
	prepare.Flags().Int64("source-id", 0, "Beeper source ID")
	prepare.Flags().Int64Slice("person", nil, "person ID (repeat twice)")
	prepare.Flags().String("title", "", "group title")
	prepare.Flags().String("body", "", "introduction text")
	for _, name := range []string{"source-id", "person", "title", "body"} {
		_ = prepare.MarkFlagRequired(name)
	}
	list := &cobra.Command{Use: "list", Short: "List introductions", Args: cobra.NoArgs, RunE: execute}
	list.Flags().Int64("source-id", 0, "filter by source ID")
	list.Flags().Int("limit", 20, "maximum records (1–100)")
	list.Flags().String("after-id", "", "pagination cursor")
	show := &cobra.Command{Use: "show ID", Short: "Show an introduction", Args: cobra.ExactArgs(1), RunE: execute}
	edit := &cobra.Command{Use: "edit ID", Short: "Edit an introduction", Args: cobra.ExactArgs(1), RunE: execute}
	edit.Flags().Int64("revision", 0, "current revision")
	edit.Flags().String("title", "", "replacement title")
	edit.Flags().String("body", "", "replacement body")
	_ = edit.MarkFlagRequired("revision")
	cancel := &cobra.Command{Use: "cancel ID", Short: "Cancel a prepared introduction", Args: cobra.ExactArgs(1), RunE: execute}
	cancel.Flags().Int64("revision", 0, "current revision")
	_ = cancel.MarkFlagRequired("revision")
	for _, cmd := range []*cobra.Command{prepare, list, show, edit, cancel} {
		cmd.Flags().Bool("json", false, "emit JSON")
		root.AddCommand(cmd)
	}
	return root
}
