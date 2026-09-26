package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const (
	stdoutSentinel = "-"
	emlFileMode    = 0o600
)

var (
	exportEMLOutput  string
	exportEMLThread  bool
	exportEMLAccount string
)

var exportEMLCmd = &cobra.Command{
	Use:   "export-eml <id>",
	Short: "Export a message as .eml file",
	Long: `Export a message from the archive as a standard .eml (MIME) file.

This command retrieves the raw MIME data stored during sync and writes it
to a file. The .eml format is compatible with most email clients.

With --thread, it writes every message in the conversation that has stored
MIME into a directory, numbered oldest first, and reports messages it had to
skip and when the account last synced.

Examples:
  msgvault export-eml 12345
  msgvault export-eml 12345 --output message.eml
  msgvault export-eml 18f0abc123def -o important.eml
  msgvault export-eml 18f0abc123def --thread -o thread/`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveMessageIDArg(args[0])
		if err != nil {
			return err
		}
		if exportEMLThread {
			return runExportEMLThread(cmd, id, exportEMLAccount, exportEMLOutput)
		}
		return runExportEML(cmd, id, exportEMLOutput)
	},
}

func sanitizeEMLFilename(sourceMessageID string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '\x00' {
			return '_'
		}
		return r
	}, sourceMessageID)
	// Ensure the result is a plain filename with no directory
	// components, guarding against IMAP mailbox names with
	// path separators or traversal sequences.
	safe = filepath.Base(safe)
	if safe == "" || safe == "." {
		safe = "message"
	}
	return safe + ".eml"
}

func runExportEML(cmd *cobra.Command, messageRef, outputPath string) error {
	return runExportEMLHTTP(cmd, messageRef, outputPath)
}

func runExportEMLHTTP(cmd *cobra.Command, messageRef, outputPath string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	rawData, sourceMessageID, err := s.GetCLIMessageRaw(cmd.Context(), messageRef)
	if errors.Is(err, store.ErrMessageNotFound) {
		return fmt.Errorf("message not found: %s", messageRef)
	}
	if err != nil {
		return fmt.Errorf("get raw message data: %w (message may not have raw data stored)", err)
	}
	if sourceMessageID == "" {
		sourceMessageID = messageRef
	}

	return writeExportedEML(cmd, sourceMessageID, outputPath, rawData)
}

func writeExportedEML(cmd *cobra.Command, sourceMessageID, outputPath string, rawData []byte) error {
	if outputPath == "" {
		outputPath = sanitizeEMLFilename(sourceMessageID)
	}

	if outputPath == stdoutSentinel {
		_, err := cmd.OutOrStdout().Write(rawData)
		return err
	}

	if err := fileutil.SecureWriteFile(outputPath, rawData, emlFileMode); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	cmd.Printf("Exported message to: %s (%d bytes)\n", outputPath, len(rawData))
	return nil
}

// runExportEMLThread writes each archived message of messageRef's
// conversation that has stored MIME to outputDir as <n>-<source id>.eml,
// numbered oldest first. A numeric reference is an internal message ID;
// anything else is a provider message ID.
func runExportEMLThread(cmd *cobra.Command, messageRef, account, outputDir string) error {
	if outputDir == stdoutSentinel {
		return errors.New("--thread writes one file per message; pass a directory with -o")
	}
	if outputDir == "" {
		outputDir = "."
	}
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()
	engine := daemonclient.NewEngineAdapter(s)

	ref := query.MessageRef{Account: account}
	if id, parseErr := strconv.ParseInt(messageRef, 10, 64); parseErr == nil {
		ref.ID = id
	} else {
		ref.SourceMessageID = messageRef
	}

	var messages []query.ThreadMessage
	var header query.ThreadPage
	for offset := 0; ; {
		page, err := engine.ListThread(cmd.Context(), query.ThreadQuery{
			MessageRef: ref, Limit: query.ThreadMaxLimit, Offset: offset,
		})
		if errors.Is(err, store.ErrMessageNotFound) {
			return fmt.Errorf("message not found: %s", messageRef)
		}
		if err != nil {
			return fmt.Errorf("list thread: %w", err)
		}
		header = *page
		messages = append(messages, page.Messages...)
		offset += len(page.Messages)
		if !page.HasMore || len(page.Messages) == 0 {
			break
		}
	}

	if err := fileutil.SecureMkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	width := len(strconv.Itoa(len(messages)))
	exported := 0
	for i, msg := range messages {
		if !msg.HasRaw {
			cmd.Printf("Skipped %s: no original MIME stored\n", msg.SourceMessageID)
			continue
		}
		original, err := engine.ReadOriginalMessage(cmd.Context(), query.MessageRef{ID: msg.ID})
		if err != nil {
			return fmt.Errorf("export message %d: %w", msg.ID, err)
		}
		name := fmt.Sprintf("%0*d-%s", width, i+1, sanitizeEMLFilename(msg.SourceMessageID))
		if err := fileutil.SecureWriteFile(filepath.Join(outputDir, name), original.MIME, emlFileMode); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
		exported++
	}

	cmd.Printf("Exported %d of %d messages in thread %s to %s\n",
		exported, len(messages), header.SourceConversationID, outputDir)
	if header.LastSyncAt != nil {
		cmd.Printf("%s last synced %s; newer replies may not be archived yet\n",
			header.Account, header.LastSyncAt.Format(time.RFC3339))
	} else {
		cmd.Printf("%s has never completed a sync; newer replies may not be archived yet\n", header.Account)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(exportEMLCmd)
	exportEMLCmd.Flags().StringVarP(&exportEMLOutput, "output", "o", "", "Output file path (default: <source_message_id>.eml, use - for stdout); with --thread, the output directory (default: current directory)")
	exportEMLCmd.Flags().BoolVar(&exportEMLThread, "thread", false, "Export every message in the conversation that has stored MIME")
	exportEMLCmd.Flags().StringVar(&exportEMLAccount, "account", "", "With --thread, the account (email address) that holds a provider message ID found in several accounts")
}
