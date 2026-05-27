package emlx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mailbox represents an Apple Mail mailbox directory containing .emlx files.
type Mailbox struct {
	// Path is the absolute path to the .mbox or .imapmbox directory.
	Path string

	// MsgDir is the absolute path to the primary Messages/ directory
	// containing .emlx files. In legacy layouts this is Path/Messages;
	// in modern V10 layouts it is Path/<GUID>/Data/Messages.
	MsgDir string

	// Label is the derived label for messages in this mailbox.
	Label string

	// Files contains sorted absolute paths to .emlx files within
	// this mailbox, including files from numeric partition
	// subdirectories in V10 layouts.
	Files []string
}

// DiscoverMailboxes walks an Apple Mail directory tree and returns all
// mailbox directories that contain .emlx files.
//
// If rootDir itself is a mailbox directory (ends in .mbox or .imapmbox
// and contains a Messages/ subdirectory with .emlx files), only that
// single mailbox is returned.
func DiscoverMailboxes(rootDir string) ([]Mailbox, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("emlx discover: abs path: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("emlx discover: stat %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf(
			"emlx discover: %q is not a directory", abs,
		)
	}

	// Auto-detect: if the path itself is a mailbox, import just that one.
	if isMailboxDir(abs) {
		msgDir, files, err := listEmlxFiles(abs)
		if err != nil {
			return nil, err
		}
		if len(files) > 0 {
			label := LabelFromPath(filepath.Dir(abs), abs)
			return []Mailbox{{
				Path: abs, MsgDir: msgDir,
				Label: label, Files: files,
			}}, nil
		}
	}

	var mailboxes []Mailbox
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries; partial discovery beats abort
		}
		if !d.IsDir() {
			return nil
		}

		// listEmlxFiles reads Messages/ directly, so skip walking it.
		if d.Name() == "Messages" {
			return filepath.SkipDir
		}

		// Skip non-mailbox directories.
		if !isMailboxDir(path) {
			return nil
		}

		msgDir, files, listErr := listEmlxFiles(path)
		if listErr != nil || len(files) == 0 {
			return nil //nolint:nilerr // unreadable mailbox is the same as empty for discovery
		}

		label := LabelFromPath(abs, path)
		mailboxes = append(mailboxes, Mailbox{
			Path: path, MsgDir: msgDir,
			Label: label, Files: files,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("emlx discover: walk: %w", err)
	}

	sort.Slice(mailboxes, func(i, j int) bool {
		return mailboxes[i].Path < mailboxes[j].Path
	})
	return mailboxes, nil
}

// LabelFromPath derives a human-readable label from a mailbox path
// relative to the root directory.
//
// Rules:
//   - Strip root prefix
//   - Strip known containers: Mailboxes/, IMAP-*/, POP-*/
//   - Strip .mbox/.imapmbox suffix from all components
//   - Use the remaining path as the label
func LabelFromPath(rootDir, mailboxPath string) string {
	rel, err := filepath.Rel(rootDir, mailboxPath)
	if err != nil {
		// Fallback: use the directory base name.
		return stripMailboxSuffix(filepath.Base(mailboxPath))
	}

	// Split into components.
	parts := strings.Split(filepath.ToSlash(rel), "/")

	// Filter out known container directories.
	var filtered []string
	for _, p := range parts {
		if p == "Mailboxes" {
			continue
		}
		if strings.HasPrefix(p, "IMAP-") || strings.HasPrefix(p, "POP-") {
			continue
		}
		// V10 account GUID directories (e.g. 13C9A646-...-E07FFBDDEED3).
		if IsUUID(p) {
			continue
		}
		filtered = append(filtered, p)
	}

	if len(filtered) == 0 {
		return stripMailboxSuffix(filepath.Base(mailboxPath))
	}

	// Strip .mbox/.imapmbox suffix from all components.
	for i := range filtered {
		filtered[i] = stripMailboxSuffix(filtered[i])
	}

	return strings.Join(filtered, "/")
}

func isMailboxDir(path string) bool {
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	if !strings.HasSuffix(lower, ".mbox") &&
		!strings.HasSuffix(lower, ".imapmbox") {
		return false
	}

	return findMessagesDir(path) != ""
}

// findMessagesDir locates the Messages/ directory within a .mbox.
// Returns "" if none found. Checks both legacy (Messages/) and
// modern V10 (<GUID>/Data/Messages/) layouts. When both exist,
// prefers whichever contains .emlx files (directly or in partitions).
func findMessagesDir(mailboxPath string) string {
	var candidates []string

	// Legacy: direct Messages/ subdirectory.
	legacy := filepath.Join(mailboxPath, "Messages")
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		candidates = append(candidates, legacy)
	}

	// Modern V10: <subdir>/Data/Messages/ subdirectory.
	// Also handles partition-only layouts where Data/Messages/ doesn't exist.
	entries, err := os.ReadDir(mailboxPath)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "Messages" {
				continue
			}
			dataDir := filepath.Join(mailboxPath, e.Name(), "Data")
			dataStat, statErr := os.Stat(dataDir)
			if statErr != nil || !dataStat.IsDir() {
				continue
			}
			modern := filepath.Join(dataDir, "Messages")
			msgStat, statErr := os.Stat(modern)
			if statErr == nil && msgStat.IsDir() {
				candidates = append(candidates, modern)
			} else if hasEmlxFilesInPartitions(dataDir) {
				// Partition-only: Data/Messages/ absent but partitions exist.
				candidates = append(candidates, modern)
			}
		}
	}

	if len(candidates) == 0 {
		return ""
	}

	// Prefer the first candidate that has .emlx files directly or
	// within numeric partition subdirectories (V10 only).
	for _, dir := range candidates {
		if hasEmlxFiles(dir) {
			return dir
		}
		// For V10 layout the parent is Data/; check partitions there.
		dataDir := filepath.Dir(dir)
		if filepath.Base(dataDir) == "Data" &&
			hasEmlxFilesInPartitions(dataDir) {
			return dir
		}
	}

	// No candidate has files; return first for isMailboxDir.
	return candidates[0]
}

// hasEmlxFiles returns true if dir contains at least one
// non-partial .emlx file.
func hasEmlxFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && isEmlxFile(e.Name()) {
			return true
		}
	}
	return false
}

// hasEmlxFilesInPartitions returns true if dir contains .emlx files
// within Messages/ subdirectories or nested numeric partition dirs (0-9).
func hasEmlxFilesInPartitions(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "Messages" {
			if hasEmlxFiles(filepath.Join(dir, name)) {
				return true
			}
		} else if isDigitDir(name) {
			if hasEmlxFilesInPartitions(filepath.Join(dir, name)) {
				return true
			}
		}
	}
	return false
}

// IsUUID returns true if s matches UUID format (8-4-4-4-12 hex).
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') ||
				(c >= 'a' && c <= 'f') ||
				(c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func isDigitDir(name string) bool {
	return len(name) == 1 && name[0] >= '0' && name[0] <= '9'
}

func isEmlxFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".emlx") &&
		!strings.HasSuffix(lower, ".partial.emlx")
}

func stripMailboxSuffix(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".imapmbox") {
		return name[:len(name)-len(".imapmbox")]
	}
	if strings.HasSuffix(lower, ".mbox") {
		return name[:len(name)-len(".mbox")]
	}
	return name
}

// listEmlxFiles returns the Messages directory path and sorted
// absolute paths to .emlx files (from both the primary Messages/ dir
// and numeric partition subdirectories).
// Returns ("", nil, nil) if no Messages directory is found.
func listEmlxFiles(
	mailboxPath string,
) (string, []string, error) {
	msgDir := findMessagesDir(mailboxPath)
	if msgDir == "" {
		return "", nil, nil
	}

	entries, err := os.ReadDir(msgDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("read Messages dir: %w", err)
		}
		// Primary Messages/ dir absent (partition-only layout); continue
		// so that partition files are still collected below.
		entries = nil
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && isEmlxFile(e.Name()) {
			files = append(files, filepath.Join(msgDir, e.Name()))
		}
	}

	// Walk numeric partition dirs in Data/ (parent of Messages/).
	// Only enter digit dirs (0-9) to avoid re-collecting from the
	// primary Messages/ dir which was already handled above.
	dataDir := filepath.Dir(msgDir)
	if filepath.Base(dataDir) == "Data" {
		topEntries, readErr := os.ReadDir(dataDir)
		if readErr == nil {
			for _, e := range topEntries {
				if e.IsDir() && isDigitDir(e.Name()) {
					collectPartitionFiles(
						filepath.Join(dataDir, e.Name()), &files,
					)
				}
			}
		}
	}

	sort.Strings(files)
	return msgDir, files, nil
}

// collectPartitionFiles recursively walks dir for Messages/ subdirs
// and numeric partition dirs (0-9), appending absolute .emlx file
// paths to files.
func collectPartitionFiles(dir string, files *[]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "Messages" {
			msgDir := filepath.Join(dir, name)
			msgs, err := os.ReadDir(msgDir)
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if !m.IsDir() && isEmlxFile(m.Name()) {
					*files = append(*files, filepath.Join(msgDir, m.Name()))
				}
			}
		} else if isDigitDir(name) {
			collectPartitionFiles(filepath.Join(dir, name), files)
		}
	}
}
