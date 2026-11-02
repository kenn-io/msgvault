package cmd

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ensureXCLICommandGroups gives the X feature its canonical command paths
// without changing the stable command names sent to the daemon.
func ensureXCLICommandGroups(root *cobra.Command) {
	if root == nil || !hasXCLICommands(root) {
		return
	}

	source := ensureCLIGroup(root, "source", "Add and sync provider sources")
	xSource := ensureCLIGroup(source, "x", "Authorize and sync X archive sources")
	draft := ensureCLIGroup(root, "draft", "Create and manage provider drafts")
	message := ensureCLIGroup(root, "message", "Manage archived messages")
	setCLIGroupHelp(source, `Add and sync provider sources. Provider setup and collection commands live under source <provider>.`, "  msgvault source x --help")
	setCLIGroupHelp(xSource, `Authorize an X account, collect its own posts and returned public replies, and run local X maintenance. Ordinary sync performs due content refresh before collection; local removal and removal-record export are options on source x sync. Snapshot recovery uses the generic msgvault backup restore command with the current X removal record and head. Collection and sends use the selected daemon's X source.`, `  msgvault source x add --help
  msgvault source x sync --source-id 42
  msgvault source x sync --source-id 42 --remove-post 123
  msgvault source x sync --export-removals --json`)
	setCLIGroupHelp(draft, `Create and manage provider drafts. Creating a draft does not send it.`, "  msgvault draft reply <message-id> --body 'Thanks for the details.'")
	setCLIGroupHelp(message, `Send an owner-reviewed reply using an archived message or a managed draft. The archived message or draft determines the source.`, `  msgvault message reply 55102 --body 'Thanks!' --json
  msgvault message reply --draft <draft-id> --revision 1 --json`)

	registerXCLICommand(root, xSource, "add-x", "add", "source x add")
	registerXCLICommand(root, xSource, "sync-x", "sync", "source x sync")
	registerXCLICommand(root, draft, "draft-reply", "reply <message-id>", "draft reply")
	registerXCLICommand(root, message, "send-reply", "reply [archived-message-id]", "message reply")
}

func hasXCLICommands(root *cobra.Command) bool {
	for _, name := range []string{"add-x", "sync-x", "draft-reply", "send-reply"} {
		if findDirectCLICommand(root, name) != nil {
			return true
		}
	}
	if source := findDirectCLICommand(root, "source"); source != nil {
		if xSource := findDirectCLICommand(source, "x"); xSource != nil &&
			(findDirectCLICommand(xSource, "add") != nil || findDirectCLICommand(xSource, "sync") != nil) {
			return true
		}
	}
	if draft := findDirectCLICommand(root, "draft"); draft != nil && findDirectCLICommand(draft, "reply") != nil {
		return true
	}
	if message := findDirectCLICommand(root, "message"); message != nil && findDirectCLICommand(message, "reply") != nil {
		return true
	}
	return false
}

func ensureCLIGroup(parent *cobra.Command, name, short string) *cobra.Command {
	if group := findDirectCLICommand(parent, name); group != nil {
		return group
	}
	group := &cobra.Command{Use: name, Short: short}
	parent.AddCommand(group)
	return group
}

func setCLIGroupHelp(command *cobra.Command, long, example string) {
	if command.Long == "" {
		command.Long = long
	}
	if command.Example == "" {
		command.Example = example
	}
}

func findDirectCLICommand(parent *cobra.Command, name string) *cobra.Command {
	if parent == nil {
		return nil
	}
	for _, command := range parent.Commands() {
		if command.Name() == name {
			return command
		}
	}
	return nil
}

func registerXCLICommand(root, parent *cobra.Command, legacyName, canonicalUse, canonicalPath string) {
	legacy := findDirectCLICommand(root, legacyName)
	canonicalName := strings.Fields(canonicalUse)[0]
	canonical := findDirectCLICommand(parent, canonicalName)
	if legacy == nil && canonical == nil {
		return
	}
	if canonical == nil {
		canonical = cloneXCLICommand(legacy, canonicalUse)
		copyXCLIFlagGroups(legacyName, canonical)
		parent.AddCommand(canonical)
	}
	if legacy != nil && !legacy.Hidden {
		markLegacyXCLICommand(legacy, canonicalPath)
	}
}

// Cobra keeps required/exclusive flag groups in private command fields rather
// than flag metadata. Re-declare the X command's one group on its public clone.
func copyXCLIFlagGroups(legacyName string, command *cobra.Command) {
	if legacyName == "sync-x" {
		command.MarkFlagsMutuallyExclusive("job-id", "new-job")
	}
}

// Clone only Cobra's public command definition. Keep the original handler and
// pflag values so both spellings run the same implementation and serialize the
// same stable daemon operation.
func cloneXCLICommand(source *cobra.Command, use string) *cobra.Command {
	target := &cobra.Command{
		Use:                        use,
		Aliases:                    slices.Clone(source.Aliases),
		SuggestFor:                 slices.Clone(source.SuggestFor),
		Short:                      source.Short,
		Long:                       source.Long,
		Example:                    source.Example,
		ValidArgs:                  slices.Clone(source.ValidArgs),
		ValidArgsFunction:          source.ValidArgsFunction,
		Args:                       source.Args,
		ArgAliases:                 slices.Clone(source.ArgAliases),
		BashCompletionFunction:     source.BashCompletionFunction,
		Annotations:                maps.Clone(source.Annotations),
		Version:                    source.Version,
		PersistentPreRun:           source.PersistentPreRun,
		PersistentPreRunE:          source.PersistentPreRunE,
		PreRun:                     source.PreRun,
		PreRunE:                    source.PreRunE,
		Run:                        source.Run,
		RunE:                       source.RunE,
		PostRun:                    source.PostRun,
		PostRunE:                   source.PostRunE,
		PersistentPostRun:          source.PersistentPostRun,
		PersistentPostRunE:         source.PersistentPostRunE,
		FParseErrWhitelist:         source.FParseErrWhitelist,
		CompletionOptions:          source.CompletionOptions,
		TraverseChildren:           source.TraverseChildren,
		SilenceErrors:              source.SilenceErrors,
		SilenceUsage:               source.SilenceUsage,
		DisableFlagParsing:         source.DisableFlagParsing,
		DisableAutoGenTag:          source.DisableAutoGenTag,
		DisableFlagsInUseLine:      source.DisableFlagsInUseLine,
		DisableSuggestions:         source.DisableSuggestions,
		SuggestionsMinimumDistance: source.SuggestionsMinimumDistance,
	}
	copyFlags := func(from, to *pflag.FlagSet) {
		from.VisitAll(func(flag *pflag.Flag) {
			cloned := *flag
			cloned.Changed = false
			if flag.Annotations != nil {
				cloned.Annotations = make(map[string][]string, len(flag.Annotations))
				for key, value := range flag.Annotations {
					cloned.Annotations[key] = slices.Clone(value)
				}
			}
			to.AddFlag(&cloned)
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

const legacyXCommandAnnotation = "msgvault.x-legacy-command"

func markLegacyXCLICommand(command *cobra.Command, canonicalPath string) {
	if command.Annotations != nil && command.Annotations[legacyXCommandAnnotation] == canonicalPath {
		return
	}
	command.Hidden = true
	command.Deprecated = ""
	if command.Annotations == nil {
		command.Annotations = make(map[string]string)
	}
	command.Annotations[legacyXCommandAnnotation] = canonicalPath
	previousFlagError := command.FlagErrorFunc()
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		noticeErr := writeXCLICommandNotice(cmd, canonicalPath)
		if previousFlagError != nil {
			err = previousFlagError(cmd, err)
		}
		return errors.Join(err, noticeErr)
	})
	args := command.Args
	command.Args = func(cmd *cobra.Command, values []string) error {
		if err := writeXCLICommandNotice(cmd, canonicalPath); err != nil {
			return err
		}
		if args != nil {
			return args(cmd, values)
		}
		return nil
	}
	help := command.Root().HelpFunc()
	command.SetHelpFunc(func(cmd *cobra.Command, values []string) {
		_ = writeXCLICommandNotice(cmd, canonicalPath)
		help(cmd, values)
	})
}

func writeXCLICommandNotice(cmd *cobra.Command, canonicalPath string) error {
	if isDaemonConsoleSubprocess() {
		return nil
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), xCLICommandNotice(cmd, canonicalPath)); err != nil {
		return fmt.Errorf("write legacy command notice: %w", err)
	}
	return nil
}

func xCLICommandNotice(cmd *cobra.Command, canonicalPath string) string {
	name := cmd.CalledAs()
	if name == "" {
		name = cmd.Name()
	}
	return fmt.Sprintf("Command %q is deprecated, use 'msgvault %s' instead", name, canonicalPath)
}

func xCommandUsageErr(cmd *cobra.Command, err error, hint string) error {
	if hint != "" {
		err = fmt.Errorf("%w\nhint: %s", err, hint)
	}
	return usageErr(cmd, err)
}

func isCanonicalDraftReplyCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "reply" && cmd.Parent() != nil &&
		cmd.Parent().Name() == "draft" && cmd.Parent().Parent() == cmd.Root()
}
