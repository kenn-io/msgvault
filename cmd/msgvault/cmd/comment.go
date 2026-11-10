package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newCommentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "comment", Short: "Act on archived Facebook and Instagram comments"}
	cmd.AddCommand(newCommentModerateCmd(), newCommentReplyCmd())
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
			return runCommentCommand(cmd, args, action, func(c commentCommand) (string, error) {
				if dryRun {
					hidden := ""
					if c.target.Hidden {
						hidden = " [hidden]"
					}
					return fmt.Sprintf("Would %s %s%s: %s", action, c.description, hidden, textutil.SanitizeTerminal(c.target.Text)), nil
				}
				client, err := c.profile.client()
				if err != nil {
					return "", err
				}
				if err := meta.ModerateComment(cmd.Context(), c.st, client, c.target, action); err != nil {
					return "", metaSourceStateError(err, c.profile.command)
				}
				done := map[meta.CommentAction]string{meta.ActionHide: "Hid", meta.ActionUnhide: "Unhid", meta.ActionDelete: "Deleted"}[action]
				return done + " " + c.description, nil
			})
		},
	}
	cmd.Flags().BoolVar(&hide, "hide", false, "hide the comment from public view")
	cmd.Flags().BoolVar(&unhide, "unhide", false, "make a hidden comment visible again")
	cmd.Flags().BoolVar(&remove, "delete", false, "delete the comment from Meta; the archive keeps it, marked deleted")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the comment without calling Meta")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --delete")
	return cmd
}

func newCommentReplyCmd() *cobra.Command {
	var body string
	var dryRun, clearUnknown bool
	cmd := &cobra.Command{
		Use:   "reply <message-id> --body <text>",
		Short: "Post a public reply to an archived Facebook or Instagram comment",
		Long: "Post a public reply to an archived Facebook Page or Instagram comment as the Page or account, then archive the reply. " +
			"The comment's profile must list \"reply\" in actions. --dry-run shows the comment and reply without calling Meta. " +
			"msgvault records each reply before sending it. If Meta's answer is lost, the same reply is refused until you check the comment on Meta and run the command again with --clear-unknown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(body) == "" {
				return opserr.Invalid(errors.New("--body must not be empty"))
			}
			return runCommentCommand(cmd, args, meta.ActionReply, func(c commentCommand) (string, error) {
				switch {
				case dryRun:
					return fmt.Sprintf("Would reply to %s: %s\nReply: %s", c.description, textutil.SanitizeTerminal(c.target.Text), textutil.SanitizeTerminal(body)), nil
				case clearUnknown:
					if err := meta.ClearUnknownReply(cmd.Context(), c.st, c.target, body); err != nil {
						return "", metaSourceStateError(err, c.profile.command)
					}
					return "Cleared the unknown reply to " + c.description + "; run the command again without --clear-unknown to send it", nil
				}
				client, err := c.profile.client()
				if err != nil {
					return "", err
				}
				replyID, err := meta.ReplyToComment(cmd.Context(), c.st, client, c.target, body)
				switch {
				case errors.Is(err, meta.ErrReplySent):
					return "", &sourceCommandError{err: err, code: 6, hint: "change the text to post another reply"}
				case errors.Is(err, meta.ErrReplyUnknown):
					return "", &sourceCommandError{err: err, code: 6, hint: fmt.Sprintf("check the comment on %s; if the reply is not there, run again with --clear-unknown, then send it", c.target.Provider())}
				case err != nil:
					return "", metaSourceStateError(err, c.profile.command)
				}
				return fmt.Sprintf("Replied to %s; reply %s", c.description, textutil.SanitizeTerminal(replyID)), nil
			})
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "reply text")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the comment and reply without calling Meta")
	cmd.Flags().BoolVar(&clearUnknown, "clear-unknown", false, "after checking the comment on Meta, allow a reply with an unknown outcome to be sent again")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "clear-unknown")
	return cmd
}

type commentCommand struct {
	st          *store.Store
	target      meta.CommentTarget
	profile     commentProfile
	description string
}

// runCommentCommand forwards a comment command to the daemon, or, inside the
// daemon, resolves the archived comment and the profile that allows action,
// runs act and prints the line it returns.
func runCommentCommand(cmd *cobra.Command, args []string, action meta.CommentAction, act func(commentCommand) (string, error)) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return opserr.Invalid(errors.New("message ID must be a positive archive message ID"))
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
	line, err := act(commentCommand{st: st, target: target, profile: profile,
		description: fmt.Sprintf("%s comment %s by %s (message %d)", target.Provider(), safe(target.CommentID), safe(target.Author), target.MessageID)})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("write comment result: %w", err)
	}
	return nil
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
