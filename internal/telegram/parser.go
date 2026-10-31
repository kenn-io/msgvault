package telegram

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const resultFile = "result.json"

var ErrNoOwner = errors.New("telegram: export does not identify the exporting user; pass --me <numeric-id>")
var peerID = regexp.MustCompile(`^(user|chat|channel)[0-9]+$`)

// ExportReader makes two passes and holds at most one chat in memory.
type ExportReader struct {
	file       *os.File
	ownerID    int64
	ownerName  string
	single     bool
	containers map[string]int64
}

func OpenExport(root string) (*ExportReader, error) {
	f, err := os.Open(filepath.Join(root, resultFile))
	if err != nil {
		return nil, fmt.Errorf("telegram: open result.json: %w", err)
	}
	r := &ExportReader{file: f, containers: map[string]int64{}}
	dec := jsontext.NewDecoder(f)
	err = readObject(dec, func(key string, dec *jsontext.Decoder) error {
		if key == "personal_information" {
			var info struct {
				ID    int64  `json:"user_id"`
				First string `json:"first_name"`
				Last  string `json:"last_name"`
			}
			if err := json.UnmarshalDecode(dec, &info); err != nil {
				return err
			}
			r.ownerID, r.ownerName = info.ID, strings.TrimSpace(info.First+" "+info.Last)
			return nil
		}
		if key == "chats" || key == "left_chats" {
			token, err := dec.ReadToken()
			if err != nil {
				return err
			}
			if token.Kind() != '{' {
				return fmt.Errorf("%s must be an object", key)
			}
			r.containers[key] = dec.InputOffset() - 1
			return readObjectContents(dec, func(_ string, dec *jsontext.Decoder) error { return dec.SkipValue() })
		}
		if key == "id" || key == "messages" {
			r.single = true
		}
		return dec.SkipValue()
	})
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("telegram: read export: %w", err)
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		_ = f.Close()
		if err == nil {
			err = errors.New("trailing content after root object")
		}
		return nil, fmt.Errorf("telegram: trailing content: %w", err)
	}
	return r, nil
}

func (r *ExportReader) Close() error      { return r.file.Close() }
func (r *ExportReader) OwnerID() int64    { return r.ownerID }
func (r *ExportReader) OwnerName() string { return r.ownerName }

// Chats visits chats.list, then left_chats.list, or the single-chat root.
func (r *ExportReader) Chats(visit func(*Chat) error) error {
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if r.single {
		return decodeChat(jsontext.NewDecoder(r.file), visit)
	}
	for _, key := range []string{"chats", "left_chats"} {
		offset, ok := r.containers[key]
		if !ok {
			continue
		}
		if _, err := r.file.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		err := readObject(jsontext.NewDecoder(r.file), func(name string, dec *jsontext.Decoder) error {
			if name != "list" {
				return dec.SkipValue()
			}
			token, err := dec.ReadToken()
			if err != nil {
				return err
			}
			if token.Kind() != '[' {
				return fmt.Errorf("%s.list must be an array", key)
			}
			for dec.PeekKind() != ']' {
				if err := decodeChat(dec, visit); err != nil {
					return err
				}
			}
			_, err = dec.ReadToken()
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func readObject(dec *jsontext.Decoder, field func(string, *jsontext.Decoder) error) error {
	token, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != '{' {
		return errors.New("expected JSON object")
	}
	return readObjectContents(dec, field)
}

func readObjectContents(dec *jsontext.Decoder, field func(string, *jsontext.Decoder) error) error {
	for dec.PeekKind() != '}' {
		key, err := dec.ReadToken()
		if err != nil {
			return err
		}
		if err := field(key.String(), dec); err != nil {
			return err
		}
	}
	_, err := dec.ReadToken()
	return err
}

func decodeChat(dec *jsontext.Decoder, visit func(*Chat) error) error {
	var chat Chat
	if err := json.UnmarshalDecode(dec, &chat); err != nil {
		return fmt.Errorf("telegram: decode chat: %w", err)
	}
	if chat.ID <= 0 {
		return fmt.Errorf("telegram: chat %q needs a positive id", chat.Name)
	}
	if chat.PeerKey() != "" {
		for i := range chat.Messages {
			m := &chat.Messages[i]
			if !m.supported() {
				continue
			}
			if err := validateMessage(m); err != nil {
				return fmt.Errorf("telegram: chat %q (%s) message %d: %w", chat.Name, chat.PeerKey(), m.ID, err)
			}
		}
	}
	return visit(&chat)
}

func validateMessage(m *Message) error {
	if m.ID <= 0 {
		return errors.New("id must be positive")
	}
	unix, err := strconv.ParseInt(m.DateUnixtime, 10, 64)
	if err != nil {
		return fmt.Errorf("date_unixtime must be an integer: %w", err)
	}
	m.SentAt = time.Unix(unix, 0).UTC()
	for _, id := range []string{m.FromID, m.ReplyToPeerID} {
		if id != "" && !peerID.MatchString(id) {
			return fmt.Errorf("invalid peer id %q", id)
		}
	}
	if len(m.TextEntities) > 0 {
		var text strings.Builder
		for _, entity := range m.TextEntities {
			text.WriteString(entity.Text)
		}
		m.Text = text.String()
	} else if len(m.RawText) == 0 {
		m.Text = ""
	} else if err := json.Unmarshal(m.RawText, &m.Text); err != nil || m.RawText[0] != '"' {
		return errors.New("text must be a string when text_entities is empty")
	}
	return nil
}
