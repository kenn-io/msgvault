package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "comment", Short: "Act on archived Facebook and Instagram comments"}
	cmd.AddCommand(newCommentModerateCmd())
	return cmd
}

func newCommentModerateCmd() *cobra.Command {
	var hide, unhide, remove, dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "moderate <message-id> --hide|--unhide|--delete",
		Short: "Hide, unhide or delete an archived comment on Facebook or Instagram",
		Long: "Hide, unhide or delete an archived Facebook Page or Instagram comment on Meta, then record the change in the archive. " +
			"The comment's [[facebook_comments]] or [[instagram_comments]] profile must list the action in actions; \"hide\" also allows unhide. " +
			"--dry-run shows the comment without calling Meta. Delete cannot be undone, so it also needs --yes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return opserr.Invalid(errors.New("message ID must be a positive archive message ID"))
			}
			var action meta.CommentAction
			selected := 0
			for _, choice := range []struct {
				set    bool
				action meta.CommentAction
			}{{hide, meta.ActionHide}, {unhide, meta.ActionUnhide}, {remove, meta.ActionDelete}} {
				if choice.set {
					action = choice.action
					selected++
				}
			}
			if selected != 1 {
				return opserr.Invalid(errors.New("choose one of --hide, --unhide or --delete"))
			}
			if action == meta.ActionDelete && !dryRun && !yes {
				return opserr.Invalid(errors.New("delete removes the comment from Meta; preview with --dry-run, then repeat with --yes"))
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
			if err != nil {
				return err
			}
			defer cleanup()
			target, err := meta.ResolveCommentTarget(cmd.Context(), st, id)
			if errors.Is(err, store.ErrMessageNotFound) {
				return opserr.NotFound(fmt.Errorf("%w; it may already be deleted from Meta", err))
			}
			if err != nil {
				return err
			}
			profile, err := commentProfileFor(state.cfg, target, action)
			if err != nil {
				return err
			}
			safe := textutil.SanitizeTerminal
			description := fmt.Sprintf("%s comment %s by %s (message %d)", target.Provider(), safe(target.CommentID), safe(target.Author), target.MessageID)
			if dryRun {
				hidden := ""
				if target.Hidden {
					hidden = " [hidden]"
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Would %s %s%s: %s\n", action, description, hidden, safe(target.Text)); err != nil {
					return fmt.Errorf("write comment preview: %w", err)
				}
				return nil
			}
			client, err := profile.client()
			if err != nil {
				return err
			}
			if err := meta.ModerateComment(cmd.Context(), st, client, target, action); err != nil {
				return metaSourceStateError(err, profile.command)
			}
			done := map[meta.CommentAction]string{meta.ActionHide: "Hid", meta.ActionUnhide: "Unhid", meta.ActionDelete: "Deleted"}[action]
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", done, description); err != nil {
				return fmt.Errorf("write comment result: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&hide, "hide", false, "hide the comment from public view")
	cmd.Flags().BoolVar(&unhide, "unhide", false, "make a hidden comment visible again")
	cmd.Flags().BoolVar(&remove, "delete", false, "delete the comment from Meta; the archive keeps it, marked deleted")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the comment without calling Meta")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --delete")
	return cmd
}

type commentProfile struct {
	command string
	actions []string
	client  func() (*meta.Client, error)
}

// commentProfileFor finds the profile that synced target and checks that it
// opts in to action. The comment's source type is also its config section.
func commentProfileFor(cfg *config.Config, target meta.CommentTarget, action meta.CommentAction) (commentProfile, error) {
	var found *commentProfile
	switch target.SourceType {
	case meta.SourceType:
		for _, p := range cfg.FacebookComments {
			if meta.SourceIdentifier(p.AppID, p.PageID) == target.SourceIdentifier {
				found = &commentProfile{command: "facebook-comments", actions: p.Actions, client: func() (*meta.Client, error) { return configuredFacebookCommentsClient(p) }}
			}
		}
	case meta.InstagramSourceType:
		for _, p := range cfg.InstagramComments {
			if meta.InstagramSourceIdentifier(p.AppID, p.PageID, p.InstagramAccountID) == target.SourceIdentifier {
				found = &commentProfile{command: "instagram-comments", actions: p.Actions, client: func() (*meta.Client, error) { return configuredInstagramCommentsClient(p) }}
			}
		}
	}
	section := "[[" + target.SourceType + "]]"
	if found == nil {
		return commentProfile{}, &sourceCommandError{err: fmt.Errorf("no %s profile matches source %s", section, target.SourceIdentifier), code: 5, hint: "configure the profile that synced this comment"}
	}
	if !slices.Contains(found.actions, action.ProfileAction()) {
		return commentProfile{}, &sourceCommandError{
			err:  fmt.Errorf("the %s profile for this comment does not allow %s", section, action),
			code: 5,
			hint: fmt.Sprintf("add %q to actions in that profile", action.ProfileAction()),
		}
	}
	return *found, nil
}

func init() { rootCmd.AddCommand(newCommentCmd()) }
