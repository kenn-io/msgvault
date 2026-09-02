package slack

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var slackdumpDailyFileRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.json$`)

// slackdumpExport provides the parts of a Slackdump standard export needed by
// the importer. Both directories and ZIP archives expose the same fs.FS view.
type slackdumpExport struct {
	fsys                fs.FS
	name                string
	standardAttachments map[string]string
	close               func() error
}

func openSlackdumpExport(name string) (*slackdumpExport, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("open Slackdump export %s: %w", name, err)
	}

	if info.IsDir() {
		root, err := os.OpenRoot(name)
		if err != nil {
			return nil, fmt.Errorf("open Slackdump export %s: %w", name, err)
		}
		return newSlackdumpExport(root.FS(), name, root.Close)
	}

	zr, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("open Slackdump export %s: %w", name, err)
	}
	root, err := slackdumpArchiveRoot(zr)
	if err != nil {
		_ = zr.Close()
		return nil, fmt.Errorf("open Slackdump export %s: %w", name, err)
	}
	return newSlackdumpExport(root, name, zr.Close)
}

func newSlackdumpExport(fsys fs.FS, name string, closeFn func() error) (*slackdumpExport, error) {
	attachments, err := slackdumpStandardAttachmentIndex(fsys)
	if err != nil {
		_ = closeFn()
		return nil, fmt.Errorf("open Slackdump export %s: index attachments: %w", name, err)
	}
	return &slackdumpExport{
		fsys: fsys, name: name, standardAttachments: attachments, close: closeFn,
	}, nil
}

// slackdumpStandardAttachmentIndex mirrors Slackdump's workspace-wide file-ID
// lookup. A file can be shared into more than one conversation while its bytes
// appear under only one conversation's attachments directory.
func slackdumpStandardAttachmentIndex(fsys fs.FS) (map[string]string, error) {
	index := make(map[string]string)
	err := fs.WalkDir(fsys, ".", func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !slackdumpPathContainsDirectory(filename, "attachments") {
			return nil
		}
		fileID, _, found := strings.Cut(path.Base(filename), "-")
		if !found || !slackdumpValidPathSegment(fileID) {
			return nil
		}
		index[fileID] = filename
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk Slackdump export for attachments: %w", err)
	}
	return index, nil
}

func slackdumpPathContainsDirectory(filename, directory string) bool {
	parts := strings.Split(filename, "/")
	return slices.Contains(parts[:len(parts)-1], directory)
}

func slackdumpValidPathSegment(value string) bool {
	return value != "" && value != "." && fs.ValidPath(value) && !strings.Contains(value, "/")
}

// slackdumpArchiveRoot accepts both Slackdump ZIPs whose indexes are at the
// archive root and ZIPs wrapped in a single top-level directory.
func slackdumpArchiveRoot(zr *zip.ReadCloser) (fs.FS, error) {
	if _, err := fs.Stat(zr, "users.json"); err == nil {
		return zr, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat Slackdump archive users.json: %w", err)
	}

	entries, err := fs.ReadDir(zr, ".")
	if err != nil {
		return nil, fmt.Errorf("read Slackdump archive root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, err := fs.Sub(zr, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("open Slackdump archive directory %s: %w", entry.Name(), err)
		}
		if _, err := fs.Stat(candidate, "users.json"); err == nil {
			return candidate, nil
		}
	}
	return zr, nil
}

func (e *slackdumpExport) Close() error {
	if e == nil || e.close == nil {
		return nil
	}
	return e.close()
}

func (e *slackdumpExport) users() ([]User, error) {
	var users []User
	_, err := e.readJSON("users.json", &users, true)
	return users, err
}

func (e *slackdumpExport) conversations(meUserID string) ([]Conversation, error) {
	var conversations []Conversation
	for _, catalog := range []string{"channels.json", "groups.json", "mpims.json"} {
		var entries []Conversation
		if _, err := e.readJSON(catalog, &entries, false); err != nil {
			return nil, err
		}
		conversations = append(conversations, entries...)
	}

	var dms []struct {
		ID      string   `json:"id"`
		User    string   `json:"user"`
		Members []string `json:"members"`
	}
	if found, err := e.readJSON("dms.json", &dms, false); err != nil {
		return nil, err
	} else if found {
		for _, dm := range dms {
			peer := dm.User
			if peer == "" {
				peer = slackdumpDMPeer(dm.Members, meUserID)
			}
			conversations = append(conversations, Conversation{
				ID:   dm.ID,
				IsIM: true,
				User: peer,
			})
		}
	}

	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].ID < conversations[j].ID
	})
	return conversations, nil
}

func slackdumpDMPeer(members []string, meUserID string) string {
	for _, member := range members {
		if member != "" && member != meUserID {
			return member
		}
	}
	for _, member := range members {
		if member != "" {
			return member
		}
	}
	return ""
}

func (e *slackdumpExport) messages(conversation Conversation) ([]Message, error) {
	var messages []Message
	err := e.walkMessages(conversation, func(message *Message) error {
		messages = append(messages, *message)
		return nil
	})
	return messages, err
}

func (e *slackdumpExport) walkMessages(conversation Conversation, visit func(*Message) error) error {
	directory := conversation.Name
	if conversation.IsIM {
		directory = conversation.ID
	}
	if directory == "" || !fs.ValidPath(directory) {
		return fmt.Errorf("read Slackdump export %s: invalid conversation directory %q", e.name, directory)
	}

	entries, err := fs.ReadDir(e.fsys, directory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return e.pathError(directory, err)
	}
	// Slackdump export (both Standard and Mattermost layouts) flattens roots
	// and replies into date files. Timestamp-named per-thread files belong to
	// Slackdump's separate dump format and are intentionally ignored here.
	var dailyFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && slackdumpDailyFileRE.MatchString(entry.Name()) {
			dailyFiles = append(dailyFiles, entry.Name())
		}
	}
	sort.Strings(dailyFiles)

	for _, filename := range dailyFiles {
		var daily []Message
		if _, err := e.readJSON(path.Join(directory, filename), &daily, true); err != nil {
			return err
		}
		sort.SliceStable(daily, func(i, j int) bool {
			return tsLess(daily[i].TS, daily[j].TS)
		})
		for index := range daily {
			if err := visit(&daily[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *slackdumpExport) attachment(conversation Conversation, file File) ([]byte, bool, error) {
	return e.attachmentWithLimit(conversation, file, defaultMaxMediaBytes)
}

func (e *slackdumpExport) attachmentWithLimit(
	conversation Conversation,
	file File,
	maxBytes int64,
) ([]byte, bool, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxMediaBytes
	}
	if !slackdumpValidPathSegment(file.ID) {
		return nil, false, fmt.Errorf("read Slackdump export %s: invalid attachment ID %q", e.name, file.ID)
	}
	directory := conversation.Name
	if conversation.IsIM {
		directory = conversation.ID
	}
	sanitized := slackdumpSanitizeFilename(file.Name)
	candidates := []string{
		path.Join(directory, "attachments", file.ID+"-"+sanitized),
	}
	if indexed := e.standardAttachments[file.ID]; indexed != "" && indexed != candidates[0] {
		candidates = append(candidates, indexed)
	}
	candidates = append(candidates, path.Join("__uploads", file.ID, sanitized))

	for _, filename := range candidates {
		if !fs.ValidPath(filename) {
			continue
		}
		content, found, err := e.readAttachmentFile(filename, maxBytes)
		if err == nil && found {
			return content, true, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (e *slackdumpExport) readAttachmentFile(filename string, maxBytes int64) ([]byte, bool, error) {
	file, err := e.fsys.Open(filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, e.pathError(filename, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, false, e.pathError(filename, err)
	}
	if info.Size() > maxBytes {
		return nil, false, e.pathError(filename, fmt.Errorf("%d bytes exceeds %d-byte limit: %w", info.Size(), maxBytes, ErrAssetTooLarge))
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, false, e.pathError(filename, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, false, e.pathError(filename, ErrAssetTooLarge)
	}
	return content, true, nil
}

func (e *slackdumpExport) readJSON(filename string, destination any, required bool) (bool, error) {
	content, err := fs.ReadFile(e.fsys, filename)
	if err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, e.pathError(filename, err)
	}
	if err := json.Unmarshal(content, destination); err != nil {
		return true, e.pathError(filename, err)
	}
	return true, nil
}

func (e *slackdumpExport) pathError(filename string, err error) error {
	return fmt.Errorf("slackdump export %s/%s: %w", e.name, filename, err)
}

var slackdumpUnsafeFilenameRE = regexp.MustCompile(`[<>:"/\\|?*]`)

var slackdumpReservedFilenames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// slackdumpSanitizeFilename mirrors Slackdump's portable filename mapping.
func slackdumpSanitizeFilename(name string) string {
	safe := slackdumpUnsafeFilenameRE.ReplaceAllString(name, "_")
	safe = strings.TrimRight(safe, " .")
	base, _, _ := strings.Cut(safe, ".")
	if _, reserved := slackdumpReservedFilenames[strings.ToUpper(base)]; reserved {
		safe = "_" + safe
	}
	if safe == "" {
		return "unnamed_file"
	}
	return safe
}
