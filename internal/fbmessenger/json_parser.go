package fbmessenger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rawJSONExport is the shape of a single message_*.json DYI file.
type rawJSONExport struct {
	Participants []rawJSONParticipant `json:"participants"`
	Messages     []rawJSONMessage     `json:"messages"`
	Title        string               `json:"title"`
	ThreadType   string               `json:"thread_type"`
	ThreadPath   string               `json:"thread_path"`
}

type rawJSONParticipant struct {
	Name string `json:"name"`
}

type rawJSONPhoto struct {
	URI               string `json:"uri"`
	CreationTimestamp int64  `json:"creation_timestamp"`
}

type rawJSONSticker struct {
	URI string `json:"uri"`
}

type rawJSONShare struct {
	Link      string `json:"link"`
	ShareText string `json:"share_text"`
}

type rawJSONReaction struct {
	Reaction string `json:"reaction"`
	Actor    string `json:"actor"`
}

type rawJSONMessage struct {
	SenderName   string            `json:"sender_name"`
	TimestampMs  int64             `json:"timestamp_ms"`
	Content      string            `json:"content"`
	Type         string            `json:"type"`
	Photos       []rawJSONPhoto    `json:"photos"`
	Videos       []rawJSONPhoto    `json:"videos"`
	Audio        []rawJSONPhoto    `json:"audio_files"`
	Files        []rawJSONPhoto    `json:"files"`
	Gifs         []rawJSONPhoto    `json:"gifs"`
	Sticker      *rawJSONSticker   `json:"sticker"`
	Share        *rawJSONShare     `json:"share"`
	CallDuration *int64            `json:"call_duration"`
	Missed       bool              `json:"missed"`
	Reactions    []rawJSONReaction `json:"reactions"`
}

var reMessageFile = regexp.MustCompile(`^message_(\d+)\.json$`)

// ParseJSONThread parses every message_*.json file in a DYI thread
// directory and returns a populated Thread. rootDir is the DYI export
// root; threadDir is the thread path returned by Discover.
func ParseJSONThread(rootDir, threadDir string) (*Thread, error) {
	entries, err := os.ReadDir(threadDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: read thread dir: %w", err)
	}

	type numbered struct {
		name string
		num  int
	}
	var files []numbered
	var badSiblings []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "message_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		m := reMessageFile.FindStringSubmatch(name)
		if m == nil {
			// Unrecognized sibling (e.g. message_final.json). Skip it and
			// record the bad name so the importer can log + count it
			// without aborting the entire thread.
			badSiblings = append(badSiblings, name)
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			// Regex guarantees \d+, so this is effectively unreachable;
			// treat it the same as an unrecognized sibling.
			badSiblings = append(badSiblings, name)
			continue
		}
		files = append(files, numbered{name: name, num: n})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("fbmessenger: no message_*.json files in %s", threadDir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].num < files[j].num })

	// Decode every file. The DYI format emits each file as a complete
	// object with `participants` and `messages`. We take participants
	// from the lowest-numbered file, and concatenate all messages.
	var thread Thread
	thread.Format = "json"
	thread.DirName = filepath.Base(threadDir)
	thread.BadSiblings = badSiblings
	var rawConcat []rawJSONMessage
	var title string
	var participants []Participant

	for i, f := range files {
		full := filepath.Join(threadDir, f.name)
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("fbmessenger: read %s: %w", f.name, err)
		}
		var decoded rawJSONExport
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorruptJSON, f.name, err)
		}
		if i == 0 {
			title = DecodeMojibake(decoded.Title)
			for _, p := range decoded.Participants {
				participants = append(participants, Participant{Name: DecodeMojibake(p.Name)})
			}
		}
		rawConcat = append(rawConcat, decoded.Messages...)
	}

	// Deduplicate participants (Facebook sometimes repeats).
	seen := make(map[string]bool, len(participants))
	uniq := participants[:0]
	for _, p := range participants {
		if seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		uniq = append(uniq, p)
	}
	participants = uniq

	// Sort concatenated messages chronologically.
	sort.SliceStable(rawConcat, func(i, j int) bool {
		return rawConcat[i].TimestampMs < rawConcat[j].TimestampMs
	})

	thread.Title = title
	thread.Participants = participants
	if len(participants) <= 2 {
		thread.ConvType = "direct_chat"
	} else {
		thread.ConvType = "group_chat"
	}

	// Store raw bytes: we concatenate the original JSON files into a
	// JSON array so the stored raw is a self-contained round-trippable
	// payload. Callers that want the exact per-file text can still read
	// from disk.
	var rawBuf strings.Builder
	rawBuf.WriteByte('[')
	for i, f := range files {
		if i > 0 {
			rawBuf.WriteByte(',')
		}
		data, err := os.ReadFile(filepath.Join(threadDir, f.name))
		if err != nil {
			return nil, err
		}
		rawBuf.Write(data)
	}
	rawBuf.WriteByte(']')
	thread.RawBytes = []byte(rawBuf.String())

	// Render each message.
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: abs root: %w", err)
	}
	thread.Messages = make([]Message, 0, len(rawConcat))
	for idx, m := range rawConcat {
		msg := Message{
			Index:      idx,
			SenderName: DecodeMojibake(m.SenderName),
			Type:       m.Type,
		}
		if m.TimestampMs > 0 {
			msg.SentAt = time.UnixMilli(m.TimestampMs).UTC()
		}
		msg.Body = renderJSONBody(m)
		msg.Attachments = resolveAttachments(absRoot, m)
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
	return &thread, nil
}

// renderJSONBody computes the body string for one DYI message, following
// the placeholder and rendering rules from plan decisions D9/D10.
func renderJSONBody(m rawJSONMessage) string {
	content := DecodeMojibake(m.Content)
	switch strings.ToLower(m.Type) {
	case "call":
		return renderCallBody(m)
	case "share":
		return renderShareBody(m, content)
	case "unsubscribe":
		if content == "" {
			return "[system]"
		}
		return "[system] " + content
	}
	if content != "" {
		return content
	}
	if m.Sticker != nil && m.Sticker.URI != "" {
		return "[sticker]"
	}
	if len(m.Photos) > 0 {
		return "[photo]"
	}
	if len(m.Videos) > 0 {
		return "[video]"
	}
	if len(m.Audio) > 0 {
		return "[audio]"
	}
	if len(m.Gifs) > 0 {
		return "[gif]"
	}
	if len(m.Files) > 0 {
		return "[file]"
	}
	return ""
}

func renderCallBody(m rawJSONMessage) string {
	dur := int64(0)
	if m.CallDuration != nil {
		dur = *m.CallDuration
	}
	if m.Missed || dur == 0 {
		return "[call: missed, 0s]"
	}
	minutes := dur / 60
	seconds := dur % 60
	if minutes > 0 {
		return fmt.Sprintf("[call: %dm %ds]", minutes, seconds)
	}
	return fmt.Sprintf("[call: %ds]", seconds)
}

func renderShareBody(m rawJSONMessage, content string) string {
	var b strings.Builder
	b.WriteString("[shared link] ")
	if m.Share != nil {
		b.WriteString(m.Share.Link)
		text := DecodeMojibake(m.Share.ShareText)
		if text != "" {
			b.WriteByte('\n')
			b.WriteString(text)
		}
	}
	if content != "" {
		b.WriteByte('\n')
		b.WriteString(content)
	}
	return b.String()
}

// reReactionSuffix matches the "\n\n[reacted: ...]" summary appended by
// appendReactionSummary. It is anchored to the end of the string because
// callers append the suffix last.
var reReactionSuffix = regexp.MustCompile(`(?s)\n\n\[reacted: .*\]\z`)

// stripReactionSuffix removes the trailing reaction summary if present,
// returning the raw body text that the parser extracted before the
// suffix was appended. HTML exports do not carry this suffix, so this
// helper is what the convergence test uses to compare JSON and HTML on
// their common ground (the message body proper).
func stripReactionSuffix(body string) string {
	return reReactionSuffix.ReplaceAllString(body, "")
}

func appendReactionSummary(body string, rs []Reaction) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s (%s)", r.Reaction, r.Actor))
	}
	suffix := "\n\n[reacted: " + strings.Join(parts, ", ") + "]"
	if body == "" {
		return suffix
	}
	return body + suffix
}

// resolveAttachments resolves every attachment URI for one DYI message,
// guarding against paths that escape rootDir.
func resolveAttachments(absRoot string, m rawJSONMessage) []Attachment {
	var out []Attachment
	add := func(kind string, items []rawJSONPhoto) {
		for _, it := range items {
			if it.URI == "" {
				continue
			}
			abs := resolveAttachmentURI(absRoot, it.URI)
			out = append(out, Attachment{
				URI:      it.URI,
				AbsPath:  abs,
				Kind:     kind,
				Filename: filepath.Base(it.URI),
				MimeType: guessMime(it.URI),
			})
		}
	}
	add("photo", m.Photos)
	add("video", m.Videos)
	add("audio", m.Audio)
	add("file", m.Files)
	add("gif", m.Gifs)
	if m.Sticker != nil && m.Sticker.URI != "" {
		abs := resolveAttachmentURI(absRoot, m.Sticker.URI)
		out = append(out, Attachment{
			URI:      m.Sticker.URI,
			AbsPath:  abs,
			Kind:     "sticker",
			Filename: filepath.Base(m.Sticker.URI),
			MimeType: guessMime(m.Sticker.URI),
		})
	}
	return out
}

// resolveAttachmentURI returns the absolute path for a DYI attachment URI
// relative to absRoot. DYI URIs are typically rooted at the "messages/"
// parent, which in the post-2024 layout lives at
// absRoot/your_activity_across_facebook. We try both candidates and
// return the first that stays inside absRoot and refers to an existing
// file. Empty string if the path escapes the root or nothing matches.
func resolveAttachmentURI(absRoot, uri string) string {
	cleaned := filepath.Clean(uri)
	if filepath.IsAbs(cleaned) {
		return ""
	}
	// Reject early if the cleaned path tries to escape.
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return ""
	}
	candidates := []string{
		filepath.Join(absRoot, "your_activity_across_facebook", cleaned),
		filepath.Join(absRoot, "your_facebook_activity", cleaned),
		filepath.Join(absRoot, cleaned),
	}
	var firstInside string
	for _, full := range candidates {
		absFull, err := filepath.Abs(full)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) && absFull != absRoot {
			continue
		}
		if firstInside == "" {
			firstInside = absFull
		}
		if _, err := os.Stat(absFull); err == nil {
			return absFull
		}
	}
	// No candidate existed on disk; return the first inside-root candidate
	// so callers can still record the (missing) row with the intended path.
	return firstInside
}

func guessMime(uri string) string {
	ext := strings.ToLower(filepath.Ext(uri))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".pdf":
		return "application/pdf"
	case ".webp":
		return "image/webp"
	}
	return ""
}
