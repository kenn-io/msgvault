// Package eml discovers RFC 5322 message files in MailMate-style mailbox trees.
package eml

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mailbox is a .mailbox directory containing direct .eml children.
type Mailbox struct {
	Path  string
	Label string
	Files []string
}

// DiscoverMailboxes returns every .mailbox directory below root that contains
// direct .eml children. Nested mailbox names become slash-separated labels.
func DiscoverMailboxes(root string) ([]Mailbox, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("eml discover: absolute path: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("eml discover: stat %q: %w", absRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("eml discover: %q is not a directory", absRoot)
	}

	labelRoot := absRoot
	if isMailboxDir(absRoot) {
		labelRoot = filepath.Dir(absRoot)
	}

	var mailboxes []Mailbox
	err = filepath.WalkDir(absRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || !isMailboxDir(path) {
			return nil
		}

		children, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("read mailbox %q: %w", path, err)
		}
		files := make([]string, 0)
		for _, child := range children {
			if child.IsDir() || !strings.EqualFold(filepath.Ext(child.Name()), ".eml") {
				continue
			}
			childInfo, err := child.Info()
			if err != nil {
				return fmt.Errorf("stat message %q: %w", filepath.Join(path, child.Name()), err)
			}
			if childInfo.Mode().IsRegular() {
				files = append(files, filepath.Join(path, child.Name()))
			}
		}
		if len(files) == 0 {
			return nil
		}
		sort.Strings(files)
		mailboxes = append(mailboxes, Mailbox{
			Path:  path,
			Label: labelFromPath(labelRoot, path),
			Files: files,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("eml discover: walk %q: %w", absRoot, err)
	}
	if len(mailboxes) == 0 {
		return nil, fmt.Errorf("eml discover: no .eml files found in .mailbox directories under %q", absRoot)
	}

	sort.Slice(mailboxes, func(i, j int) bool {
		return mailboxes[i].Path < mailboxes[j].Path
	})
	return mailboxes, nil
}

func isMailboxDir(path string) bool {
	return strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".mailbox")
}

func labelFromPath(root, mailboxPath string) string {
	rel, err := filepath.Rel(root, mailboxPath)
	if err != nil {
		return strings.TrimSuffix(filepath.Base(mailboxPath), filepath.Ext(mailboxPath))
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		if !strings.HasSuffix(strings.ToLower(part), ".mailbox") {
			continue
		}
		labels = append(labels, part[:len(part)-len(".mailbox")])
	}
	if len(labels) == 0 {
		return strings.TrimSuffix(filepath.Base(mailboxPath), filepath.Ext(mailboxPath))
	}
	return strings.Join(labels, "/")
}
