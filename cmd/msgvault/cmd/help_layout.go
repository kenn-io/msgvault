package cmd

import (
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var helpLayoutRoots sync.Map
var helpTemplateFunctions sync.Once

const helpOnlyGroupAnnotation = "msgvault.help_only_group"

const (
	helpGroupRead        = "read"
	helpGroupExport      = "export"
	helpGroupPeople      = "people"
	helpGroupSources     = "sources"
	helpGroupImport      = "import"
	helpGroupWrite       = "write"
	helpGroupDelete      = "delete"
	helpGroupIndex       = "index"
	helpGroupMaintenance = "maint"
	helpGroupOperations  = "ops"
)

var topLevelCommandGroups = map[string]string{
	"activity":               helpGroupIndex,
	"add-account":            helpGroupSources,
	"add-beeper":             helpGroupSources,
	"add-calendar":           helpGroupSources,
	"add-carddav":            helpGroupSources,
	"add-circleback":         helpGroupSources,
	"add-discord":            helpGroupSources,
	"add-granola":            helpGroupSources,
	"add-imap":               helpGroupSources,
	"add-matrix":             helpGroupSources,
	"add-muesli":             helpGroupSources,
	"add-notion-meetings":    helpGroupSources,
	"add-o365":               helpGroupSources,
	"add-plaud":              helpGroupSources,
	"add-slack":              helpGroupSources,
	"add-synctech-sms-drive": helpGroupSources,
	"add-teams":              helpGroupSources,
	"add-twenty":             helpGroupSources,
	"add-twilio":             helpGroupSources,
	"agent-token":            helpGroupOperations,
	"archive-remote-images":  helpGroupSources,
	"attribute-definition":   helpGroupPeople,
	"backfill-beeper-media":  helpGroupSources,
	"backfill-discord-media": helpGroupSources,
	"backfill-slack-media":   helpGroupSources,
	"backfill-teams-media":   helpGroupSources,
	"backup":                 helpGroupMaintenance,
	"build-cache":            helpGroupIndex,
	"cache-stats":            helpGroupIndex,
	"calendar":               helpGroupWrite,
	"cancel-deletion":        helpGroupDelete,
	"carddav":                helpGroupPeople,
	"collection":             helpGroupPeople,
	"completion":             helpGroupOperations,
	"create-subset":          helpGroupExport,
	"credentials":            helpGroupOperations,
	"daemon":                 helpGroupOperations,
	"deduplicate":            helpGroupDelete,
	"delete-deduped":         helpGroupDelete,
	"delete-staged":          helpGroupDelete,
	"documents":              helpGroupIndex,
	"draft-compose":          helpGroupWrite,
	"draft-delete":           helpGroupWrite,
	"draft-edit":             helpGroupWrite,
	"draft-forward":          helpGroupWrite,
	"draft-get":              helpGroupWrite,
	"draft-recover":          helpGroupWrite,
	"draft-reply":            helpGroupWrite,
	"draft-send-as":          helpGroupWrite,
	"embeddings":             helpGroupIndex,
	"employment":             helpGroupPeople,
	"eval":                   helpGroupIndex,
	"export-attachment":      helpGroupExport,
	"export-attachments":     helpGroupExport,
	"export-discord":         helpGroupExport,
	"export-eml":             helpGroupExport,
	"export-messages":        helpGroupExport,
	"export-token":           helpGroupOperations,
	"find-chat":              helpGroupRead,
	"gc":                     helpGroupDelete,
	"identity":               helpGroupPeople,
	"import-eml":             helpGroupImport,
	"import-emlx":            helpGroupImport,
	"import-gvoice":          helpGroupImport,
	"import-imazing-csv":     helpGroupImport,
	"import-imessage":        helpGroupImport,
	"import-maildir":         helpGroupImport,
	"import-mbox":            helpGroupImport,
	"import-messenger":       helpGroupImport,
	"import-pst":             helpGroupImport,
	"import-slackdump":       helpGroupImport,
	"import-synctech-sms":    helpGroupImport,
	"import-whatsapp":        helpGroupImport,
	"init-db":                helpGroupMaintenance,
	"kata":                   helpGroupWrite,
	"list-accounts":          helpGroupRead,
	"list-deletions":         helpGroupDelete,
	"list-domains":           helpGroupRead,
	"list-folders":           helpGroupSources,
	"list-labels":            helpGroupRead,
	"list-senders":           helpGroupRead,
	"logs":                   helpGroupOperations,
	"mcp":                    helpGroupOperations,
	"media":                  helpGroupRead,
	"meetings":               helpGroupRead,
	"migrate":                helpGroupMaintenance,
	"multimodal":             helpGroupIndex,
	"muesli-hook":            helpGroupSources,
	"openapi":                helpGroupOperations,
	"organization":           helpGroupPeople,
	"pack-attachments":       helpGroupMaintenance,
	"person":                 helpGroupPeople,
	"purge-excluded-media":   helpGroupMaintenance,
	"query":                  helpGroupRead,
	"quickstart":             helpGroupOperations,
	"rebuild-fts":            helpGroupIndex,
	"relationship-type":      helpGroupPeople,
	"remove-account":         helpGroupSources,
	"repack-attachments":     helpGroupMaintenance,
	"repair-dates":           helpGroupMaintenance,
	"repair-derived":         helpGroupMaintenance,
	"repair-encoding":        helpGroupMaintenance,
	"repair-identity":        helpGroupMaintenance,
	"repair-labels":          helpGroupMaintenance,
	"repair-list-ids":        helpGroupMaintenance,
	"repair-message":         helpGroupMaintenance,
	"repair-senders":         helpGroupMaintenance,
	"schema-version":         helpGroupMaintenance,
	"search":                 helpGroupRead,
	"serve":                  helpGroupOperations,
	"setup":                  helpGroupOperations,
	"show-deletion":          helpGroupDelete,
	"show-message":           helpGroupRead,
	"skills":                 helpGroupOperations,
	"stage-delete":           helpGroupDelete,
	"stats":                  helpGroupRead,
	"sync":                   helpGroupSources,
	"sync-beeper":            helpGroupSources,
	"sync-calendar":          helpGroupSources,
	"sync-carddav":           helpGroupSources,
	"sync-circleback":        helpGroupSources,
	"sync-discord":           helpGroupSources,
	"sync-full":              helpGroupSources,
	"sync-granola":           helpGroupSources,
	"sync-matrix":            helpGroupSources,
	"sync-muesli":            helpGroupSources,
	"sync-notion-meetings":   helpGroupSources,
	"sync-plaud":             helpGroupSources,
	"sync-slack":             helpGroupSources,
	"sync-synctech-sms":      helpGroupSources,
	"sync-teams":             helpGroupSources,
	"sync-twenty":            helpGroupSources,
	"sync-twilio":            helpGroupSources,
	"tui":                    helpGroupRead,
	"unpack-attachments":     helpGroupMaintenance,
	"update":                 helpGroupOperations,
	"update-account":         helpGroupSources,
	"verify":                 helpGroupSources,
	"version":                helpGroupOperations,
}

func ensureHelpLayout(root *cobra.Command) {
	if _, loaded := helpLayoutRoots.LoadOrStore(root, struct{}{}); loaded {
		return
	}
	registerHelpTemplateFunctions()
	root.AddGroup(
		&cobra.Group{ID: helpGroupRead, Title: "Search and read:"},
		&cobra.Group{ID: helpGroupExport, Title: "Export:"},
		&cobra.Group{ID: helpGroupPeople, Title: "People, organizations, and identity:"},
		&cobra.Group{ID: helpGroupSources, Title: "Add and sync sources:"},
		&cobra.Group{ID: helpGroupImport, Title: "Import local exports:"},
		&cobra.Group{ID: helpGroupWrite, Title: "Drafts, calendar, and Kata (write to providers):"},
		&cobra.Group{ID: helpGroupDelete, Title: "Review and delete:"},
		&cobra.Group{ID: helpGroupIndex, Title: "Indexes, embeddings, and analytics cache:"},
		&cobra.Group{ID: helpGroupMaintenance, Title: "Maintenance and repair:"},
		&cobra.Group{ID: helpGroupOperations, Title: "Setup, daemon, and integrations:"},
	)
	for _, command := range root.Commands() {
		command.GroupID = topLevelCommandGroups[command.Name()]
	}
	root.SetHelpCommandGroupID(helpGroupOperations)
	root.SetCompletionCommandGroupID(helpGroupOperations)
	root.SetUsageTemplate(compactHelpUsageTemplate)
	var wrapGroups func(*cobra.Command)
	wrapGroups = func(command *cobra.Command) {
		if command != root && command.HasSubCommands() && !command.Runnable() {
			if command.Annotations == nil {
				command.Annotations = make(map[string]string)
			}
			command.Annotations[helpOnlyGroupAnnotation] = "true"
			command.Args = cobra.ArbitraryArgs
			command.RunE = func(cmd *cobra.Command, args []string) error {
				if len(args) == 0 {
					return cmd.Help()
				}
				return usageErr(cmd, fmt.Errorf("unknown command %q for %q%s", args[0], cmd.CommandPath(), suggestionText(cmd, args[0])))
			}
		}
		for _, child := range command.Commands() {
			wrapGroups(child)
		}
	}
	wrapGroups(root)
}

func registerHelpTemplateFunctions() {
	helpTemplateFunctions.Do(func() {
		cobra.AddTemplateFunc("globalFlagNames", globalFlagNames)
		cobra.AddTemplateFunc("parentFlagUsages", parentFlagUsages)
		cobra.AddTemplateFunc("groupNamePadding", groupNamePadding)
	})
}

func isHelpOnlyGroup(command *cobra.Command) bool {
	return command.Annotations[helpOnlyGroupAnnotation] == "true"
}

// Align each group independently so long source names do not pad every row.
func groupNamePadding(commands []*cobra.Command, group string) int {
	padding := 11 // Cobra's minimum name padding.
	for _, command := range commands {
		if command.GroupID == group && (command.IsAvailableCommand() || command.Name() == "help") {
			if len(command.Name()) > padding {
				padding = len(command.Name())
			}
		}
	}
	return padding
}

// isRootFlag reports whether an inherited flag is the root's own persistent
// flag rather than one a parent group defines, such as calendar's --account.
func isRootFlag(command *cobra.Command, flag *pflag.Flag) bool {
	return command.Root().PersistentFlags().Lookup(flag.Name) == flag
}

// globalFlagNames lists root flags by name; root help describes them.
func globalFlagNames(command *cobra.Command) string {
	names := []string{}
	command.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || !isRootFlag(command, flag) {
			return
		}
		name := "--" + flag.Name
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			name = "-" + flag.Shorthand + "/" + name
		}
		names = append(names, name)
	})
	return strings.Join(names, ", ")
}

// parentFlagUsages describes flags inherited from parent groups in full,
// because root help does not document them.
func parentFlagUsages(command *cobra.Command) string {
	flags := pflag.NewFlagSet(command.Name(), pflag.ContinueOnError)
	command.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if !isRootFlag(command, flag) {
			flags.AddFlag(flag)
		}
	})
	return flags.FlagUsages()
}

func suggestionText(command *cobra.Command, arg string) string {
	if command.DisableSuggestions {
		return ""
	}
	if command.SuggestionsMinimumDistance <= 0 {
		command.SuggestionsMinimumDistance = 2
	}
	suggestions := command.SuggestionsFor(arg)
	if len(suggestions) == 0 {
		return ""
	}
	return "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t") + "\n"
}

// Based on Cobra v1.10.2's defaultUsageTemplate in command.go. When bumping
// Cobra, re-diff this copy. Inherited flags, help-only groups, and group padding differ.
// Root flags are listed by name; flags from parent groups print in full.
const compactHelpUsageTemplate = `Usage:{{if and .Runnable (not (index .Annotations "` + helpOnlyGroupAnnotation + `"))}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name (groupNamePadding $cmds $group.ID) }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}{{with parentFlagUsages .}}

Inherited Flags:
{{. | trimTrailingWhitespaces}}{{end}}{{with globalFlagNames .}}

Global Flags: {{.}} (see 'msgvault --help'){{end}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`
