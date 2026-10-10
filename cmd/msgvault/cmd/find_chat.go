package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newFindChatCommand() *cobra.Command {
	var limit int
	var sourceID int64
	var asJSON bool
	command := &cobra.Command{
		Use:   "find-chat <name or title>",
		Short: "Find archived chats by participant name or title",
		Long: `Find archived chats whose participant names or titles contain any whole
query word. Exact matches rank first, then most matching words, then recent
activity. JSON results have conversation_id, message_id, source_id, network,
title, matched_names, and match_kind; pass message_id to show-message.
Results are candidates: read the messages before linking identities.`,
		Example: `  msgvault find-chat "Alice Example" --json
  msgvault find-chat alice --source-id 3 --limit 20`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := store.ChatDiscoveryQuery{Query: strings.Join(args, " "), Limit: limit, SourceID: sourceID}
			if limit == 0 {
				return usageErr(cmd, errors.New("--limit must be between 1 and 100"))
			}
			if cmd.Flags().Changed("source-id") && sourceID == 0 {
				return usageErr(cmd, errors.New("--source-id must be positive"))
			}
			if err := store.ValidateChatDiscoveryQuery(q); err != nil {
				return usageErr(cmd, err)
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			page, err := client.SearchChats(cmd.Context(), q)
			if err != nil {
				return err
			}
			if asJSON {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), page, json.Deterministic(true))
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "CONVERSATION\tMESSAGE\tSOURCE\tNETWORK\tPROVIDER CHAT\tMATCH\tTITLE\tMATCHED")
			clean := func(value string) string { return strings.ReplaceAll(textutil.SanitizeTerminal(value), "\t", "\\t") }
			for _, result := range page.Results {
				matched := strings.Join(result.MatchedNames, ", ")
				if result.EvidenceTruncated {
					matched += " …"
				}
				_, _ = fmt.Fprintf(writer, "%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", result.ConversationID, result.MessageID,
					result.SourceID, clean(result.Network), clean(result.SourceConversationID),
					result.MatchKind, clean(result.Title), clean(matched))
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("write chat discovery results: %w", err)
			}
			if page.HasMore {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), "More matches available; use --limit up to 100 or narrow the name or --source-id.")
				if err != nil {
					return fmt.Errorf("write chat discovery pagination hint: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", store.DefaultChatDiscoveryLimit, "Maximum results (1..100)")
	command.Flags().Int64Var(&sourceID, "source-id", 0, "Restrict to one archived source")
	command.Flags().BoolVar(&asJSON, flagJSON, false, "Output structured JSON")
	return command
}

func init() {
	rootCmd.AddCommand(newFindChatCommand())
}
