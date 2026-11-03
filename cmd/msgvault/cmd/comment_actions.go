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
	SupportsAPISchemaVersion(ctx context.Context, version string) (bool, error)
	RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest) (*meta.ActionResult, error)
	ListCommentActions(ctx context.Context, id int64) ([]meta.Receipt, error)
	UpdateCommentTriage(ctx context.Context, id int64, state, expect string) (string, error)
	ListCommentQueue(ctx context.Context, req api.CommentQueueRequest) (*api.CommentQueueResponse, error)
}

func newCommentCommand(name string, backend commentActionBackend) *cobra.Command {
	var req meta.ActionRequest
	var filter api.CommentQueueRequest
	var hide, unhide, remove, asJSON bool
	var state, expect, visibility string
	descriptions := map[string]string{"reply": "Reply publicly or privately to an archived Facebook comment", "moderate": "Hide, unhide or delete an archived Facebook comment", "triage": "Set a comment's local triage state", "queue": "List archived comments by triage state", "actions": "Read action receipts for an archived comment"}
	cmd := &cobra.Command{Use: name + " <message-id>", Short: descriptions[name], Args: func(cmd *cobra.Command, args []string) error {
		return commentUsageError(cmd, cobra.ExactArgs(1)(cmd, args))
	}, SilenceErrors: true, SilenceUsage: true}
	if name == "queue" {
		cmd.Use = name
		cmd.Args = func(cmd *cobra.Command, args []string) error { return commentUsageError(cmd, cobra.NoArgs(cmd, args)) }
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() {
			if runErr != nil {
				runErr = classifyCommentCLIError(cmd, runErr)
			}
		}()
		var id int64
		if name != "queue" {
			var err error
			id, err = strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return newCommentCLIError(cmd, "validation", 3, errors.New("message ID must be a positive archived message ID"))
			}
		}
		switch name {
		case "moderate":
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
				return commentUsageError(cmd, errors.New("choose exactly one of --hide, --unhide or --delete"))
			}
			if req.ResolveUnknown != "" {
				if !slices.Contains([]string{"sent", "not-sent"}, req.ResolveUnknown) {
					return newCommentCLIError(cmd, "validation", 3, errors.New("--resolve-unknown must be sent or not-sent"))
				}
				if req.DryRun {
					return commentUsageError(cmd, errors.New("--resolve-unknown cannot be combined with --dry-run"))
				}
			}
		case "reply":
			if visibility == "" {
				return commentUsageError(cmd, errors.New("--visibility is required"))
			}
			if !slices.Contains([]string{"public", "private"}, visibility) {
				return newCommentCLIError(cmd, "validation", 3, errors.New("--visibility must be public or private"))
			}
			if req.Body == "" {
				return commentUsageError(cmd, errors.New("--body is required"))
			}
			req.Action = meta.ActionKind(visibility + "_reply")
			if req.ResolveUnknown != "" {
				if !slices.Contains([]string{"sent", "not-sent"}, req.ResolveUnknown) {
					return newCommentCLIError(cmd, "validation", 3, errors.New("--resolve-unknown must be sent or not-sent"))
				}
				if req.DryRun {
					return commentUsageError(cmd, errors.New("--resolve-unknown cannot be combined with --dry-run"))
				}
			}
		case "triage":
			if state == "" {
				return commentUsageError(cmd, errors.New("--state is required"))
			}
			valid := func(v string) bool { return slices.Contains([]string{"open", "handled", "skipped"}, v) }
			if !valid(state) || (expect != "" && !valid(expect)) {
				return newCommentCLIError(cmd, "validation", 3, errors.New("--state and --expect must be open, handled or skipped"))
			}
		case "queue":
			if filter.Limit < 1 {
				return newCommentCLIError(cmd, "validation", 3, errors.New("--limit must be 1–200"))
			}
			if err := api.ValidateCommentQueueRequest(filter); err != nil {
				return newCommentCLIError(cmd, "validation", 3, err)
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
			return newCommentCLIError(cmd, "not_initialized", 5, fmt.Errorf("this daemon is too old for comment actions; upgrade it to API schema %s or newer", commentActionsMinAPISchemaVersion))
		}
		var result any
		switch name {
		case "moderate", "reply":
			result, err = client.RunCommentAction(cmd.Context(), id, req)
		case "triage":
			var got string
			got, err = client.UpdateCommentTriage(cmd.Context(), id, state, expect)
			if err == nil {
				result = api.CommentTriageResponse{State: got}
			}
		case "queue":
			result, err = client.ListCommentQueue(cmd.Context(), filter)
		case "actions":
			result, err = client.ListCommentActions(cmd.Context(), id)
		}
		return printCommentResult(cmd, result, asJSON, err)
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result or coded partial error as JSON")
	switch name {
	case "moderate":
		cmd.Flags().BoolVar(&hide, "hide", false, "")
		cmd.Flags().BoolVar(&unhide, "unhide", false, "")
		cmd.Flags().BoolVar(&remove, "delete", false, "Delete from Facebook; keep archived history")
	case "reply":
		cmd.Flags().StringVar(&visibility, "visibility", "", "public or private (required)")
		cmd.Flags().StringVar(&req.Body, "body", "", "")
	}
	if name == "moderate" || name == "reply" {
		cmd.Flags().BoolVar(&req.DryRun, "dry-run", false, "Preview locally and print the confirmation token")
		cmd.Flags().StringVar(&req.Confirm, "confirm", "", "Confirmation token from a dry run")
		cmd.Flags().StringVar(&req.ResolveUnknown, "resolve-unknown", "", "Resolve an uncertain action as sent or not-sent after inspection")
	}
	switch name {
	case "triage":
		cmd.Flags().StringVar(&state, "state", "", "Local state: open, handled or skipped")
		cmd.Flags().StringVar(&expect, "expect", "", "Require this current triage state")
	case "queue":
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
			failure := classifyCommentCLIError(cmd, callErr)
			failure.details = result
			return failure
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
			return fmt.Errorf("write comment result: %w", err)
		}
	} else {
		var out strings.Builder
		safe := textutil.SanitizeTerminal
		switch value := result.(type) {
		case *meta.ActionResult:
			if value != nil {
				fmt.Fprintf(&out, "Comment by %s on %s\n", safe(value.Disclosure.AuthorName), safe(value.Disclosure.AssetName))
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
					fmt.Fprintf(&out, "Receipt: %s\n", safe(value.Receipt.State))
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
				fmt.Fprintf(&out, "%s\t%s\t%s\n", safe(string(row.Action)), safe(row.State), safe(row.ErrorCode))
			}
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), out.String()); err != nil {
			return fmt.Errorf("write comment result: %w", err)
		}
	}
	return callErr
}
func newCommentGroup(backend commentActionBackend) *cobra.Command {
	group := &cobra.Command{Use: "comment", Args: cobra.ArbitraryArgs, Annotations: map[string]string{helpOnlyGroupAnnotation: strconv.FormatBool(true)}, Short: "Reply to, moderate and triage archived comments", SilenceErrors: true, SilenceUsage: true}
	group.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		_ = cmd.Usage()
		return commentUsageError(cmd, fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	group.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if strings.HasPrefix(err.Error(), "invalid argument ") || strings.HasPrefix(err.Error(), "invalid boolean value ") {
			return newCommentCLIError(cmd, "validation", 3, err)
		}
		return commentUsageError(cmd, err)
	})
	for _, name := range []string{"reply", "moderate", "triage", "queue", "actions"} {
		group.AddCommand(newCommentCommand(name, backend))
	}
	return group
}

func init() { rootCmd.AddCommand(newCommentGroup(nil)) }
