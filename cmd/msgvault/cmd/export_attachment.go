package cmd

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/export"
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

To export all attachments from a message with original filenames, use
'msgvault export-attachments <message-id> -o <dir>', which sanitizes
filenames. Attachment filenames are sender-controlled: do not pass the
JSON 'filename' field to -o (a name like ../../evil escapes the output
directory). Use content hashes or your own fixed paths instead.

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
		return usageErr(cmd, errors.New("--json and --base64 are mutually exclusive"))
	}
	if exportAttachmentOutput != "" && exportAttachmentOutput != "-" {
		if exportAttachmentJSON {
			return usageErr(cmd, errors.New("--json and --output are mutually exclusive (--json writes to stdout)"))
		}
		if exportAttachmentBase64 {
			return usageErr(cmd, errors.New("--base64 and --output are mutually exclusive (--base64 writes to stdout)"))
		}
	}

	return runExportAttachmentHTTP(cmd, contentHash)
}

func runExportAttachmentHTTP(cmd *cobra.Command, contentHash string) error {
	if cmd == nil {
		return errors.New("command context is required for HTTP attachment export")
	}
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	body, err := s.OpenCLIAttachment(cmd.Context(), contentHash)
	if err != nil {
		return err
	}
	if exportAttachmentJSON {
		return errors.Join(exportAttachmentStreamAsJSON(body, contentHash), body.Close())
	}

	if exportAttachmentBase64 {
		return errors.Join(exportAttachmentStreamAsBase64(body), body.Close())
	}
	return exportAttachmentBinaryDownload(body)
}

// exportAttachmentStreamAsJSON writes the same document as a deterministic
// indented encoder, streaming the base64 data so size can come last.
func exportAttachmentStreamAsJSON(r io.Reader, contentHash string) error {
	hash, err := json.Marshal(contentHash)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(os.Stdout, "{\n  \"content_hash\": %s,\n  \"data_base64\": \"", hash); err != nil {
		return fmt.Errorf("write attachment JSON: %w", err)
	}
	encoder := base64.NewEncoder(base64.StdEncoding, os.Stdout)
	size, err := io.Copy(encoder, r)
	if err != nil {
		return fmt.Errorf("encode attachment: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("finalize base64: %w", err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "\",\n  \"size\": %d\n}\n", size); err != nil {
		return fmt.Errorf("write attachment JSON: %w", err)
	}
	return nil
}

func exportAttachmentStreamAsBase64(r io.Reader) error {
	encoder := base64.NewEncoder(base64.StdEncoding, os.Stdout)
	if _, err := io.Copy(encoder, r); err != nil {
		return fmt.Errorf("encode attachment: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("finalize base64: %w", err)
	}
	fmt.Println() // trailing newline
	return nil
}

func exportAttachmentBinaryStream(r io.Reader) error {
	outputPath := exportAttachmentOutput
	if outputPath == "" || outputPath == "-" {
		_, err := io.Copy(os.Stdout, r)
		return err
	}

	n, err := writeAttachmentStreamToFile(outputPath, r)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Exported attachment to: %s (%d bytes)\n", outputPath, n)
	return nil
}

func exportAttachmentBinaryDownload(body io.ReadCloser) (err error) {
	sourceClosed := false
	closeSource := func() error {
		if sourceClosed {
			return nil
		}
		sourceClosed = true
		return body.Close()
	}
	defer func() {
		err = errors.Join(err, closeSource())
	}()

	outputPath := exportAttachmentOutput
	if outputPath == "" || outputPath == "-" {
		_, copyErr := io.Copy(os.Stdout, body)
		return errors.Join(copyErr, closeSource())
	}

	n, err := writeAttachmentStreamToFileBeforeInstall(outputPath, body, closeSource)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Exported attachment to: %s (%d bytes)\n", outputPath, n)
	return nil
}

func writeAttachmentStreamToFile(outputPath string, r io.Reader) (int64, error) {
	return writeAttachmentStreamToFileBeforeInstall(outputPath, r, nil)
}

func writeAttachmentStreamToFileBeforeInstall(
	outputPath string,
	r io.Reader,
	closeSource func() error,
) (int64, error) {
	dir := filepath.Dir(outputPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outputPath)+".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("create output file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("set output file permissions: %w", err)
	}

	n, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()
	var sourceCloseErr error
	if closeSource != nil {
		sourceCloseErr = closeSource()
	}
	if copyErr != nil || closeErr != nil || sourceCloseErr != nil {
		return 0, errors.Join(
			wrapAttachmentExportError("write file", copyErr),
			wrapAttachmentExportError("close file", closeErr),
			wrapAttachmentExportError("verify downloaded attachment", sourceCloseErr),
		)
	}
	if err := replaceOutputFile(tmpPath, outputPath); err != nil {
		return 0, fmt.Errorf("replace output file: %w", err)
	}
	cleanup = false
	return n, nil
}

func wrapAttachmentExportError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func replaceOutputFile(tmpPath, outputPath string) error {
	info, err := os.Lstat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return atomicfile.Replace(tmpPath, outputPath)
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("output path is a directory: %s", outputPath)
	}

	dir := filepath.Dir(outputPath)
	backup, err := os.CreateTemp(dir, "."+filepath.Base(outputPath)+".old-*")
	if err != nil {
		return fmt.Errorf("create output backup: %w", err)
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		return fmt.Errorf("close output backup: %w", err)
	}
	if err := os.Remove(backupPath); err != nil {
		return fmt.Errorf("prepare output backup: %w", err)
	}

	if err := atomicfile.Replace(outputPath, backupPath); err != nil {
		return fmt.Errorf("backup existing output: %w", err)
	}
	if err := atomicfile.Replace(tmpPath, outputPath); err != nil {
		if restoreErr := atomicfile.Replace(backupPath, outputPath); restoreErr != nil {
			return fmt.Errorf("%w; restore existing output: %w", err, restoreErr)
		}
		return err
	}
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove output backup: %w", err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(exportAttachmentCmd)
	exportAttachmentCmd.Flags().StringVarP(&exportAttachmentOutput, "output", "o", "", "Output file path (default: stdout, use - for stdout)")
	exportAttachmentCmd.Flags().BoolVar(&exportAttachmentJSON, flagJSON, false, "Output as JSON with base64-encoded data")
	exportAttachmentCmd.Flags().BoolVar(&exportAttachmentBase64, "base64", false, "Output raw base64 to stdout")
}
