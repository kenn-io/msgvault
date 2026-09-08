// Package maildir reads Maildir layouts without modifying mailbox files.
package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mailbox contains the delivered files and archive label of one Maildir.
type Mailbox struct {
	Path  string
	Label string
	Files []string
}

// Discover finds standard and Maildir++ mailboxes below root. It never follows
// symlinks or descends into cur, new, or tmp directories, and it skips
// dotfiles in cur and new because Maildir unique names never start with a dot.
func Discover(root string) ([]Mailbox, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var boxes []Mailbox
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root {
			switch entry.Name() {
			case "cur", "new", "tmp":
				return filepath.SkipDir
			}
		}
		valid := true
		for _, dir := range []string{"cur", "new", "tmp"} {
			info, err := os.Lstat(filepath.Join(path, dir))
			if os.IsNotExist(err) {
				valid = false
				break
			}
			if err != nil {
				return err
			}
			if !info.IsDir() {
				valid = false
				break
			}
		}
		if !valid {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		label := "INBOX"
		if rel != "." {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			for i, part := range parts {
				if trimmed, ok := strings.CutPrefix(part, "."); ok {
					parts[i] = strings.ReplaceAll(trimmed, ".", "/")
				}
			}
			label = strings.Join(parts, "/")
		}
		box := Mailbox{Path: path, Label: label}
		for _, dir := range []string{"cur", "new"} {
			entries, err := os.ReadDir(filepath.Join(path, dir))
			if err != nil {
				return err
			}
			for _, file := range entries {
				// Unique names never start with a dot; such entries are
				// bookkeeping files, not messages.
				if strings.HasPrefix(file.Name(), ".") {
					continue
				}
				info, err := file.Info()
				if err != nil {
					return fmt.Errorf("inspect Maildir entry: %w", err)
				}
				if info.Mode().IsRegular() {
					box.Files = append(box.Files, filepath.Join(path, dir, file.Name()))
				}
			}
		}
		boxes = append(boxes, box)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover Maildir: %w", err)
	}
	if len(boxes) == 0 {
		return nil, fmt.Errorf("no Maildir mailboxes (cur, new, tmp) found under %q", root)
	}
	return boxes, nil
}

// Flags returns archival labels for the standard Maildir info suffix. Unknown
// flags are ignored. Absence of the seen flag denotes an unread message.
func Flags(path string) []string {
	name := filepath.Base(path)
	flags := ""
	if i := strings.LastIndex(name, ":2,"); i >= 0 {
		flags = name[i+3:]
	}
	var labels []string
	if !strings.Contains(flags, "S") {
		labels = append(labels, "UNREAD")
	}
	for _, flag := range []struct{ letter, label string }{
		{"D", "DRAFT"}, {"F", "STARRED"}, {"P", "PASSED"}, {"R", "REPLIED"}, {"T", "TRASH"},
	} {
		if strings.Contains(flags, flag.letter) {
			labels = append(labels, flag.label)
		}
	}
	return labels
}
