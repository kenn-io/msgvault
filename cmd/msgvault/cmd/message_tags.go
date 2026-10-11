package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/textutil"
)

type messageTagBackend interface {
	SupportsAPISchemaVersion(ctx context.Context, version string) (bool, error)
	MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error)
}

func newMessageTagsCommand(backend messageTagBackend) *cobra.Command {
	var change emailtags.MessageTagChange
	var asJSON bool
	cmd := &cobra.Command{
		Use: "message-tags <id>", Short: "Read or update native email tags", Args: cobra.ExactArgs(1),
		Long: `Read tags from the provider or add and remove them on one archived message.
Gmail edits use existing user label IDs returned in available_tags. IMAP edits
use custom keyword atoms. Microsoft Graph edits use category names.
Use --mailbox to select a recorded IMAP mailbox copy.
Use --dry-run with an edit to preview it without writing.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return errors.New("message ID must be a positive archived message ID")
			}
			var input *emailtags.MessageTagChange
			if len(change.Add)+len(change.Remove) > 0 {
				input = &change
			} else if change.DryRun {
				return errors.New("--dry-run requires --add or --remove")
			}
			client := backend
			if client == nil {
				st, _, err := OpenHTTPStore(cmd.Context())
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
				client = st
			}
			supported, err := client.SupportsAPISchemaVersion(cmd.Context(), messageTagsMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("check daemon native tag support: %w", err)
			}
			if !supported {
				return fmt.Errorf("this daemon is too old for native email tags; upgrade it to API schema %s or newer", messageTagsMinAPISchemaVersion)
			}
			result, err := client.MessageTags(cmd.Context(), id, input, change.Mailbox)
			if asJSON {
				var value any = result
				if err != nil {
					if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
						value = failure
					} else {
						value = emailtags.Failure("request_failed", "Cannot read or update provider tags", result, nil)
					}
				}
				data, encodeErr := json.Marshal(value)
				if encodeErr != nil {
					return encodeErr
				}
				if _, writeErr := fmt.Fprintln(cmd.OutOrStdout(), string(data)); writeErr != nil {
					return fmt.Errorf("write message tags JSON: %w", writeErr)
				}
			} else if result != nil {
				verb := "Tags"
				if err != nil {
					verb = "Last observed tags"
				} else if result.DryRun {
					verb = "Preview tags"
				}
				if _, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", verb, textutil.SanitizeTerminal(strings.Join(result.Tags, ", "))); writeErr != nil {
					return fmt.Errorf("write message tags: %w", writeErr)
				}
				if input == nil {
					for _, tag := range result.AvailableTags {
						if _, writeErr := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", textutil.SanitizeTerminal(tag.ID), textutil.SanitizeTerminal(tag.Name)); writeErr != nil {
							return fmt.Errorf("write available tag: %w", writeErr)
						}
					}
				}
			}
			return err
		},
	}
	cmd.Flags().StringArrayVar(&change.Add, "add", nil, "Add native tag (repeatable)")
	cmd.Flags().StringArrayVar(&change.Remove, "remove", nil, "Remove native tag (repeatable)")
	cmd.Flags().StringVar(&change.Mailbox, "mailbox", "", "Exact recorded IMAP mailbox")
	cmd.Flags().BoolVar(&change.DryRun, "dry-run", false, "Preview changes without writing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output tags or partial error as JSON")
	return cmd
}
func init() { rootCmd.AddCommand(newMessageTagsCommand(nil)) }
