package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
	"golang.org/x/sync/errgroup"
)

func newShowThreadCmd() *cobra.Command {
	var limit, offset int
	var asJSON, stripQuoted bool
	cmd := &cobra.Command{
		Use: "show-thread <id>", Short: "Show a message's thread in chronological order",
		Long: `Show the thread containing an internal message ID or provider message ID.
Messages appear oldest first, with undated messages last. Quoted lines, complete
single-line dated reply headers above them, and "-- " signatures are removed.
Inline replies, uncertain wrapped headers, and complete URLs are preserved.
Only ">" quoting and English "On ... wrote:" headers are recognized; Outlook
"Original Message" blocks and other languages stay as written.
Text and JSON use the same --strip-quoted policy.
Use --strip-quoted=false to print the archived body text unchanged.
The default page contains at most 100 messages; --limit accepts 1–500.
Use --offset to read another page. JSON includes conversation_id, total, offset,
has_more, and messages. An exhausted offset returns an empty page.
Provider IDs that match several accounts require an internal message ID.

Examples:
  msgvault show-thread 123
  msgvault show-thread 123 --json --limit 50 --offset 50`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > query.ThreadMaxLimit {
				return usageErr(cmd, errors.New("--limit must be between 1 and 500"))
			}
			if offset < 0 {
				return usageErr(cmd, errors.New("--offset must be non-negative"))
			}
			id, err := resolveMessageIDArg(args[0])
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context(), daemonclient.AgentReadMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = client.Close() }()
			engine := daemonclient.NewEngineAdapter(client)
			request := query.ThreadQuery{Limit: limit, Offset: offset}
			numeric, parseErr := strconv.ParseInt(id, 10, 64)
			if parseErr == nil && numeric > 0 {
				request.ID = numeric
			} else {
				request.SourceMessageID = id
			}
			page, err := engine.ListThread(cmd.Context(), request)
			if request.ID != 0 && errors.Is(err, store.ErrMessageNotFound) {
				request.ID = 0
				request.SourceMessageID = id
				page, err = engine.ListThread(cmd.Context(), request)
			}
			if errors.Is(err, query.ErrAmbiguousReference) {
				return errors.New("provider message ID is ambiguous; use an internal message ID")
			}
			if err != nil {
				return fmt.Errorf("get thread: %w", err)
			}
			messages, err := loadThreadMessages(cmd.Context(), client, page)
			if err != nil {
				return err
			}
			if page.HasMore {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "More messages: use --offset %d\n", page.Offset+len(page.Messages))
			}
			if asJSON {
				values := make([]map[string]any, len(messages))
				for i, msg := range messages {
					if stripQuoted {
						msg = readableMessageDetail(msg)
					}
					values[i] = messageJSONValue(msg)
				}
				enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
				return json.MarshalEncode(enc, map[string]any{"conversation_id": page.ConversationID, "total": page.Total, "offset": page.Offset, "has_more": page.HasMore, "messages": values}, json.Deterministic(true))
			}
			fmt.Printf("Thread %d (%d messages, offset %d)\n", page.ConversationID, page.Total, page.Offset)
			for _, msg := range messages {
				date := "undated"
				if !msg.SentAt.IsZero() {
					date = msg.SentAt.Format("2006-01-02 15:04:05Z07:00")
				}
				fmt.Printf("\nMessage %d · %s · %s\n%s\n", msg.ID, date, textutil.SanitizeTerminal(formatAddresses(msg.From)), textutil.SanitizeTerminal(msg.Subject))
				fmt.Println(textutil.SanitizeTerminalMultiline(messageTextBody(msg, stripQuoted)))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", query.ThreadDefaultLimit, "Maximum messages to show (1–500)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Skip this many chronological messages")
	cmd.Flags().BoolVar(&asJSON, flagJSON, false, "Output a thread page as JSON")
	cmd.Flags().BoolVar(&stripQuoted, "strip-quoted", true, "Remove quoted history and conventional signatures")
	return cmd
}

func init() { rootCmd.AddCommand(newShowThreadCmd()) }

func loadThreadMessages(ctx context.Context, client *daemonclient.Client, page *query.ThreadPage) ([]*query.MessageDetail, error) {
	messages := make([]*query.MessageDetail, len(page.Messages))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for i, listed := range page.Messages {
		group.Go(func() error {
			detail, err := client.GetCLIMessage(ctx, strconv.FormatInt(listed.ID, 10))
			if err != nil {
				return fmt.Errorf("get thread message %d: %w", listed.ID, err)
			}
			if detail.ID != listed.ID {
				return fmt.Errorf("thread message %d changed during retrieval", listed.ID)
			}
			if detail.ConversationID != page.ConversationID {
				return fmt.Errorf("thread message %d changed conversation during retrieval", listed.ID)
			}
			messages[i] = detail
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("read thread page: %w", err)
	}
	return messages, nil
}
