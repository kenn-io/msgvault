package cmd

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/cligrammar"
)

// Keep the production identity even when a test temporarily swaps rootCmd.
var grammarRoot = rootCmd
var grammarRoots sync.Map

var grammarGroups = map[string]struct{ short, group string }{
	"account":          {"List and manage source accounts", helpGroupSources},
	"attachment":       {"Pack, restore, and maintain archived attachments", helpGroupMaintenance},
	"cache":            {"Build and inspect the analytics cache", helpGroupIndex},
	"chat":             {"Find archived chats", helpGroupRead},
	"database":         {"Initialize, migrate, and inspect the archive schema", helpGroupMaintenance},
	"deletion":         {"Stage, review, and execute deletions", helpGroupDelete},
	"domain":           {"List message domains", helpGroupRead},
	"draft":            {"Create and manage provider drafts", helpGroupWrite},
	"export":           {"Export archived messages and attachments", helpGroupExport},
	fullTextSearchMode: {"Maintain full-text search", helpGroupIndex},
	"import":           {"Import messages from local exports", helpGroupImport},
	"label":            {"List archive labels", helpGroupRead},
	"message":          {"Manage archived messages", helpGroupWrite},
	"repair":           {"Repair archived messages and metadata", helpGroupMaintenance},
	"sender":           {"List message senders", helpGroupRead},
	"show":             {"Show an archived message", helpGroupRead},
	"source":           {"Add and sync provider sources", helpGroupSources},
	"thread":           {"Show archived threads", helpGroupRead},
}

// Enroll only the production registry, after init-time flags and before wrappers.
// Private roots in tests and detached MCP constructors retain their definitions.
func ensureCommandGrammar(root *cobra.Command) {
	if root != grammarRoot || root == nil {
		return
	}
	if _, loaded := grammarRoots.LoadOrStore(root, struct{}{}); loaded {
		return
	}
	for name, definition := range grammarGroups {
		topLevelCommandGroups[name] = definition.group
	}
	for _, move := range cligrammar.Moves() {
		original := directCommand(root, move.Legacy)
		if original == nil || original.HasSubCommands() {
			panic("CLI grammar requires a registered leaf: " + move.Legacy)
		}
		path := strings.Fields(move.Canonical)
		parent := root
		for _, name := range path[:len(path)-1] {
			child := directCommand(parent, name)
			if child == nil {
				short := grammarGroups[name].short
				if short == "" {
					short = "Add and sync " + name + " sources"
				}
				child = &cobra.Command{Use: name, Short: short}
				parent.AddCommand(child)
			}
			parent = child
		}
		name := path[len(path)-1]
		if directCommand(parent, name) != nil {
			panic("duplicate canonical CLI path: " + move.Canonical)
		}
		canonical := cloneCommandDefinition(original, name)
		root.RemoveCommand(original)
		parent.AddCommand(canonical)
	}
	configureImportGroup()
	var updateHelp func(*cobra.Command)
	updateHelp = func(cmd *cobra.Command) {
		cmd.Short = canonicalHelpText(cmd.Short)
		cmd.Long = canonicalHelpText(cmd.Long)
		cmd.Example = canonicalHelpText(cmd.Example)
		cmd.Flags().VisitAll(func(flag *pflag.Flag) { flag.Usage = canonicalHelpText(flag.Usage) })
		for _, child := range cmd.Commands() {
			updateHelp(child)
		}
	}
	updateHelp(root)
	for _, provider := range []struct{ name, label string }{
		{"gmail", "Gmail"}, {"imap", "IMAP"}, {"o365", "Microsoft 365"},
	} {
		group := directCommand(directCommand(root, "source"), provider.name)
		group.Short = "Add " + provider.label + " accounts (sync with 'msgvault sync')"
		group.Long = "Add " + provider.label + " accounts. Sync mail with 'msgvault sync <account>'; use 'msgvault sync full <account>' for a full sync."
	}
	directCommand(directCommand(root, "source"), "carddav").Long = "Add and sync CardDAV sources. Manage remote address books and contacts with 'msgvault carddav'."
	applyReadHelp(root)
	applySourceHelp(root)
	applyOpsHelp(root)
	directCommand(root, "show").Aliases = []string{"get"}
	directCommand(root, "search").Aliases = append(directCommand(root, "search").Aliases, "find")
}

func directCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, child := range parent.Commands() {
		if child.Name() == name {
			return child
		}
	}
	return nil
}

// Copy public definitions, never Cobra's private tree, parse, or execution state.
// Flag values remain shared with the existing handlers; metadata is independent.
func cloneCommandDefinition(source *cobra.Command, name string) *cobra.Command {
	_, synopsis, _ := strings.Cut(source.Use, " ")
	use := name
	if synopsis != "" {
		use += " " + synopsis
	}
	target := &cobra.Command{
		Use: use, SuggestFor: slices.Clone(source.SuggestFor), Short: source.Short,
		Long: source.Long, Example: source.Example, ValidArgs: slices.Clone(source.ValidArgs),
		ValidArgsFunction: source.ValidArgsFunction, Args: source.Args, ArgAliases: slices.Clone(source.ArgAliases),
		BashCompletionFunction: source.BashCompletionFunction, Annotations: maps.Clone(source.Annotations), Version: source.Version,
		PersistentPreRun: source.PersistentPreRun, PersistentPreRunE: source.PersistentPreRunE,
		PreRun: source.PreRun, PreRunE: source.PreRunE, Run: source.Run, RunE: source.RunE,
		PostRun: source.PostRun, PostRunE: source.PostRunE,
		PersistentPostRun: source.PersistentPostRun, PersistentPostRunE: source.PersistentPostRunE,
		FParseErrWhitelist: source.FParseErrWhitelist, CompletionOptions: source.CompletionOptions,
		TraverseChildren: source.TraverseChildren, SilenceErrors: source.SilenceErrors, SilenceUsage: source.SilenceUsage,
		DisableFlagParsing: source.DisableFlagParsing, DisableAutoGenTag: source.DisableAutoGenTag,
		DisableFlagsInUseLine: source.DisableFlagsInUseLine, DisableSuggestions: source.DisableSuggestions,
		SuggestionsMinimumDistance: source.SuggestionsMinimumDistance,
	}
	copyFlags := func(from, to *pflag.FlagSet) {
		from.VisitAll(func(flag *pflag.Flag) {
			copied := *flag
			copied.Changed = false
			if flag.Annotations != nil {
				copied.Annotations = make(map[string][]string, len(flag.Annotations))
				for key, value := range flag.Annotations {
					copied.Annotations[key] = slices.Clone(value)
				}
			}
			to.AddFlag(&copied)
			if complete, ok := source.GetFlagCompletionFunc(flag.Name); ok {
				if err := target.RegisterFlagCompletionFunc(flag.Name, complete); err != nil {
					panic(err)
				}
			}
		})
	}
	copyFlags(source.LocalNonPersistentFlags(), target.Flags())
	copyFlags(source.PersistentFlags(), target.PersistentFlags())
	return target
}

func configureImportGroup() {
	importCmd.Hidden = false
	importCmd.Deprecated = ""
	importCmd.Short = grammarGroups["import"].short
	importCmd.Long = "Import a local export with the matching format subcommand."
	importCmd.Use = "import"
	importCmd.Flags().VisitAll(func(flag *pflag.Flag) { flag.Hidden = true })
	importCmd.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && !cmd.Flags().Changed("type") {
			return nil
		}
		return cobra.ExactArgs(1)(cmd, args)
	}
	run := importCmd.RunE
	importCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return run(cmd, args)
	}
}

var commandHelpWords = regexp.MustCompile(`[A-Za-z0-9_-]+`)
var grammarHelpNames = func() map[string]string {
	names := map[string]string{}
	for _, move := range cligrammar.Moves() {
		if move.Legacy != "migrate" {
			names[move.Legacy] = move.Canonical
		}
	}
	return names
}()

func canonicalHelpText(text string) string {
	text = strings.ReplaceAll(text, "msgvault migrate", "msgvault database migrate")
	var result strings.Builder
	end := 0
	for _, match := range commandHelpWords.FindAllStringIndex(text, -1) {
		word := text[match[0]:match[1]]
		replacement, ok := grammarHelpNames[word]
		// Keep link anchors, filesystem paths and filenames intact.
		if !ok || (match[0] > 0 && strings.ContainsRune("/#.", rune(text[match[0]-1]))) || (match[1]+1 < len(text) && text[match[1]] == '.' && text[match[1]+1] >= 'a' && text[match[1]+1] <= 'z') {
			continue
		}
		canonicalPath := strings.Fields(replacement)
		if len(canonicalPath) > 1 && canonicalPath[len(canonicalPath)-1] == word &&
			strings.HasSuffix(text[:match[0]], strings.Join(canonicalPath[:len(canonicalPath)-1], " ")+" ") {
			continue
		}
		result.WriteString(text[end:match[0]])
		result.WriteString(replacement)
		end = match[1]
	}
	result.WriteString(text[end:])
	return result.String()
}

func stableCommandPath(cmd *cobra.Command) string {
	return strings.Join(cligrammar.Legacy(daemonCLICommandPath(cmd)), " ")
}

// LauncherArgs supports the hook executable name without a deprecated command.
func LauncherArgs(argv []string) []string {
	if len(argv) == 1 && strings.TrimSuffix(filepath.Base(argv[0]), ".exe") == "msgvault-muesli-hook" {
		return []string{argv[0], "source", "muesli", "hook"}
	}
	return slices.Clone(argv)
}
