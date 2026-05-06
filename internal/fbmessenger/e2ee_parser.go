package fbmessenger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rawE2EEExport is the shape of a single E2EE thread JSON file.
type rawE2EEExport struct {
	Participants []string         `json:"participants"`
	ThreadName   string           `json:"threadName"`
	Messages     []rawE2EEMessage `json:"messages"`
}

type rawE2EEMedia struct {
	URI string `json:"uri"`
}

type rawE2EEReaction struct {
	Actor    string `json:"actor"`
	Reaction string `json:"reaction"`
}

type rawE2EEMessage struct {
	SenderName string            `json:"senderName"`
	Text       string            `json:"text"`
	Timestamp  int64             `json:"timestamp"`
	Type       string            `json:"type"`
	IsUnsent   bool              `json:"isUnsent"`
	Media      []rawE2EEMedia    `json:"media"`
	Reactions  []rawE2EEReaction `json:"reactions"`
}

// missingThreadKey names the missing top-level key for the corrupt-
// thread error message, given which of "participants"/"messages" is
// present.
func missingThreadKey(hasP, hasM bool) string {
	switch {
	case !hasP:
		return "participants"
	case !hasM:
		return "messages"
	}
	return ""
}

// ParseE2EEJSONFile parses a single E2EE flat-export JSON file and
// returns a populated Thread. rootDir is the export root (used for
// resolving media paths); filePath is the absolute path to the JSON file.
func ParseE2EEJSONFile(rootDir, filePath string) (*Thread, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: read e2ee file: %w", err)
	}
	// Classify the top-level shape before doing the strongly-typed
	// decode. discover.probeE2EEShape already filters out obvious
	// non-thread JSON, but the parser defends against direct callers
	// and against a discovery layer that defers to it on ambiguous
	// shapes. Distinguish three cases:
	//   - neither "participants" nor "messages": not a thread → silent skip
	//   - exactly one of the two: malformed → ErrCorruptJSON so the
	//     importer logs and counts it
	//   - both present, or non-object: object→full decode; non-object→not a thread
	var top any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorruptJSON, filepath.Base(filePath), err)
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotE2EEThread, filepath.Base(filePath))
	}
	_, hasP := obj["participants"]
	_, hasM := obj["messages"]
	switch {
	case !hasP && !hasM:
		return nil, fmt.Errorf("%w: %s", ErrNotE2EEThread, filepath.Base(filePath))
	case hasP != hasM:
		return nil, fmt.Errorf("%w: %s: missing %s", ErrCorruptJSON, filepath.Base(filePath), missingThreadKey(hasP, hasM))
	}
	var decoded rawE2EEExport
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorruptJSON, filepath.Base(filePath), err)
	}

	thread := &Thread{
		DirName:  strings.TrimSuffix(filepath.Base(filePath), ".json"),
		Title:    DecodeMojibake(decoded.ThreadName),
		Format:   "e2ee_json",
		RawBytes: data,
	}

	// Participants.
	seen := make(map[string]bool, len(decoded.Participants))
	for _, name := range decoded.Participants {
		name = DecodeMojibake(name)
		if seen[name] {
			continue
		}
		seen[name] = true
		thread.Participants = append(thread.Participants, Participant{Name: name})
	}

	if len(thread.Participants) <= 2 {
		thread.ConvType = "direct_chat"
	} else {
		thread.ConvType = "group_chat"
	}

	// Sort messages chronologically.
	sort.SliceStable(decoded.Messages, func(i, j int) bool {
		return decoded.Messages[i].Timestamp < decoded.Messages[j].Timestamp
	})

	// The media directory sits alongside the JSON files.
	mediaDir := filepath.Dir(filePath)
	absMediaDir, err := filepath.Abs(mediaDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: abs media dir: %w", err)
	}

	thread.Messages = make([]Message, 0, len(decoded.Messages))
	msgIdx := 0
	for _, m := range decoded.Messages {
		if m.IsUnsent {
			continue
		}
		msg := Message{
			Index:      msgIdx,
			SenderName: DecodeMojibake(m.SenderName),
			Type:       m.Type,
		}
		msgIdx++
		if m.Timestamp > 0 {
			msg.SentAt = time.UnixMilli(m.Timestamp).UTC()
		}
		msg.Body = renderE2EEBody(m)
		msg.Attachments = resolveE2EEAttachments(absMediaDir, m)
		for _, r := range m.Reactions {
			msg.Reactions = append(msg.Reactions, Reaction{
				Actor:    DecodeMojibake(r.Actor),
				Reaction: DecodeMojibake(r.Reaction),
			})
		}
		if len(msg.Reactions) > 0 {
			msg.Body = appendReactionSummary(msg.Body, msg.Reactions)
		}
		thread.Messages = append(thread.Messages, msg)
	}
	return thread, nil
}

// renderE2EEBody computes the body string for one E2EE message.
func renderE2EEBody(m rawE2EEMessage) string {
	if m.Text != "" {
		return DecodeMojibake(m.Text)
	}
	if len(m.Media) > 0 {
		return "[media]"
	}
	return ""
}

// resolveE2EEAttachments resolves media URIs for an E2EE message.
// E2EE media URIs are relative like "./media/uuid.jpeg".
func resolveE2EEAttachments(absDir string, m rawE2EEMessage) []Attachment {
	var out []Attachment
	for _, media := range m.Media {
		if media.URI == "" {
			continue
		}
		// Strip leading "./" from relative URIs.
		rel := strings.TrimPrefix(media.URI, "./")
		abs := filepath.Join(absDir, rel)
		// Verify the resolved path stays inside absDir.
		absClean, err := filepath.Abs(abs)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(absClean, absDir+string(filepath.Separator)) && absClean != absDir {
			continue
		}
		out = append(out, Attachment{
			URI:      media.URI,
			AbsPath:  absClean,
			Kind:     guessE2EEKind(media.URI),
			Filename: filepath.Base(media.URI),
			MimeType: guessMime(media.URI),
		})
	}
	return out
}

func guessE2EEKind(uri string) string {
	ext := strings.ToLower(filepath.Ext(uri))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp":
		return "photo"
	case ".mp4", ".mov":
		return "video"
	case ".mp3", ".wav", ".ogg":
		return "audio"
	case ".gif":
		return "gif"
	}
	return "file"
}
