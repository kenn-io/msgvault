package imessage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errOutsideAttachmentRoot marks a source path that points outside the selected tree.
var errOutsideAttachmentRoot = errors.New("attachment path is outside the attachments root")

// openMediaRoot confines later reads to one media tree of the Messages folder, so a
// symlink inside Attachments cannot lead to other files in the Messages folder. A copied
// folder (strict) may not redirect the media tree itself; a user's own folder may.
func openMediaRoot(directory, filename string, strict bool) (*os.Root, string, error) {
	tree, rest, _ := strings.Cut(filepath.ToSlash(filepath.Clean(filename)), "/")
	if !filepath.IsLocal(filename) || (tree != "Attachments" && tree != "StickerCache") {
		return nil, "", errOutsideAttachmentRoot
	}
	dir := filepath.Join(directory, tree)
	if info, err := os.Lstat(dir); strict && err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return nil, "", errOutsideAttachmentRoot
	}
	root, err := os.OpenRoot(dir)
	return root, filepath.FromSlash(rest), err
}

// statMedia stats name inside root. os.Root reports a symlink that leaves the tree with an
// error that is neither "not found" nor "permission", so a symlink there marks an escape.
func statMedia(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Stat(name)
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return info, err
	}
	if link, lerr := root.Lstat(name); lerr != nil || link.Mode()&fs.ModeSymlink != 0 {
		return nil, errOutsideAttachmentRoot
	}
	return info, err
}

// openAttachment never reopens the source outside the Messages folder's
// Attachments and StickerCache trees. A copied folder may retain canonical Mac
// paths in its database.
func openAttachment(directory, filename string, rebase bool) (*os.File, error) {
	const prefix = "Library/Messages/"
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
		return nil, errOutsideAttachmentRoot
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
	root, filename, err := openMediaRoot(directory, filename, rebase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	info, err := statMedia(root, filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || attachmentOffloaded(info) {
		return nil, errors.New("attachment is unavailable locally")
	}
	f, err := root.OpenFile(filename, os.O_RDONLY, 0)
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
