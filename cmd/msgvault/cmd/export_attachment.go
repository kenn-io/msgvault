package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/fileutil"
)

var (
	exportAttachmentOutput string
	exportAttachmentJSON   bool
	exportAttachmentBase64 bool
)

var exportAttachmentCmd = &cobra.Command{
	Use:   "export-attachment <content-hash>",
	Short: "Export an attachment by content hash",
	Long: `Export an attachment binary by its SHA-256 content hash.

Get the content hash from 'show-message --json':
  msgvault show-message 45 --json | jq '.attachments[0].content_hash'

Examples:
  msgvault export-attachment 61ccf192b5bd358738802dc2676d3ceab856f47d26dd29681ac3d335bfd5bbd0
  msgvault export-attachment 61ccf192... --output invoice.pdf

Export all attachments from a message with original filenames:
  msgvault show-message 45 --json | \
    jq -r '.attachments[] | "\(.content_hash)\t\(.filename)"' | \
    while IFS=$'\t' read -r hash name; do
      msgvault export-attachment "$hash" -o "$name"
    done
  msgvault export-attachment 61ccf192... -o -       # stdout (binary)
  msgvault export-attachment 61ccf192... --base64  # stdout (base64)
  msgvault export-attachment 61ccf192... --json    # JSON with base64 data`,
	Args: cobra.ExactArgs(1),
	RunE: runExportAttachment,
}

func runExportAttachment(cmd *cobra.Command, args []string) error {
	contentHash := args[0]

	// Validate hash format using shared validation
	if err := export.ValidateContentHash(contentHash); err != nil {
		return err
	}

	// Validate flag combinations
	if exportAttachmentJSON && exportAttachmentBase64 {
		return usageErr(cmd, fmt.Errorf("--json and --base64 are mutually exclusive"))
	}
	if exportAttachmentOutput != "" && exportAttachmentOutput != "-" {
		if exportAttachmentJSON {
			return usageErr(cmd, fmt.Errorf("--json and --output are mutually exclusive (--json writes to stdout)"))
		}
		if exportAttachmentBase64 {
			return usageErr(cmd, fmt.Errorf("--base64 and --output are mutually exclusive (--base64 writes to stdout)"))
		}
	}

	// Validate output path before doing any work
	if exportAttachmentOutput != "" && exportAttachmentOutput != "-" {
		if err := export.ValidateOutputPath(exportAttachmentOutput); err != nil {
			return err
		}
	}

	// Construct storage path: attachmentsDir/hash[:2]/hash
	attachmentsDir := cfg.AttachmentsDir()
	storagePath := filepath.Join(attachmentsDir, contentHash[:2], contentHash)

	// JSON mode reads the full file into memory for base64 encoding.
	// Base64 and binary modes stream directly to avoid loading large files.
	if exportAttachmentJSON {
		return exportAttachmentAsJSON(storagePath, contentHash)
	}
	if exportAttachmentBase64 {
		return exportAttachmentAsBase64(storagePath)
	}
	return exportAttachmentBinary(storagePath, contentHash)
}

func exportAttachmentAsJSON(storagePath, contentHash string) error {
	data, err := readAttachmentFile(storagePath, contentHash)
	if err != nil {
		return err
	}

	output := map[string]any{
		"content_hash": contentHash,
		"size":         len(data),
		"data_base64":  base64.StdEncoding.EncodeToString(data),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func exportAttachmentAsBase64(storagePath string) error {
	f, err := openAttachmentFile(storagePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	encoder := base64.NewEncoder(base64.StdEncoding, os.Stdout)
	if _, err := io.Copy(encoder, f); err != nil {
		return fmt.Errorf("encode attachment: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("finalize base64: %w", err)
	}
	fmt.Println() // trailing newline
	return nil
}

func exportAttachmentBinary(storagePath, contentHash string) error {
	f, err := openAttachmentFile(storagePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	outputPath := exportAttachmentOutput
	if outputPath == "" || outputPath == "-" {
		_, err = io.Copy(os.Stdout, f)
		return err
	}

	dst, err := fileutil.SecureOpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}

	n, copyErr := io.Copy(dst, f)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("write file: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("close file: %w", closeErr)
	}

	fmt.Fprintf(os.Stderr, "Exported attachment to: %s (%d bytes)\n", outputPath, n)
	return nil
}

func openAttachmentFile(storagePath string) (*os.File, error) {
	f, err := os.Open(storagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("attachment not found: %s", filepath.Base(storagePath))
		}
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	return f, nil
}

func readAttachmentFile(storagePath, contentHash string) ([]byte, error) {
	data, err := os.ReadFile(storagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("attachment not found: no file for hash %s", contentHash)
		}
		return nil, fmt.Errorf("read attachment: %w", err)
	}
	return data, nil
}

func init() {
	rootCmd.AddCommand(exportAttachmentCmd)
	exportAttachmentCmd.Flags().StringVarP(&exportAttachmentOutput, "output", "o", "", "Output file path (default: stdout, use - for stdout)")
	exportAttachmentCmd.Flags().BoolVar(&exportAttachmentJSON, "json", false, "Output as JSON with base64-encoded data")
	exportAttachmentCmd.Flags().BoolVar(&exportAttachmentBase64, "base64", false, "Output raw base64 to stdout")
}
