package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

var (
	showMessageJSON bool
)

var showMessageCmd = &cobra.Command{
	Use:   "show-message <id>",
	Short: "Show full message details",
	Long: `Show the complete details of a message by its internal ID or Gmail ID.

Uses configured remote server or the local daemon by default.
Use --local to use the local daemon even when a remote is configured.

This command displays the full message including headers, body, labels,
and attachment information. Use --json for programmatic output.

Examples:
  msgvault show-message 12345
	msgvault show-message 18f0abc123def --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return showHTTPMessage(cmd, args[0])
	},
}

func showHTTPMessage(cmd *cobra.Command, idStr string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

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
		return outputMessageJSON(msg)
	}
	return outputMessageText(msg)
}

// nil error return mirrors outputMessageJSON so callers can return either
// uniformly; text printing never fails.
//
//nolint:unparam // symmetry with error-returning outputMessageJSON sibling
func outputMessageText(msg *query.MessageDetail) error {
	// Header section
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")
	fmt.Printf("Message ID: %d (Gmail: %s)\n", msg.ID, msg.SourceMessageID)
	fmt.Println("───────────────────────────────────────────────────────────────────────────────")

	// From
	if len(msg.From) > 0 {
		fmt.Printf("From:    %s\n", formatAddresses(msg.From))
	}

	// To
	if len(msg.To) > 0 {
		fmt.Printf("To:      %s\n", formatAddresses(msg.To))
	}

	// CC
	if len(msg.Cc) > 0 {
		fmt.Printf("Cc:      %s\n", formatAddresses(msg.Cc))
	}

	// BCC
	if len(msg.Bcc) > 0 {
		fmt.Printf("Bcc:     %s\n", formatAddresses(msg.Bcc))
	}

	// Subject
	fmt.Printf("Subject: %s\n", msg.Subject)

	// Date
	fmt.Printf("Date:    %s\n", msg.SentAt.Format(time.RFC1123))

	// Size
	fmt.Printf("Size:    %s\n", formatSize(msg.SizeEstimate))

	// Labels
	if len(msg.Labels) > 0 {
		fmt.Printf("Labels:  %s\n", strings.Join(msg.Labels, ", "))
	}

	// Attachments
	if len(msg.Attachments) > 0 {
		fmt.Println("\nAttachments:")
		for _, att := range msg.Attachments {
			if att.URL != "" {
				fmt.Printf("  • %s (%s, link) %s\n", att.Filename, att.MimeType, att.URL)
			} else {
				fmt.Printf("  • %s (%s, %s)\n", att.Filename, att.MimeType, formatSize(att.Size))
			}
		}
	}

	// Body
	fmt.Println("\n═══════════════════════════════════════════════════════════════════════════════")
	if msg.BodyText != "" {
		fmt.Println(msg.BodyText)
	} else if msg.Snippet != "" {
		fmt.Printf("[No body text available. Snippet: %s]\n", msg.Snippet)
	} else {
		fmt.Println("[No body content available]")
	}
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")

	return nil
}

func outputMessageJSON(msg *query.MessageDetail) error {
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
		if att.URL != "" {
			attachments[i]["url"] = att.URL
		}
	}

	output := map[string]any{
		"id":                     msg.ID,
		"source_message_id":      msg.SourceMessageID,
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
	}

	if msg.ReceivedAt != nil {
		output["received_at"] = msg.ReceivedAt.Format(time.RFC3339)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
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
}
