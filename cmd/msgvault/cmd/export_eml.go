package cmd

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/fileutil"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
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
		return runExportEML(cmd, args[0], exportEMLOutput)
	},
}

type resolvedMessage struct {
	ID              int64
	SourceMessageID string
}

func resolveMessage(engine *query.SQLiteEngine, cmd *cobra.Command, messageRef string) (resolvedMessage, error) {
	if id, err := strconv.ParseInt(messageRef, 10, 64); err == nil {
		msg, err := engine.GetMessage(cmd.Context(), id)
		if err != nil {
			return resolvedMessage{}, fmt.Errorf("get message: %w", err)
		}
		if msg == nil {
			return resolvedMessage{}, fmt.Errorf("message not found: %s", messageRef)
		}
		return resolvedMessage{ID: id, SourceMessageID: msg.SourceMessageID}, nil
	}

	msg, err := engine.GetMessageBySourceID(cmd.Context(), messageRef)
	if err != nil {
		return resolvedMessage{}, fmt.Errorf("get message: %w", err)
	}
	if msg == nil {
		return resolvedMessage{}, fmt.Errorf("message not found: %s", messageRef)
	}
	return resolvedMessage{ID: msg.ID, SourceMessageID: msg.SourceMessageID}, nil
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

	engine := query.NewSQLiteEngine(s.DB())

	resolved, err := resolveMessage(engine, cmd, messageRef)
	if err != nil {
		return err
	}

	rawData, err := s.GetMessageRaw(resolved.ID)
	if err != nil {
		return fmt.Errorf("get raw message data: %w (message may not have raw data stored)", err)
	}

	if outputPath == "" {
		outputPath = sanitizeEMLFilename(resolved.SourceMessageID)
	}

	if outputPath == stdoutSentinel {
		_, err = cmd.OutOrStdout().Write(rawData)
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
