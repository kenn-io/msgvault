package cmd

import (
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func newCLISearchCommand(root *cobra.Command) *cobra.Command {
	options := cliSearchOptions()
	var cli *cobra.Command
	options.UsageError = func(err error) error {
		// The root wraps both group and leaf RunE handlers to hide usage
		// for runtime failures. Discovery validation is a usage error.
		for _, child := range cli.Commands() {
			child.SilenceUsage = false
		}
		return usageErr(cli, err)
	}
	options.Render = func(command *cobra.Command, response clisearch.Response) error {
		asJSON, _ := command.Flags().GetBool(flagJSON)
		if asJSON {
			return printJSONTo(command.OutOrStdout(), response)
		}
		return clisearch.RenderText(command, response)
	}
	cli = clisearch.NewCommand(root, options)
	for _, child := range cli.Commands() {
		if child.Name() == "search" {
			child.Flags().Bool(flagJSON, false, "Output structured JSON")
		}
	}
	return cli
}

func cliSearchOptions() clisearch.Options {
	const messageKeyword = "message"

	return clisearch.Options{
		Synonyms: map[string][]string{
			"email":         {"mail", messageKeyword},
			"mail":          {"email", messageKeyword},
			messageKeyword:  {"email", "mail"},
			"contact":       {"person", helpGroupPeople},
			helpGroupPeople: {"person", "contact"},
			"person":        {"contact", helpGroupPeople},
			"attachment":    {"document"},
			"document":      {"attachment"},
			"transcription": {"transcript"},
			"transcript":    {"transcription"},
			"photo":         {"image", "picture"},
			"picture":       {"image", "photo"},
			"image":         {"photo", "picture"},
		},
		Skip: skipCLICommand,
	}
}

func skipCLICommand(command *cobra.Command) bool {
	if isHelpOnlyGroup(command) {
		return true
	}
	path := strings.TrimPrefix(command.CommandPath(), command.Root().Name()+" ")
	switch path {
	case "tui", "setup", "add-account", "add-calendar", "add-inline", "add-o365", "add-circleback",
		"add-teams", "add-plaud", "add-synctech-sms-drive",
		"carddav authorize-google", "carddav authorize-microsoft",
		"person provider login", "person provider enroll-codex":
		return true
	}
	return false
}

// Only the root's discovery subtree is independent of archive configuration.
// A leaf-name exemption would also bypass the existing message search.
func isCLIDiscoveryCommand(command *cobra.Command) bool {
	for ancestor := command; ancestor != nil; ancestor = ancestor.Parent() {
		if ancestor.Name() == "cli" && ancestor.Parent() == command.Root() {
			return true
		}
	}
	return false
}

func init() {
	// Disambiguate intents left implicit in the short help summaries.
	clisearch.SetKeywords(searchCmd, "email mail sender")
	clisearch.SetKeywords(showMessageCmd, "read message id")
	clisearch.SetKeywords(listAccountsCmd, "list archived accounts")
	clisearch.SetKeywords(personNotesAppendCmd, "log note person")
	rootCmd.AddCommand(newCLISearchCommand(rootCmd))
}
