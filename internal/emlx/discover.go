package emlx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiscoveryError reports failed filesystem reads. Discovery may still return
// readable mailboxes alongside this error; callers must report the failures.
type DiscoveryError struct {
	Errors []error
}

func (e *DiscoveryError) Error() string {
	message := "emlx discover: " + errors.Join(e.Errors...).Error()
	if errors.Is(e, os.ErrPermission) {
		message += "; on macOS, check Full Disk Access for the process running the import (including the daemon), then restart the daemon from that context"
	}
	return message
}

func (e *DiscoveryError) Unwrap() []error { return e.Errors }

type discovery struct {
	errors []error
	seen   map[string]bool
}

func (d *discovery) record(err error) {
	if err == nil {
		return
	}
	key := err.Error()
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		key = pathErr.Path
	}
	if !d.seen[key] {
		d.seen[key] = true
		d.errors = append(d.errors, err)
	}
}

func (d *discovery) err() error {
	if len(d.errors) == 0 {
		return nil
	}
	return &DiscoveryError{Errors: d.errors}
}

func (d *discovery) readDir(path string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	d.record(err)
	return entries, err
}

// statOptional probes optional layout components. Missing components are normal;
// access failures are not evidence that a layout is absent.
func (d *discovery) statOptional(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if !os.IsNotExist(err) {
		d.record(err)
	}
	return info, err
}

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
// A non-nil error can accompany partial results. An empty result with an error
// must not be treated as an empty source tree.
func DiscoverMailboxes(rootDir string) ([]Mailbox, error) {
	discovery := &discovery{seen: make(map[string]bool)}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("emlx discover: abs path: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		discovery.record(err)
		return nil, discovery.err()
	}
	if !info.IsDir() {
		return nil, fmt.Errorf(
			"emlx discover: %q is not a directory", abs,
		)
	}

	// Auto-detect: if the path itself is a mailbox, import just that one.
	if isMailboxDir(abs) {
		msgDir, files := discovery.listEmlxFiles(abs)
		if len(files) > 0 {
			label := LabelFromPath(filepath.Dir(abs), abs)
			return []Mailbox{{
				Path: abs, MsgDir: msgDir,
				Label: label, Files: files,
			}}, discovery.err()
		}
	}

	var mailboxes []Mailbox
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			discovery.record(err)
			return nil // Preserve readable siblings, but report this failure.
		}
		if !d.IsDir() {
			return nil
		}

		// listEmlxFiles reads Messages/ directly, so skip walking it.
		if d.Name() == "Messages" && path != abs {
			// Check readability even when no enclosing mailbox was recognized.
			_, _ = discovery.readDir(path)
			return filepath.SkipDir
		}

		// Skip non-mailbox directories.
		if !isMailboxDir(path) {
			return nil
		}

		msgDir, files := discovery.listEmlxFiles(path)
		if len(files) == 0 {
			return nil
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
	return mailboxes, discovery.err()
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

	return true
}

// findMessagesDir locates the Messages/ directory within a .mbox.
// Returns "" if none found. Checks both legacy (Messages/) and
// modern V10 (<GUID>/Data/Messages/) layouts. When both exist,
// prefers whichever contains .emlx files (directly or in partitions).
func (d *discovery) findMessagesDir(mailboxPath string) string {
	var candidates []string
	entries, err := d.readDir(mailboxPath)
	if err != nil {
		return ""
	}

	// Legacy: direct Messages/ subdirectory.
	legacy := filepath.Join(mailboxPath, "Messages")
	if info, err := d.statOptional(legacy); err == nil && info.IsDir() {
		candidates = append(candidates, legacy)
	}

	// Modern V10: <subdir>/Data/Messages/ subdirectory.
	// Also handles partition-only layouts where Data/Messages/ doesn't exist.
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "Messages" {
				continue
			}
			dataDir := filepath.Join(mailboxPath, e.Name(), "Data")
			dataStat, statErr := d.statOptional(dataDir)
			if statErr != nil || !dataStat.IsDir() {
				continue
			}
			modern := filepath.Join(dataDir, "Messages")
			msgStat, statErr := d.statOptional(modern)
			if statErr == nil && msgStat.IsDir() {
				candidates = append(candidates, modern)
			} else if d.hasEmlxFilesInPartitions(dataDir) {
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
		if d.hasEmlxFiles(dir) {
			return dir
		}
		// For V10 layout the parent is Data/; check partitions there.
		dataDir := filepath.Dir(dir)
		if filepath.Base(dataDir) == "Data" &&
			d.hasEmlxFilesInPartitions(dataDir) {
			return dir
		}
	}

	// No candidate has files; return first for isMailboxDir.
	return candidates[0]
}

// hasEmlxFiles returns true if dir contains at least one .emlx file
// (including *.partial.emlx).
func (d *discovery) hasEmlxFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A partition-only layout has no primary Messages directory.
		if !os.IsNotExist(err) {
			d.record(err)
		}
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
func (d *discovery) hasEmlxFilesInPartitions(dir string) bool {
	entries, err := d.readDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "Messages" {
			if d.hasEmlxFiles(filepath.Join(dir, name)) {
				return true
			}
		} else if isDigitDir(name) {
			if d.hasEmlxFilesInPartitions(filepath.Join(dir, name)) {
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
	return strings.HasSuffix(strings.ToLower(name), ".emlx")
}

// IsPartial reports whether name (or path) is a *.partial.emlx file.
// Apple Mail uses the .partial.emlx form for IMAP/Gmail messages whose
// body is fully downloaded but whose attachments are not cached locally;
// the RFC822 payload is complete apart from the attachment parts.
func IsPartial(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".partial.emlx")
}

// fullEmlxName returns the non-partial counterpart of a partial name
// (e.g. "513139.partial.emlx" -> "513139.emlx").
func fullEmlxName(name string) string {
	return name[:len(name)-len(".partial.emlx")] + ".emlx"
}

// selectEmlxNames returns the .emlx file names among entries, skipping
// a N.partial.emlx when its fully-downloaded N.emlx counterpart exists
// in the same directory (the full copy supersedes the partial).
func selectEmlxNames(entries []os.DirEntry) []string {
	full := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() && isEmlxFile(e.Name()) && !IsPartial(e.Name()) {
			full[strings.ToLower(e.Name())] = true
		}
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isEmlxFile(e.Name()) {
			continue
		}
		if IsPartial(e.Name()) &&
			full[strings.ToLower(fullEmlxName(e.Name()))] {
			continue
		}
		names = append(names, e.Name())
	}
	return names
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
// Returns ("", nil) if no Messages directory is found, recording read failures.
func (d *discovery) listEmlxFiles(
	mailboxPath string,
) (string, []string) {
	msgDir := d.findMessagesDir(mailboxPath)
	if msgDir == "" {
		return "", nil
	}

	entries, err := os.ReadDir(msgDir)
	if err != nil {
		if !os.IsNotExist(err) {
			d.record(err)
		}
		// Primary Messages/ dir absent (partition-only layout); continue
		// so that partition files are still collected below.
		entries = nil
	}

	var files []string
	for _, name := range selectEmlxNames(entries) {
		files = append(files, filepath.Join(msgDir, name))
	}

	// Walk numeric partition dirs in Data/ (parent of Messages/).
	// Only enter digit dirs (0-9) to avoid re-collecting from the
	// primary Messages/ dir which was already handled above.
	dataDir := filepath.Dir(msgDir)
	if filepath.Base(dataDir) == "Data" {
		topEntries, readErr := d.readDir(dataDir)
		if readErr == nil {
			for _, e := range topEntries {
				if e.IsDir() && isDigitDir(e.Name()) {
					d.collectPartitionFiles(
						filepath.Join(dataDir, e.Name()), &files,
					)
				}
			}
		}
	}

	sort.Strings(files)
	return msgDir, files
}

// collectPartitionFiles recursively walks dir for Messages/ subdirs
// and numeric partition dirs (0-9), appending absolute .emlx file
// paths to files.
func (d *discovery) collectPartitionFiles(dir string, files *[]string) {
	entries, err := d.readDir(dir)
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
			msgs, err := d.readDir(msgDir)
			if err != nil {
				continue
			}
			for _, n := range selectEmlxNames(msgs) {
				*files = append(*files, filepath.Join(msgDir, n))
			}
		} else if isDigitDir(name) {
			d.collectPartitionFiles(filepath.Join(dir, name), files)
		}
	}
}
