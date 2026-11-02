package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/textutil"
)

type commentActionBackend interface {
	SupportsAPISchemaVersion(context.Context, string) (bool, error)
	RunCommentAction(context.Context, int64, meta.ActionRequest) (*meta.ActionResult, error)
	ListCommentActions(context.Context, int64) ([]meta.Receipt, error)
	UpdateCommentTriage(context.Context, int64, string, string) (string, error)
	ListCommentQueue(context.Context, api.CommentQueueRequest) (*api.CommentQueueResponse, error)
}

func newCommentCommand(name string, backend commentActionBackend) *cobra.Command {
	var req meta.ActionRequest
	var filter api.CommentQueueRequest
	var hide, unhide, remove, asJSON bool
	var state, expect, visibility string
	descriptions := map[string]string{"comment-reply": "Reply publicly or privately to an archived Facebook comment", "comment-moderate": "Hide, unhide or delete an archived Facebook comment", "comment-triage": "Set a comment's local triage state", "comment-queue": "List archived comments by triage state", "comment-actions": "Read action receipts for an archived comment"}
	cmd := &cobra.Command{Use: name + " <message-id>", Short: descriptions[name], Args: cobra.ExactArgs(1)}
	if name == "comment-queue" {
		cmd.Use = name
		cmd.Args = cobra.NoArgs
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var id int64
		if name != "comment-queue" {
			var err error
			id, err = strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return errors.New("message ID must be a positive archived message ID")
			}
		}
		switch name {
		case "comment-moderate":
			selected := 0
			for _, choice := range []struct {
				set  bool
				kind meta.ActionKind
			}{{hide, meta.ActionHide}, {unhide, meta.ActionUnhide}, {remove, meta.ActionDelete}} {
				if choice.set {
					selected++
					req.Action = choice.kind
				}
			}
			if selected != 1 {
				return errors.New("choose exactly one of --hide, --unhide or --delete")
			}
			if req.ResolveUnknown != "" && (!slices.Contains([]string{"sent", "not-sent"}, req.ResolveUnknown) || req.DryRun) {
				return errors.New("--resolve-unknown must be sent or not-sent and cannot be combined with --dry-run")
			}
		case "comment-reply":
			if !slices.Contains([]string{"public", "private"}, visibility) {
				return errors.New("--visibility is required and must be public or private")
			}
			if req.Body == "" {
				return errors.New("--body is required")
			}
			req.Action = meta.ActionKind(visibility + "_reply")
			if req.ResolveUnknown != "" && (!slices.Contains([]string{"sent", "not-sent"}, req.ResolveUnknown) || req.DryRun) {
				return errors.New("--resolve-unknown must be sent or not-sent and cannot be combined with --dry-run")
			}
		case "comment-triage":
			valid := func(v string) bool { return slices.Contains([]string{"open", "handled", "skipped"}, v) }
			if !valid(state) || (expect != "" && !valid(expect)) {
				return errors.New("--state and --expect must be open, handled or skipped")
			}
		case "comment-queue":
			if filter.Limit < 1 {
				return errors.New("--limit must be 1–200")
			}
			if err := api.ValidateCommentQueueRequest(filter); err != nil {
				return err
			}
		}
		cmd.SilenceUsage = true
		client := backend
		if client == nil {
			st, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			client = st
		}
		supported, err := client.SupportsAPISchemaVersion(cmd.Context(), commentActionsMinAPISchemaVersion)
		if err != nil {
			return fmt.Errorf("check daemon comment actions support: %w", err)
		}
		if !supported {
			return fmt.Errorf("this daemon is too old for comment actions; upgrade it to API schema %s or newer", commentActionsMinAPISchemaVersion)
		}
		var result any
		switch name {
		case "comment-moderate", "comment-reply":
			result, err = client.RunCommentAction(cmd.Context(), id, req)
		case "comment-triage":
			var got string
			got, err = client.UpdateCommentTriage(cmd.Context(), id, state, expect)
			result = api.CommentTriageResponse{State: got}
		case "comment-queue":
			result, err = client.ListCommentQueue(cmd.Context(), filter)
		case "comment-actions":
			result, err = client.ListCommentActions(cmd.Context(), id)
		}
		return printCommentResult(cmd, result, asJSON, err)
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result or coded partial error as JSON")
	switch name {
	case "comment-moderate":
		cmd.Flags().BoolVar(&hide, "hide", false, "Hide the comment")
		cmd.Flags().BoolVar(&unhide, "unhide", false, "Unhide the comment")
		cmd.Flags().BoolVar(&remove, "delete", false, "Delete from Facebook; keep archived history")
	case "comment-reply":
		cmd.Flags().StringVar(&visibility, "visibility", "", "Required reply visibility: public or private")
		cmd.Flags().StringVar(&req.Body, "body", "", "Exact reply text")
	}
	if name == "comment-moderate" || name == "comment-reply" {
		cmd.Flags().BoolVar(&req.DryRun, "dry-run", false, "Preview locally and print the confirmation token")
		cmd.Flags().StringVar(&req.Confirm, "confirm", "", "Confirmation token from a dry run")
		cmd.Flags().StringVar(&req.ResolveUnknown, "resolve-unknown", "", "Resolve an uncertain action as sent or not-sent after inspection")
	}
	switch name {
	case "comment-triage":
		cmd.Flags().StringVar(&state, "state", "", "Local state: open, handled or skipped")
		cmd.Flags().StringVar(&expect, "expect", "", "Require this current triage state")
	case "comment-queue":
		cmd.Flags().StringVar(&filter.Source, "source", "", "Exact archive source identifier")
		cmd.Flags().StringVar(&filter.State, "state", "open", "open, handled, skipped or all")
		cmd.Flags().IntVar(&filter.Limit, "limit", 50, "Maximum comments (1–200)")
		cmd.Flags().Int64Var(&filter.BeforeID, "before-id", 0, "Last-seen message ID for the next page")
	}
	return cmd
}
func printCommentResult(cmd *cobra.Command, result any, asJSON bool, callErr error) error {
	if asJSON {
		value := result
		if callErr != nil {
			if failure, ok := errors.AsType[*meta.ActionError](callErr); ok {
				value = failure
			} else {
				value = &meta.ActionError{Code: "request_failed", Message: "Cannot access comment actions"}
			}
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
			return err
		}
	} else {
		var out strings.Builder
		safe := textutil.SanitizeTerminal
		switch value := result.(type) {
		case *meta.ActionResult:
			if value != nil {
				fmt.Fprintf(&out, "Comment %s by %s on %s\n", safe(value.Disclosure.CommentID), safe(value.Disclosure.AuthorName), safe(value.Disclosure.AssetName))
				if value.Visibility != "" {
					fmt.Fprintf(&out, "%s reply: %s\n", safe(value.Visibility), safe(value.Disclosure.Body))
				}
				for _, warning := range value.Warnings {
					if warning == "prior_private_reply_to_author" {
						warning = "A private reply to this author was already recorded in the past seven days."
					}
					fmt.Fprintf(&out, "Warning: %s\n", safe(warning))
				}
				if value.DryRun {
					fmt.Fprintf(&out, "Confirm: %s\n", safe(value.Confirm))
				}
				if value.Receipt != nil {
					fmt.Fprintf(&out, "Receipt %d: %s\n", value.Receipt.ID, safe(value.Receipt.State))
				}
			}
		case api.CommentTriageResponse:
			fmt.Fprintf(&out, "Triage: %s\n", safe(value.State))
		case *api.CommentQueueResponse:
			if value != nil {
				out.WriteString("MESSAGE\tTRIAGE\tAUTHOR\tCOMMENT\n")
				for _, item := range value.Items {
					fmt.Fprintf(&out, "%d\t%s\t%s\t%s\n", item.MessageID, safe(item.Triage), safe(item.Author.DisplayName), safe(item.Snippet))
				}
				if value.NextBeforeID != 0 {
					fmt.Fprintf(&out, "Next: --before-id %d\n", value.NextBeforeID)
				}
			}
		case []meta.Receipt:
			for _, row := range value {
				fmt.Fprintf(&out, "%d\t%s\t%s\t%s\n", row.ID, safe(string(row.Action)), safe(row.State), safe(row.ErrorCode))
			}
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), out.String()); err != nil {
			return err
		}
	}
	return callErr
}
func init() {
	for _, name := range []string{"comment-moderate", "comment-reply", "comment-triage", "comment-queue", "comment-actions"} {
		rootCmd.AddCommand(newCommentCommand(name, nil))
	}
}
