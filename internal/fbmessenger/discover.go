package fbmessenger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ThreadDir is one discovered DYI thread directory ready for parsing.
type ThreadDir struct {
	// Path is the absolute filesystem path to the thread directory.
	// For E2EE flat exports, this is the parent directory containing
	// the JSON file.
	Path string
	// FilePath is set for E2EE flat exports where each thread is a
	// single JSON file (e.g. "Name_N.json"). Empty for DYI exports.
	FilePath string
	// Section is the DYI section name (e.g. "inbox", "archived_threads").
	Section string
	// Name is the thread directory basename (e.g. "testuser_ABC123XYZ").
	// For E2EE exports, this is the filename without extension.
	Name string
	// Format is "json", "html", "both", or "e2ee_json".
	Format string
}

// knownSections are the DYI subdirectories we walk for threads.
var knownSections = []string{
	"inbox",
	"archived_threads",
	"filtered_threads",
	"message_requests",
	"e2ee_cutover",
}

// messagesRootCandidates returns candidate "messages" roots inside a DYI
// export, handling the post-2024 ("your_activity_across_facebook/messages"),
// 2025+ ("your_facebook_activity/messages"), and pre-2024 ("messages") layouts.
func messagesRootCandidates(root string) []string {
	return []string{
		filepath.Join(root, "your_activity_across_facebook", "messages"),
		filepath.Join(root, "your_facebook_activity", "messages"),
		filepath.Join(root, "messages"),
	}
}

// Discover walks a DYI export root and returns one ThreadDir per thread
// directory found, sorted by (Section, Name). Absolute and relative root
// inputs yield the same logical result (paths are converted to absolute).
func Discover(root string) ([]ThreadDir, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: abs root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fbmessenger: root is not a directory: %s", absRoot)
	}

	var out []ThreadDir
	scannedE2EE := make(map[string]bool)
	for _, candidate := range messagesRootCandidates(absRoot) {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		// Check for DYI section subdirectories.
		for _, section := range knownSections {
			sectionDir := filepath.Join(candidate, section)
			entries, err := os.ReadDir(sectionDir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				name := entry.Name()
				if strings.HasPrefix(name, ".") {
					continue
				}
				threadPath := filepath.Join(sectionDir, name)
				format, ok := detectFormat(threadPath)
				if !ok {
					continue
				}
				out = append(out, ThreadDir{
					Path:    threadPath,
					Section: section,
					Name:    name,
					Format:  format,
				})
			}
		}
		// Check for E2EE flat export: *.json files directly in the
		// messages root (no section subdirectories).
		if !scannedE2EE[candidate] {
			scannedE2EE[candidate] = true
			out = append(out, discoverE2EEFlat(candidate)...)
		}
	}

	// Also check if absRoot itself is an E2EE flat export directory
	// (user passed the messages dir directly).
	if !scannedE2EE[absRoot] {
		out = append(out, discoverE2EEFlat(absRoot)...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// discoverE2EEFlat scans a directory for E2EE flat export files: JSON
// files like "Name_N.json" sitting directly in the directory (not inside
// section subdirectories). Each file becomes one ThreadDir with Format
// "e2ee_json".
func discoverE2EEFlat(dir string) []ThreadDir {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ThreadDir
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// Skip known non-thread JSON files.
		if isKnownMetadataFile(name) {
			continue
		}
		full := filepath.Join(dir, name)
		// Probe the top-level JSON shape via a streaming decoder so
		// only actual threads enter the indexed list. Keeping the list
		// stable across runs matters because per-thread checkpoints
		// resume by index — if metadata files joined the list one run
		// and dropped the next (e.g. after a Facebook DYI schema change
		// or an allowlist update) the saved index would point past the
		// next real thread. On any I/O or JSON error we fall through
		// and include the file so ParseE2EEJSONFile can classify it
		// (corrupt files still raise ErrCorruptJSON instead of being
		// silently dropped at discovery).
		if shape := probeE2EEShape(full); shape == e2eeShapeNotThread {
			continue
		}
		threadName := strings.TrimSuffix(name, ".json")
		out = append(out, ThreadDir{
			Path:     dir,
			FilePath: full,
			Section:  "e2ee_cutover",
			Name:     threadName,
			Format:   "e2ee_json",
		})
	}
	return out
}

type e2eeShape int

const (
	// e2eeShapeUnknown: the probe couldn't read or decode the file; the
	// caller should include it so the full parser can classify it.
	e2eeShapeUnknown e2eeShape = iota
	// e2eeShapeThread: the file is a JSON object with at least both of
	// "participants" and "messages" at the top level.
	e2eeShapeThread
	// e2eeShapeNotThread: the file is valid JSON but not a thread —
	// non-object shape (array/scalar), or an object missing both
	// "participants" and "messages". Objects that have exactly one of
	// the two are reported as e2eeShapeUnknown so the parser can raise
	// ErrCorruptJSON rather than being silently dropped.
	e2eeShapeNotThread
)

// probeE2EEShape classifies the top-level shape of a candidate E2EE
// flat-export JSON file without decoding the entire body. It streams
// tokens with json.Decoder and stops as soon as both "participants"
// and "messages" keys have been seen.
func probeE2EEShape(filePath string) e2eeShape {
	f, err := os.Open(filePath)
	if err != nil {
		return e2eeShapeUnknown
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil {
		return e2eeShapeUnknown
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return e2eeShapeNotThread
	}
	var hasP, hasM bool
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return e2eeShapeUnknown
		}
		key, ok := tok.(string)
		if !ok {
			return e2eeShapeUnknown
		}
		if key == "participants" {
			hasP = true
		}
		if key == "messages" {
			hasM = true
		}
		if hasP && hasM {
			return e2eeShapeThread
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return e2eeShapeUnknown
		}
	}
	if !hasP && !hasM {
		return e2eeShapeNotThread
	}
	// Object with exactly one of the two keys — let the parser raise
	// ErrCorruptJSON rather than silently dropping it.
	return e2eeShapeUnknown
}

// isKnownMetadataFile returns true for JSON filenames that are DYI
// metadata rather than thread exports.
func isKnownMetadataFile(name string) bool {
	switch name {
	case "autofill_information.json",
		"chat_invites_received.json",
		"community_chats_settings.json",
		"encrypted_messaging_backup_settings.json",
		"information_about_your_devices.json",
		"messaging_settings.json",
		"messenger_active_status_platform_settings.json",
		"messenger_active_status_settings.json",
		"messenger_ui_settings.json",
		"secret_conversations.json",
		"support_messages.json",
		"your_chat_settings_on_web.json",
		"your_end-to-end_encryption_enabled_messenger_device.json",
		"your_messenger_app_install_information.json",
		"your_responsiveness_in_messaging_threads.json":
		return true
	}
	return false
}

// detectFormat inspects a thread directory and reports whether it has a
// numbered message_<N>.json, a message_*.html, or both. Directories with
// neither are ignored (returns ok=false). The JSON match mirrors
// ParseJSONThread's regex so a thread with only an unnumbered JSON
// sibling (e.g. message_final.json) is not misclassified as JSON and
// does not crowd out a sibling HTML file.
func detectFormat(threadPath string) (string, bool) {
	entries, err := os.ReadDir(threadPath)
	if err != nil {
		return "", false
	}
	hasJSON, hasHTML := false, false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "autofill_information.json" {
			continue
		}
		if reMessageFile.MatchString(name) {
			hasJSON = true
		} else if strings.HasPrefix(name, "message_") && strings.HasSuffix(name, ".html") {
			hasHTML = true
		}
	}
	switch {
	case hasJSON && hasHTML:
		return "both", true
	case hasJSON:
		return "json", true
	case hasHTML:
		return "html", true
	}
	return "", false
}
