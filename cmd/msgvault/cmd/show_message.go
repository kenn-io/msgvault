package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var (
	showMessageJSON bool
)

var showMessageCmd = &cobra.Command{
	Use:   "show-message <id>",
	Short: "Show full message details",
	Long: `Show the complete details of a message by its internal ID or Gmail ID.

Uses remote server if [remote].url is configured, otherwise uses local database.
Use --local to force local database.

This command displays the full message including headers, body, labels,
and attachment information. Use --json for programmatic output.

Examples:
  msgvault show-message 12345
  msgvault show-message 18f0abc123def --json`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		idStr := args[0]

		// Use remote if configured
		if IsRemoteMode() {
			return showRemoteMessage(idStr)
		}

		return showLocalMessage(cmd, idStr)
	},
}

// showRemoteMessage fetches and displays a message from the remote server.
func showRemoteMessage(idStr string) error {
	// Parse as numeric ID (remote API only supports numeric IDs)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return fmt.Errorf("remote mode requires numeric message ID (got: %s)", idStr)
	}

	s, err := OpenRemoteStore()
	if err != nil {
		return fmt.Errorf("connect to remote: %w", err)
	}
	defer func() { _ = s.Close() }()

	msg, err := s.GetMessage(id)
	if err != nil {
		return fmt.Errorf("get message: %w", err)
	}
	if msg == nil {
		return fmt.Errorf("message not found: %s", idStr)
	}

	if showMessageJSON {
		return outputRemoteMessageJSON(msg)
	}
	return outputRemoteMessageText(msg)
}

// showLocalMessage fetches and displays a message from the local database.
func showLocalMessage(cmd *cobra.Command, idStr string) error {
	// Open database
	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		return fmt.Errorf("startup migrations: %w", err)
	}

	// Create query engine
	engine := query.NewSQLiteEngine(s.DB())

	// Try to parse as numeric ID first
	var msg *query.MessageDetail
	if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
		msg, err = engine.GetMessage(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
	}

	// If not found or not numeric, try as source message ID (Gmail ID)
	if msg == nil {
		var err error
		msg, err = engine.GetMessageBySourceID(cmd.Context(), idStr)
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
	}

	if msg == nil {
		return fmt.Errorf("message not found: %s", idStr)
	}

	if showMessageJSON {
		return outputMessageJSON(msg)
	}
	return outputMessageText(msg)
}

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
			fmt.Printf("  • %s (%s, %s)\n", att.Filename, att.MimeType, formatSize(att.Size))
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
		fromAddrs[i] = map[string]string{"email": addr.Email, "name": addr.Name}
	}
	toAddrs := make([]map[string]string, len(msg.To))
	for i, addr := range msg.To {
		toAddrs[i] = map[string]string{"email": addr.Email, "name": addr.Name}
	}
	ccAddrs := make([]map[string]string, len(msg.Cc))
	for i, addr := range msg.Cc {
		ccAddrs[i] = map[string]string{"email": addr.Email, "name": addr.Name}
	}
	bccAddrs := make([]map[string]string, len(msg.Bcc))
	for i, addr := range msg.Bcc {
		bccAddrs[i] = map[string]string{"email": addr.Email, "name": addr.Name}
	}

	// Build attachment array
	attachments := make([]map[string]interface{}, len(msg.Attachments))
	for i, att := range msg.Attachments {
		attachments[i] = map[string]interface{}{
			"id":           att.ID,
			"filename":     att.Filename,
			"mime_type":    att.MimeType,
			"size":         att.Size,
			"content_hash": att.ContentHash,
		}
	}

	output := map[string]interface{}{
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

// outputRemoteMessageText displays a message from the remote API.
func outputRemoteMessageText(msg *store.APIMessage) error {
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")
	fmt.Printf("Message ID: %d\n", msg.ID)
	fmt.Println("───────────────────────────────────────────────────────────────────────────────")

	// From
	if msg.From != "" {
		fmt.Printf("From:    %s\n", msg.From)
	}

	// To
	if len(msg.To) > 0 {
		fmt.Printf("To:      %s\n", strings.Join(msg.To, ", "))
	}

	// Subject
	fmt.Printf("Subject: %s\n", msg.Subject)

	// Date
	if !msg.SentAt.IsZero() {
		fmt.Printf("Date:    %s\n", msg.SentAt.Format(time.RFC1123))
	}

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
			fmt.Printf("  • %s (%s, %s)\n", att.Filename, att.MimeType, formatSize(att.Size))
		}
	}

	// Body
	fmt.Println("\n═══════════════════════════════════════════════════════════════════════════════")
	if msg.Body != "" {
		fmt.Println(msg.Body)
	} else if msg.Snippet != "" {
		fmt.Printf("[No body text available. Snippet: %s]\n", msg.Snippet)
	} else {
		fmt.Println("[No body content available]")
	}
	fmt.Println("═══════════════════════════════════════════════════════════════════════════════")

	return nil
}

// outputRemoteMessageJSON outputs a remote message as JSON.
func outputRemoteMessageJSON(msg *store.APIMessage) error {
	// Build attachment array
	attachments := make([]map[string]interface{}, len(msg.Attachments))
	for i, att := range msg.Attachments {
		attachments[i] = map[string]interface{}{
			"filename":  att.Filename,
			"mime_type": att.MimeType,
			"size":      att.Size,
		}
	}

	output := map[string]interface{}{
		"id":              msg.ID,
		"subject":         msg.Subject,
		"snippet":         msg.Snippet,
		"from":            msg.From,
		"to":              msg.To,
		"sent_at":         msg.SentAt.Format(time.RFC3339),
		"size_estimate":   msg.SizeEstimate,
		"has_attachments": msg.HasAttachments,
		"labels":          msg.Labels,
		"attachments":     attachments,
		"body":            msg.Body,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func init() {
	rootCmd.AddCommand(showMessageCmd)
	showMessageCmd.Flags().BoolVar(&showMessageJSON, "json", false, "Output as JSON")
}
