package imessage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// openAttachment never reopens the source outside the selected directory.
// A copied tree may retain canonical Mac paths in its database.
func openAttachment(directory, filename string, rebase bool) (*os.File, error) {
	const prefix = "Library/Messages/Attachments/"
	if filename == "" || strings.Contains(filename, "://") || strings.ContainsRune(filename, '\x00') {
		return nil, errors.New("invalid attachment path")
	}
	if rebase {
		if suffix, ok := strings.CutPrefix(filename, "~/"+prefix); ok {
			filename = suffix
		} else if userPath, ok := strings.CutPrefix(filename, "/Users/"); ok {
			// Only the canonical user-home layout is eligible for rebasing.
			_, rest, ok := strings.Cut(userPath, "/")
			if suffix, found := strings.CutPrefix(rest, prefix); ok && found {
				filename = suffix
			}
		}
	}
	if strings.HasPrefix(filename, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		filename = filepath.Join(home, filename[2:])
	} else if strings.HasPrefix(filename, "~") {
		return nil, errors.New("unsupported attachment home path")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if filepath.IsAbs(filename) {
		filename, err = filepath.Rel(directory, filename)
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsLocal(filename) {
		return nil, errors.New("attachment path escapes root")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || attachmentOffloaded(info) {
		return nil, errors.New("attachment is unavailable locally")
	}
	f, err := root.OpenFile(filename, os.O_RDONLY|attachmentNonblock, 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || attachmentOffloaded(info) {
		_ = f.Close()
		return nil, errors.New("attachment is not a regular file")
	}
	return f, nil
}
