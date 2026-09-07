package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/maildir"
	"go.kenn.io/msgvault/internal/store"
)

// MaildirImportOptions configures a read-only Maildir archive import.
// CheckpointInterval defaults to 200 and MaxMessageBytes to 128 MiB.
type MaildirImportOptions = EMLImportOptions

// MaildirImportSummary reports Maildir import progress and recoverable errors.
type MaildirImportSummary = EMLImportSummary

// ImportMaildir archives delivered Maildir messages. Content hashes make
// filename/flag renames idempotent; duplicate copies accumulate archive labels.
// Import a stable snapshot rather than a mailbox being modified concurrently.
func ImportMaildir(ctx context.Context, st *store.Store, root string, opts MaildirImportOptions) (*MaildirImportSummary, error) {
	if opts.Identifier == "" {
		return nil, errors.New("identifier is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Confinement also protects against files replaced with external symlinks
	// between discovery and read.
	dir, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, fmt.Errorf("open Maildir root: %w", err)
	}
	defer func() { _ = dir.Close() }()
	return importRawDirectory(ctx, st, absRoot, opts, rawDirectoryLayout{
		sourceType: "maildir",
		discover: func(root string) ([]directoryMailbox, error) {
			boxes, err := maildir.Discover(root)
			result := make([]directoryMailbox, len(boxes))
			for i, box := range boxes {
				result[i] = directoryMailbox(box)
			}
			return result, err
		},
		labels: maildir.Flags,
		read: func(path string, maxBytes int64) ([]byte, error) {
			rel, err := filepath.Rel(absRoot, path)
			if err != nil {
				return nil, err
			}
			info, err := dir.Lstat(rel)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, errors.New("maildir message is not a regular file")
			}
			file, err := dir.Open(rel)
			if err != nil {
				return nil, err
			}
			defer func() { _ = file.Close() }()
			opened, err := file.Stat()
			if err != nil {
				return nil, err
			}
			if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
				return nil, errors.New("maildir message changed during import")
			}
			raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(raw)) > maxBytes {
				return nil, fmt.Errorf("message exceeds %d-byte limit", maxBytes)
			}
			return raw, nil
		},
	})
}
