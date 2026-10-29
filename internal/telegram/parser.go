package telegram

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	jsontext "encoding/json/jsontext"
	json "encoding/json/v2"
)

// resultFile is the export's machine-readable index; Telegram Desktop's
// documented export layout writes the whole export into this one file.
const resultFile = "result.json"

// ErrNoOwner reports an export that does not identify the exporting user and
// was given no --me override.
var ErrNoOwner = errors.New("telegram: export does not identify the exporting user; pass --me <numeric-id>")

// ExportReader reads one Telegram Desktop export incrementally. Opening
// streams the file once to read the exporting user and a fingerprint of the
// file's bytes; NextChat then yields chats one at a time from a second pass,
// so retained memory is bounded by the largest chat, not the whole export.
//
// The fingerprint identifies the export's content: a checkpoint may only be
// resumed against byte-identical input, because chat and message positions
// are meaningless in a file that changed.
type ExportReader struct {
	file        *os.File
	dec         *jsontext.Decoder
	owner       ownerInfo
	fingerprint string
	// single marks a root that is one chat object rather than a full export
	// with chats/left_chats containers.
	single     bool
	singleRead bool
	// inList marks a decoder positioned inside a chats/left_chats list
	// array.
	inList bool
}

// OpenExport opens root's result.json and reads its identity: the exporting
// user and the SHA-256 fingerprint of the file's bytes.
func OpenExport(root string) (*ExportReader, error) {
	path := filepath.Join(root, resultFile)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("telegram: no %s in %s", resultFile, root)
		}
		return nil, fmt.Errorf("telegram: open %s: %w", path, err)
	}
	r := &ExportReader{file: f}
	if err := r.scanIdentity(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// Close releases the export file.
func (r *ExportReader) Close() error {
	return r.file.Close()
}

// OwnerID is personal_information.user_id, or 0 when the export does not
// identify the exporting user (single-chat exports).
func (r *ExportReader) OwnerID() int64 { return r.owner.ID }

// OwnerName is the exporting user's display name, when known.
func (r *ExportReader) OwnerName() string { return r.owner.Name }

// Fingerprint is the hex SHA-256 of result.json's bytes.
func (r *ExportReader) Fingerprint() string { return r.fingerprint }

// scanIdentity runs the first pass: the owner's identity, whether the root is
// a full export, and the fingerprint of every byte in the file.
func (r *ExportReader) scanIdentity() error {
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("telegram: rewind export: %w", err)
	}
	hash := sha256.New()
	tee := io.TeeReader(r.file, hash)
	owner, hasChatLists, err := scanOwner(jsontext.NewDecoder(tee))
	if err != nil {
		return err
	}
	// The decoder may stop before the file does; drain the rest so the
	// fingerprint covers the whole file.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return fmt.Errorf("telegram: read export tail: %w", err)
	}
	r.owner = owner
	r.single = !hasChatLists
	r.fingerprint = hex.EncodeToString(hash.Sum(nil))
	return nil
}

// NextChat returns the next chat in export order. io.EOF reports the end of
// the export. A full export walks chats.list then left_chats.list; a
// single-chat root yields its one chat.
func (r *ExportReader) NextChat() (*Chat, error) {
	if r.single {
		if r.singleRead {
			return nil, io.EOF
		}
		r.singleRead = true
		if _, err := r.file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("telegram: rewind single chat: %w", err)
		}
		return decodeChatFrom(jsontext.NewDecoder(r.file))
	}
	if r.dec == nil {
		if _, err := r.file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("telegram: rewind export: %w", err)
		}
		r.dec = jsontext.NewDecoder(r.file)
		if err := expectObjectOpen(r.dec, "export"); err != nil {
			return nil, err
		}
	}
	for {
		if r.inList {
			if r.dec.PeekKind() != ']' {
				return decodeChatFrom(r.dec)
			}
			if err := r.closeChatList(); err != nil {
				return nil, err
			}
			r.inList = false
		}
		if r.dec.PeekKind() == '}' {
			if _, err := r.dec.ReadToken(); err != nil {
				return nil, fmt.Errorf("telegram: close export object: %w", err)
			}
			return nil, io.EOF
		}
		keyToken, err := r.dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("telegram: read export key: %w", err)
		}
		switch key := keyToken.String(); key {
		case "chats", "left_chats":
			found, err := r.openChatList(key)
			if err != nil {
				return nil, err
			}
			r.inList = found
		default:
			if err := r.dec.SkipValue(); err != nil {
				return nil, fmt.Errorf("telegram: skip export value: %w", err)
			}
		}
	}
}

// openChatList descends into one chats/left_chats container and positions the
// decoder on the first entry of its list array. found is false when the
// container carries no list.
func (r *ExportReader) openChatList(container string) (found bool, err error) {
	if err := expectObjectOpen(r.dec, container); err != nil {
		return false, err
	}
	for r.dec.PeekKind() != '}' {
		keyToken, err := r.dec.ReadToken()
		if err != nil {
			return false, fmt.Errorf("telegram: read %s key: %w", container, err)
		}
		if keyToken.String() != "list" {
			if err := r.dec.SkipValue(); err != nil {
				return false, fmt.Errorf("telegram: skip %s value: %w", container, err)
			}
			continue
		}
		if r.dec.PeekKind() != '[' {
			return false, fmt.Errorf("telegram: %s.list is not an array", container)
		}
		if _, err := r.dec.ReadToken(); err != nil { // '['
			return false, fmt.Errorf("telegram: open %s.list: %w", container, err)
		}
		return true, nil
	}
	if _, err := r.dec.ReadToken(); err != nil { // container '}'
		return false, fmt.Errorf("telegram: close %s: %w", container, err)
	}
	return false, nil
}

// closeChatList consumes the rest of a list array and its container object.
func (r *ExportReader) closeChatList() error {
	container := "chats"
	if _, err := r.dec.ReadToken(); err != nil { // ']'
		return fmt.Errorf("telegram: close list: %w", err)
	}
	for r.dec.PeekKind() != '}' {
		if _, err := r.dec.ReadToken(); err != nil {
			return fmt.Errorf("telegram: read %s key: %w", container, err)
		}
		if err := r.dec.SkipValue(); err != nil {
			return fmt.Errorf("telegram: skip %s value: %w", container, err)
		}
	}
	if _, err := r.dec.ReadToken(); err != nil { // container '}'
		return fmt.Errorf("telegram: close %s: %w", container, err)
	}
	return nil
}

type ownerInfo struct {
	ID   int64
	Name string
}

// scanOwner streams the top-level object once, reading
// personal_information.user_id and reporting whether the root carries a
// chats/left_chats container (a full export) or is a single chat object.
func scanOwner(dec *jsontext.Decoder) (owner ownerInfo, hasChatLists bool, err error) {
	if err := expectObjectOpen(dec, "export"); err != nil {
		return owner, false, err
	}
	for dec.PeekKind() != '}' {
		keyToken, err := dec.ReadToken()
		if err != nil {
			return owner, false, fmt.Errorf("telegram: read export key: %w", err)
		}
		switch keyToken.String() {
		case "personal_information":
			var info struct {
				UserID    int64  `json:"user_id"`
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
			}
			if err := json.UnmarshalDecode(dec, &info); err != nil {
				return owner, false, fmt.Errorf("telegram: parse personal_information: %w", err)
			}
			owner = ownerInfo{ID: info.UserID, Name: strings.TrimSpace(info.FirstName + " " + info.LastName)}
		case "chats", "left_chats":
			hasChatLists = true
			if err := dec.SkipValue(); err != nil {
				return owner, false, fmt.Errorf("telegram: skip %s: %w", keyToken.String(), err)
			}
		default:
			if err := dec.SkipValue(); err != nil {
				return owner, false, fmt.Errorf("telegram: skip export value: %w", err)
			}
		}
	}
	if _, err := dec.ReadToken(); err != nil { // consume '}'
		return owner, false, fmt.Errorf("telegram: close export object: %w", err)
	}
	return owner, hasChatLists, nil
}

// expectObjectOpen reads and validates the opening object token.
func expectObjectOpen(dec *jsontext.Decoder, what string) error {
	kind := dec.PeekKind()
	if kind != '{' {
		return fmt.Errorf("telegram: %s is not a JSON object", what)
	}
	if _, err := dec.ReadToken(); err != nil {
		return fmt.Errorf("telegram: open %s: %w", what, err)
	}
	return nil
}

// rawChat mirrors the chat object's fields.
type rawChat struct {
	ID       int64        `json:"id"`
	Type     string       `json:"type"`
	Name     string       `json:"name"`
	Messages []rawMessage `json:"messages"`
}

// rawMessage mirrors one messages array entry. Text stays raw because it is
// either a string or an array of strings and entity objects.
type rawMessage struct {
	ID               int64          `json:"id"`
	Type             string         `json:"type"`
	DateUnixtime     string         `json:"date_unixtime"`
	Date             string         `json:"date"`
	Edited           string         `json:"edited"`
	EditedUnixtime   string         `json:"edited_unixtime"`
	ReplyToMessageID int64          `json:"reply_to_message_id"`
	From             string         `json:"from"`
	FromID           string         `json:"from_id"`
	Text             jsontext.Value `json:"text"`
	TextEntities     []textEntity   `json:"text_entities"`
	Photo            string         `json:"photo"`
	File             string         `json:"file"`
	Thumbnail        string         `json:"thumbnail"`
	MediaType        string         `json:"media_type"`
	MimeType         string         `json:"mime_type"`
	FileName         string         `json:"file_name"`
	Title            string         `json:"title"`
	StickerEmoji     string         `json:"sticker_emoji"`
}

// textEntity is one text_entities entry.
type textEntity struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeChatFrom decodes one chat object from the decoder's next value.
func decodeChatFrom(dec *jsontext.Decoder) (*Chat, error) {
	var raw rawChat
	if err := json.UnmarshalDecode(dec, &raw); err != nil {
		return nil, fmt.Errorf("telegram: parse chat: %w", err)
	}
	if raw.ID == 0 {
		return nil, fmt.Errorf("telegram: chat %q has no id", raw.Name)
	}
	chat := &Chat{ID: raw.ID, Type: raw.Type, Name: raw.Name}
	chat.Messages = make([]Message, 0, len(raw.Messages))
	for _, m := range raw.Messages {
		msg, err := buildMessage(m)
		if err != nil {
			return nil, fmt.Errorf("telegram: chat %d message %d: %w", raw.ID, m.ID, err)
		}
		chat.Messages = append(chat.Messages, *msg)
	}
	return chat, nil
}

// buildMessage converts one raw entry. Text flattening and timestamp parsing
// are the only shaping steps; classification stays with the importer.
func buildMessage(m rawMessage) (*Message, error) {
	sentAt, err := parseExportTime(m.DateUnixtime, m.Date)
	if err != nil {
		return nil, err
	}
	msg := &Message{
		ID:        m.ID,
		Service:   m.Type == "service",
		SentAt:    sentAt,
		ReplyTo:   m.ReplyToMessageID,
		Edited:    m.Edited != "" || m.EditedUnixtime != "",
		FromName:  m.From,
		FromID:    m.FromID,
		Text:      flattenText(m.Text, m.TextEntities),
		Photo:     m.Photo,
		File:      m.File,
		Thumbnail: m.Thumbnail,
		MediaType: m.MediaType,
		MimeType:  m.MimeType,
		FileName:  m.FileName,
		Title:     m.Title,
	}
	if m.StickerEmoji != "" && msg.MediaType == "" {
		msg.MediaType = "sticker"
	}
	return msg, nil
}

// flattenText renders a message's text. text_entities carries the same parts
// as the text array in entity form; when present it is authoritative,
// otherwise the text field is flattened: a plain string as-is, an array by
// concatenating each string part and each entity object's text in order,
// with no inserted separators — Telegram's own rendering inserts nothing
// either.
func flattenText(raw jsontext.Value, entities []textEntity) string {
	if len(entities) > 0 {
		var b strings.Builder
		for _, e := range entities {
			b.WriteString(e.Text)
		}
		return b.String()
	}
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	case '[':
		dec := jsontext.NewDecoder(strings.NewReader(raw.String()))
		if dec.PeekKind() != '[' {
			return ""
		}
		if _, err := dec.ReadToken(); err != nil {
			return ""
		}
		var b strings.Builder
		for dec.PeekKind() != ']' {
			switch dec.PeekKind() {
			case '"':
				var s string
				if err := json.UnmarshalDecode(dec, &s); err != nil {
					return ""
				}
				b.WriteString(s)
			case '{':
				var part struct {
					Text string `json:"text"`
				}
				if err := json.UnmarshalDecode(dec, &part); err != nil {
					return ""
				}
				b.WriteString(part.Text)
			default:
				if err := dec.SkipValue(); err != nil {
					return ""
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// parseExportTime prefers date_unixtime (seconds since epoch as a string)
// and falls back to the date field, whose time part may carry Telegram
// Desktop's extra colon-separated tail. A message with neither timestamp
// keeps a zero time.
func parseExportTime(unixtime, date string) (time.Time, error) {
	if unixtime != "" {
		sec, err := strconv.ParseInt(strings.TrimSpace(unixtime), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("telegram: parse date_unixtime %q: %w", unixtime, err)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	if date == "" {
		return time.Time{}, nil
	}
	if strings.Contains(date, "Z") || strings.Contains(date, "+") {
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			return t.UTC(), nil
		}
	}
	// Drop a trailing ":NN" tail Telegram Desktop appends to its naive
	// timestamps ("2021-01-17T18:57:52:00").
	parts := strings.Split(date, ":")
	if len(parts) > 3 {
		date = strings.Join(parts[:3], ":")
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", date, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("telegram: parse date %q: %w", date, err)
	}
	return t.UTC(), nil
}

// OwnerSourceIdentifier returns the archive source identifier for an export
// owned by the given Telegram user ID.
func OwnerSourceIdentifier(ownerID int64) string {
	return "user" + strconv.FormatInt(ownerID, 10)
}
