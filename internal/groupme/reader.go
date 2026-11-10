package groupme

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"go.kenn.io/msgvault/internal/export"
)

const maxRecordBytes = 8 << 20
const maxMetadataBytes = 1 << 20

var errRecordTooLarge = errors.New("groupme: message record exceeds 8 MiB")

// recordReader limits decoder read-ahead as well as the raw record allocation.
// Its budget is renewed at each record's input offset, not at each file read.
type recordReader struct {
	reader       io.Reader
	total, limit int64
}

func (r *recordReader) Read(p []byte) (int, error) {
	remaining := r.limit - r.total
	if remaining <= 0 {
		return 0, errRecordTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.reader.Read(p)
	r.total += int64(n)
	return n, err
}

// statRegular rejects non-regular files before an open that could block,
// such as a FIFO. Callers still check the opened descriptor.
func statRegular(root *os.Root, name string) error {
	info, err := root.Stat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("groupme: %s must be a regular file", filepath.Base(name))
	}
	return nil
}

func messages(ctx context.Context, root *os.Root, folder string, visit func(*Message, jsontext.Value) error) (retErr error) {
	if err := statRegular(root, filepath.Join(folder, "message.json")); err != nil {
		return err
	}
	f, err := root.Open(filepath.Join(folder, "message.json"))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("groupme: message.json must be a regular file")
	}
	r := &recordReader{reader: &export.ContextReader{Context: ctx, Reader: f}, limit: maxRecordBytes}
	dec := jsontext.NewDecoder(r)
	token, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if token.Kind() != '[' {
		return errors.New("groupme: message.json must contain an array")
	}
	for {
		r.limit = dec.InputOffset() + maxRecordBytes
		if dec.PeekKind() == ']' {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return fmt.Errorf("groupme: read message: %w", err)
		}
		var m Message
		if raw.Kind() != '{' || json.Unmarshal(raw, &m) != nil {
			// ReadValue already validated JSON syntax. An incompatible record
			// is unsupported, not a reason to lose the remaining messages.
			if err := visit(nil, raw); err != nil {
				return err
			}
			continue
		}
		m.decodeAttachments()
		if err := visit(&m, raw); err != nil {
			return err
		}
	}
	if _, err := dec.ReadToken(); err != nil {
		return err
	}
	r.limit = dec.InputOffset() + maxRecordBytes
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("groupme: trailing JSON after message array")
	}
	return nil
}

func discover(ctx context.Context, root *os.Root, absolute string, visit func(chat) error) error {
	folders := []string{}
	if _, err := root.Stat("message.json"); err == nil {
		folders = append(folders, ".")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		f, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		for _, entry := range entries {
			// Do not follow directory symlinks during discovery.
			if !entry.IsDir() {
				continue
			}
			folder := entry.Name()
			if _, err := root.Stat(filepath.Join(folder, "message.json")); err == nil {
				folders = append(folders, folder)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if len(folders) == 0 {
		return errors.New("groupme: no message.json found in the directory or its immediate conversation directories")
	}
	sort.Strings(folders)
	for _, folder := range folders {
		c := chat{folder: folder, kind: "chat"}
		err := statRegular(root, filepath.Join(folder, "conversation.json"))
		var metadata *os.File
		if err == nil {
			metadata, err = root.Open(filepath.Join(folder, "conversation.json"))
		}
		if err == nil {
			info, statErr := metadata.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				_ = metadata.Close()
				return errors.New("groupme: conversation.json must be a regular file")
			}
			raw, readErr := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: metadata}, maxMetadataBytes+1))
			readErr = errors.Join(readErr, metadata.Close())
			if readErr != nil {
				return readErr
			}
			if len(raw) > maxMetadataBytes {
				return errors.New("groupme: conversation.json exceeds 1 MiB")
			}
			if err := json.Unmarshal(raw, &c.metadata); err != nil {
				return fmt.Errorf("groupme: decode conversation metadata: %w", err)
			}
			if c.metadata.Name == "" {
				c.metadata.Name = c.metadata.Chat.OtherUser.Name
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		reconcile := func(key, kind string) error {
			if key == "" {
				return nil
			}
			if c.key != "" && c.key != key {
				return errors.New("groupme: conflicting conversation IDs in metadata/message.json")
			}
			c.key, c.kind = key, kind
			return nil
		}
		for _, identity := range []struct{ prefix, id, kind string }{
			{"group:", c.metadata.ID, "group_chat"}, {"group:", c.metadata.GroupID, "group_chat"},
			{"dm:", c.metadata.ConversationID, "direct_chat"}, {"dm:", c.metadata.Chat.LastMessage.ChatID, "direct_chat"},
		} {
			if identity.id != "" {
				if err := reconcile(identity.prefix+identity.id, identity.kind); err != nil {
					return err
				}
			}
		}
		err = messages(ctx, root, folder, func(m *Message, _ jsontext.Value) error {
			if m == nil {
				return nil
			}
			key, kind := m.peer()
			return reconcile(key, kind)
		})
		if err != nil {
			return fmt.Errorf("groupme: conversation %q: %w", folder, err)
		}
		if c.key == "" {
			name := folder
			if folder == "." {
				name = filepath.Base(absolute)
			}
			c.key = "folder:" + name
		}
		if err := visit(c); err != nil {
			return err
		}
	}
	return nil
}
