package cmd

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

var (
	showMessageJSON        bool
	showMessageBodyOnly    bool
	showMessageStripQuoted bool
)

var showMessageCmd = &cobra.Command{
	Use:     "show-message <id>",
	Aliases: []string{"show"},
	Short:   "Show full message details",
	Long: `Show the complete details of a message by its internal ID or Gmail ID.

Uses configured remote server or the local daemon by default.
Use --local to use the local daemon even when a remote is configured.

This command displays the full message including headers, body, labels,
and attachment information. Use --json for programmatic output, including
a web_url that opens the message in the selected daemon browser UI.

Use --body-only to print just body text. Add --strip-quoted to remove quoted
lines, complete single-line dated reply headers above them, and "-- " signatures.
Inline replies, uncertain wrapped headers, and complete URLs are preserved.
Only ">" quoting and English "On ... wrote:" headers are recognized; Outlook
"Original Message" blocks and other languages stay as written.
--strip-quoted also cleans body_text in JSON; the archived body and body_html
are unchanged. --body-only and --json cannot be combined.

Examples:
  msgvault show-message 12345
	msgvault show-message 18f0abc123def --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveMessageIDArg(args[0])
		if err != nil {
			return err
		}
		return showHTTPMessage(cmd, id)
	},
}

// resolveMessageIDArg validates a positional message-reference argument for
// commands that accept either an internal numeric ID or a source/Gmail message
// ID. Empty or whitespace-only input, and malformed numeric input such as
// "42.5" or "1e3", are rejected up front with a clear error so the user is not
// misled by a downstream "message not found". Any other non-empty value is
// forwarded unchanged (it may be a Gmail/source ID like "18f0abc123def").
func resolveMessageIDArg(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("invalid message ID: %q (expected a number or Gmail ID)", raw)
	}
	if _, intErr := strconv.ParseInt(trimmed, 10, 64); intErr != nil {
		if _, floatErr := strconv.ParseFloat(trimmed, 64); floatErr == nil {
			return "", fmt.Errorf("invalid message ID: %q (expected a whole number)", trimmed)
		}
	}
	return trimmed, nil
}

func showHTTPMessage(cmd *cobra.Command, idStr string) (result error) {
	s, _, err := OpenHTTPStore(cmd.Context(), daemonclient.AgentReadMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()
	defer reportSuccessfulCLIAction(cmd, s, "message_opened", "", "", &result)

	msg, err := s.GetCLIMessage(cmd.Context(), idStr)
	if errors.Is(err, store.ErrMessageNotFound) {
		return fmt.Errorf("message not found: %s", idStr)
	}
	if err != nil {
		return fmt.Errorf("get message: %w", err)
	}
	if msg == nil {
		return fmt.Errorf("message not found: %s", idStr)
	}

	if showMessageJSON {
		return outputMessageJSON(cmd.OutOrStdout(), msg)
	}
	if msg.HiddenTextPresent {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Warning: this message's HTML may hide text.")
	}
	return outputMessageText(cmd.OutOrStdout(), msg)
}

func outputMessageText(w io.Writer, msg *query.MessageDetail) error {
	output := bufio.NewWriter(w)
	body := messageTextBody(msg, showMessageStripQuoted)
	if showMessageBodyOnly {
		_, _ = fmt.Fprintln(output, textutil.SanitizeTerminalMultiline(body))
		if err := output.Flush(); err != nil {
			return fmt.Errorf("write message: %w", err)
		}
		return nil
	}
	// Header section
	_, _ = fmt.Fprintln(output, "═══════════════════════════════════════════════════════════════════════════════")
	_, _ = fmt.Fprintf(output, "Message ID: %d (Gmail: %s)\n", msg.ID, msg.SourceMessageID)
	if msg.RelatedMessageID != nil {
		_, _ = fmt.Fprintf(output, "Related message: %d (show-message %d)\n", *msg.RelatedMessageID, *msg.RelatedMessageID)
	}
	_, _ = fmt.Fprintln(output, "───────────────────────────────────────────────────────────────────────────────")

	// From
	if len(msg.From) > 0 {
		_, _ = fmt.Fprintf(output, "From:    %s\n", formatAddresses(msg.From))
	}

	// To
	if len(msg.To) > 0 {
		_, _ = fmt.Fprintf(output, "To:      %s\n", formatAddresses(msg.To))
	}

	// CC
	if len(msg.Cc) > 0 {
		_, _ = fmt.Fprintf(output, "Cc:      %s\n", formatAddresses(msg.Cc))
	}

	// BCC
	if len(msg.Bcc) > 0 {
		_, _ = fmt.Fprintf(output, "Bcc:     %s\n", formatAddresses(msg.Bcc))
	}

	// Subject
	_, _ = fmt.Fprintf(output, "Subject: %s\n", msg.Subject)

	// Date
	_, _ = fmt.Fprintf(output, "Date:    %s\n", msg.SentAt.Format(time.RFC1123))

	// Size
	_, _ = fmt.Fprintf(output, "Size:    %s\n", formatSize(msg.SizeEstimate))

	// Labels
	if len(msg.Labels) > 0 {
		_, _ = fmt.Fprintf(output, "Labels:  %s\n", textutil.SanitizeTerminal(strings.Join(msg.Labels, ", ")))
	}
	if msg.DeletedAt != nil {
		_, _ = fmt.Fprintf(output, "Deleted from source: %s\n", msg.DeletedAt.UTC().Format(time.RFC3339))
	}

	// Attachments
	if len(msg.Attachments) > 0 {
		_, _ = fmt.Fprintln(output, "\nAttachments:")
		for _, att := range msg.Attachments {
			if att.URL != "" {
				_, _ = fmt.Fprintf(output, "  • %s (%s, link) %s\n", att.Filename, att.MimeType, att.URL)
			} else {
				_, _ = fmt.Fprintf(output, "  • %s (%s, %s)\n", att.Filename, att.MimeType, formatSize(att.Size))
			}
			for _, ref := range att.Docbank {
				_, _ = fmt.Fprintf(output, "    Docbank node %d (%s)\n", ref.NodeID, textutil.SanitizeTerminal(ref.Endpoint+ref.Folder))
			}
		}
	}

	// Body
	_, _ = fmt.Fprintln(output, "\n═══════════════════════════════════════════════════════════════════════════════")
	if msg.BodyText != "" {
		_, _ = fmt.Fprintln(output, textutil.SanitizeTerminalMultiline(body))
	} else if msg.Snippet != "" {
		_, _ = fmt.Fprintf(output, "[No body text available. Snippet: %s]\n", textutil.SanitizeTerminal(body))
	} else {
		_, _ = fmt.Fprintln(output, "[No body content available]")
	}
	_, _ = fmt.Fprintln(output, "═══════════════════════════════════════════════════════════════════════════════")

	if err := output.Flush(); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	return nil
}

func messageJSONValue(msg *query.MessageDetail) map[string]any {
	// Build address arrays
	fromAddrs := make([]map[string]string, len(msg.From))
	for i, addr := range msg.From {
		fromAddrs[i] = map[string]string{keyEmail: addr.Email, "name": addr.Name}
	}
	toAddrs := make([]map[string]string, len(msg.To))
	for i, addr := range msg.To {
		toAddrs[i] = map[string]string{keyEmail: addr.Email, "name": addr.Name}
	}
	ccAddrs := make([]map[string]string, len(msg.Cc))
	for i, addr := range msg.Cc {
		ccAddrs[i] = map[string]string{keyEmail: addr.Email, "name": addr.Name}
	}
	bccAddrs := make([]map[string]string, len(msg.Bcc))
	for i, addr := range msg.Bcc {
		bccAddrs[i] = map[string]string{keyEmail: addr.Email, "name": addr.Name}
	}

	// Build attachment array
	attachments := make([]map[string]any, len(msg.Attachments))
	for i, att := range msg.Attachments {
		attachments[i] = map[string]any{
			"id":           att.ID,
			"filename":     att.Filename,
			"mime_type":    att.MimeType,
			"size":         att.Size,
			"content_hash": att.ContentHash,
		}
		if len(att.Docbank) > 0 {
			attachments[i]["docbank"] = att.Docbank
		}
		if att.URL != "" {
			attachments[i]["url"] = att.URL
		}
	}

	output := map[string]any{
		"id":                     msg.ID,
		"source_message_id":      msg.SourceMessageID,
		"rfc822_message_id":      msg.RFC822MessageID,
		"conversation_id":        msg.ConversationID,
		"source_conversation_id": msg.SourceConversationID,
		"subject":                msg.Subject,
		"snippet":                msg.Snippet,
		"sent_at":                msg.SentAt.Format(time.RFC3339),
		"size_estimate":          msg.SizeEstimate,
		"has_attachments":        msg.HasAttachments,
		"from":                   fromAddrs,
		"to":                     toAddrs,
		"cc":                     ccAddrs,
		"bcc":                    bccAddrs,
		"labels":                 msg.Labels,
		"attachments":            attachments,
		"body_text":              msg.BodyText,
		"body_html":              msg.BodyHTML,
		"hidden_text_present":    msg.HiddenTextPresent,
	}

	if msg.RelatedMessageID != nil {
		output["related_message_id"] = *msg.RelatedMessageID
	}
	if msg.WebURL != "" {
		output["web_url"] = msg.WebURL
	}
	if msg.ReceivedAt != nil {
		output["received_at"] = msg.ReceivedAt.Format(time.RFC3339)
	}
	if msg.DeletedAt != nil {
		output["deleted_from_source_at"] = msg.DeletedAt.UTC().Format(time.RFC3339)
	}

	return output
}

func outputMessageJSON(w io.Writer, msg *query.MessageDetail) error {
	if showMessageStripQuoted {
		msg = readableMessageDetail(msg)
	}
	enc := jsontext.NewEncoder(w, jsontext.WithIndent("  "))
	return json.MarshalEncode(enc, messageJSONValue(msg), json.Deterministic(true))
}

func formatAddresses(addrs []query.Address) string {
	parts := make([]string, len(addrs))
	for i, addr := range addrs {
		if addr.Name != "" {
			parts[i] = fmt.Sprintf("%s <%s>", addr.Name, addr.Email)
		} else {
			parts[i] = addr.Email
		}
	}
	return strings.Join(parts, ", ")
}

func init() {
	rootCmd.AddCommand(showMessageCmd)
	showMessageCmd.Flags().BoolVar(&showMessageJSON, flagJSON, false, "Output as JSON")
	showMessageCmd.Flags().BoolVar(&showMessageBodyOnly, "body-only", false, "Print only the text body")
	showMessageCmd.Flags().BoolVar(&showMessageStripQuoted, "strip-quoted", false, "Remove quoted history and conventional signatures")
	showMessageCmd.MarkFlagsMutuallyExclusive("body-only", flagJSON)
}
