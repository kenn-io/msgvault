package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/export"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/store"
)

var exportAttachmentsOutput string

var exportAttachmentsCmd = &cobra.Command{
	Use:   "export-attachments <message-id>",
	Short: "Export all attachments from a message as individual files",
	Long: `Export all attachments from a message to a directory with original filenames.

Takes a message ID (internal numeric or Gmail ID) and writes each attachment
as a separate file. Filenames are sanitized and deduplicated automatically.
Files are never overwritten — a numeric suffix is appended on conflict.

Examples:
  msgvault export-attachments 45                  # all attachments → cwd
  msgvault export-attachments 45 -o ~/Downloads   # all attachments → specific dir
  msgvault export-attachments 18f0abc123def       # by Gmail ID`,
	Args: cobra.ExactArgs(1),
	RunE: runExportAttachments,
}

func runExportAttachments(cmd *cobra.Command, args []string) error {
	idStr := args[0]

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

	engine := query.NewSQLiteEngine(s.DB())

	// Resolve message ID — try numeric first, fallback to Gmail ID
	var msg *query.MessageDetail
	if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
		msg, err = engine.GetMessage(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("get message: %w", err)
		}
	}
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

	if len(msg.Attachments) == 0 {
		fmt.Fprintln(os.Stderr, "No attachments on this message.")
		return nil
	}

	// Resolve output directory
	outputDir := exportAttachmentsOutput
	if outputDir == "" {
		outputDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}
	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	info, err := os.Stat(outputDir)
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", outputDir)
	}
	tmpFile, err := os.CreateTemp(outputDir, ".msgvault_write_test-*")
	if err != nil {
		return fmt.Errorf("output directory not writable: %w", err)
	}
	_ = tmpFile.Close()
	_ = os.Remove(tmpFile.Name())

	// Export
	attachmentsDir := cfg.AttachmentsDir()
	result := export.AttachmentsToDir(outputDir, attachmentsDir, msg.Attachments)

	// Print per-file results
	for _, f := range result.Files {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n",
			filepath.Base(f.Path), export.FormatBytesLong(f.Size))
	}

	// Print errors
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}

	// Summary
	if len(result.Files) > 0 {
		fmt.Fprintf(os.Stderr, "Exported %d attachment(s) (%s) to %s\n",
			len(result.Files), export.FormatBytesLong(result.TotalSize()), outputDir)
	}

	if len(result.Errors) > 0 && len(result.Files) == 0 {
		return fmt.Errorf("all %d attachment(s) failed to export", len(result.Errors))
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%d of %d attachment(s) failed to export",
			len(result.Errors), len(msg.Attachments))
	}

	return nil
}

func init() {
	rootCmd.AddCommand(exportAttachmentsCmd)
	exportAttachmentsCmd.Flags().StringVarP(&exportAttachmentsOutput, "output", "o", "",
		"Output directory (default: current directory)")
}
