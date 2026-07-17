package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

const (
	stdoutSentinel = "-"
	emlFileMode    = 0o600
)

var (
	exportEMLOutput string
)

var exportEMLCmd = &cobra.Command{
	Use:   "export-eml <id>",
	Short: "Export a message as .eml file",
	Long: `Export a message from the archive as a standard .eml (MIME) file.

This command retrieves the raw MIME data stored during sync and writes it
to a file. The .eml format is compatible with most email clients.

Examples:
  msgvault export-eml 12345
  msgvault export-eml 12345 --output message.eml
  msgvault export-eml 18f0abc123def -o important.eml`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := resolveMessageIDArg(args[0])
		if err != nil {
			return err
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

func init() {
	rootCmd.AddCommand(exportEMLCmd)
	exportEMLCmd.Flags().StringVarP(&exportEMLOutput, "output", "o", "", "Output file path (default: <source_message_id>.eml, use - for stdout)")
}
