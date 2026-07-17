package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
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
	idStr, err := resolveMessageIDArg(args[0])
	if err != nil {
		return err
	}
	return runExportAttachmentsHTTP(cmd, idStr)
}

type cliAttachmentClient interface {
	GetCLIAttachment(ctx context.Context, contentHash string) ([]byte, error)
}

func runExportAttachmentsHTTP(cmd *cobra.Command, idStr string) error {
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

	if len(msg.Attachments) == 0 {
		fmt.Fprintln(os.Stderr, "No attachments on this message.")
		return nil
	}

	outputDir, err := resolveExportAttachmentsOutputDir()
	if err != nil {
		return err
	}

	result := exportAttachmentsFromHTTP(cmd.Context(), s, outputDir, msg.Attachments)
	return printExportAttachmentsResult(result, len(msg.Attachments), outputDir)
}

func resolveExportAttachmentsOutputDir() (string, error) {
	outputDir := exportAttachmentsOutput
	if outputDir == "" {
		var err error
		outputDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
	}
	var err error
	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	info, err := os.Stat(outputDir)
	if errors.Is(err, os.ErrNotExist) {
		// Create the requested output directory (and any parents) so the
		// command behaves like its sibling exporters, which create the file
		// or path they are asked to write to.
		if mkErr := os.MkdirAll(outputDir, 0o755); mkErr != nil {
			return "", fmt.Errorf("create output directory: %w", mkErr)
		}
		info, err = os.Stat(outputDir)
	}
	if err != nil {
		return "", fmt.Errorf("output directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", outputDir)
	}
	tmpFile, err := os.CreateTemp(outputDir, ".msgvault_write_test-*")
	if err != nil {
		return "", fmt.Errorf("output directory not writable: %w", err)
	}
	_ = tmpFile.Close()
	_ = os.Remove(tmpFile.Name())
	return outputDir, nil
}

func exportAttachmentsFromHTTP(
	ctx context.Context,
	client cliAttachmentClient,
	outputDir string,
	attachments []query.AttachmentInfo,
) export.DirExportResult {
	var result export.DirExportResult
	usedNames := make(map[string]int)

	for _, att := range attachments {
		if att.URL != "" {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: URL-backed attachment is available at %s", att.Filename, att.URL))
			continue
		}
		if err := export.ValidateContentHash(att.ContentHash); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}

		filename := resolveExportAttachmentFilename(att.Filename, att.ContentHash, usedNames)
		data, err := client.GetCLIAttachment(ctx, att.ContentHash)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		exported, err := writeExportAttachmentBytes(outputDir, filename, data)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		result.Files = append(result.Files, exported)
	}

	return result
}

func resolveExportAttachmentFilename(original, contentHash string, usedNames map[string]int) string {
	filename := export.SanitizeFilename(filepath.Base(original))
	if filename == "" || filename == "." {
		filename = contentHash
	}

	baseKey := filename
	if count, exists := usedNames[baseKey]; exists {
		ext := filepath.Ext(filename)
		base := filename[:len(filename)-len(ext)]
		filename = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		usedNames[baseKey] = count + 1
	} else {
		usedNames[baseKey] = 1
	}

	return filename
}

func writeExportAttachmentBytes(outputDir, filename string, data []byte) (export.ExportedFile, error) {
	destPath := filepath.Join(outputDir, filename)
	dst, finalPath, err := export.CreateExclusiveFile(destPath, 0600)
	if err != nil {
		return export.ExportedFile{}, fmt.Errorf("create output file: %w", err)
	}

	n, writeErr := dst.Write(data)
	closeErr := dst.Close()
	if writeErr != nil {
		_ = os.Remove(finalPath)
		return export.ExportedFile{}, fmt.Errorf("write: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(finalPath)
		return export.ExportedFile{}, fmt.Errorf("close: %w", closeErr)
	}
	if n != len(data) {
		_ = os.Remove(finalPath)
		return export.ExportedFile{}, errors.New("write: short write")
	}

	return export.ExportedFile{Path: finalPath, Size: int64(n)}, nil
}

func printExportAttachmentsResult(result export.DirExportResult, attachmentCount int, outputDir string) error {
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
			len(result.Errors), attachmentCount)
	}

	return nil
}

func init() {
	rootCmd.AddCommand(exportAttachmentsCmd)
	exportAttachmentsCmd.Flags().StringVarP(&exportAttachmentsOutput, "output", "o", "",
		"Output directory (default: current directory)")
}
