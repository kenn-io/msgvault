package signalexport

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/export"
)

const maxRecord = 16 << 20
const maxMetadata = 64 << 10
const maxMediaIndex = 1 << 18
const ambiguousMedia = "\x00"

type peer struct {
	key, name, kind string
}
type reader struct {
	media                  map[string]map[string]string
	mediaNames, maxMedia   int
	mediaExhausted         bool
	omitted                map[string]int64
	root                   *os.Root
	file                   *os.File
	lines                  *bufio.Reader
	line                   int
	header                 backupInfo
	peers                  map[decimal]peer
	chats                  map[decimal]decimal
	self                   decimal
	strings                int
	maxEntries, maxStrings int
	empty, unknown         int64
}

func openReader(ctx context.Context, path string) (r *reader, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("signal export directory: %w", err)
	}
	r = &reader{media: map[string]map[string]string{}, omitted: map[string]int64{}, root: root, peers: map[decimal]peer{}, chats: map[decimal]decimal{}, maxEntries: 100000, maxStrings: 64 << 20, maxMedia: maxMediaIndex}
	resource := r
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, resource.Close())
		}
	}()
	f, err := openRegular(root, "metadata.json")
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: f}, maxMetadata+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if len(b) > maxMetadata {
		return nil, errors.New("signal export: metadata limit (64 KiB)")
	}
	var metadata struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &metadata); err != nil {
		return nil, fmt.Errorf("metadata.json: %w", err)
	}
	if metadata.Version != 1 {
		return nil, fmt.Errorf("metadata.json: unsupported version %d; expected 1", metadata.Version)
	}
	r.file, err = openRegular(root, "main.jsonl")
	if err != nil {
		return nil, err
	}
	r.lines = bufio.NewReaderSize(&export.ContextReader{Context: ctx, Reader: r.file}, 64<<10)
	b, err = r.next()
	if err != nil {
		return nil, fmt.Errorf("signal header: %w", err)
	}
	if err = json.Unmarshal(b, &r.header); err != nil {
		return nil, fmt.Errorf("signal header version: %w", err)
	}
	if r.header.Version != "1" {
		return nil, errors.New("signal header: unsupported version; expected string \"1\"")
	}
	b, err = r.next()
	if err != nil {
		return nil, fmt.Errorf("signal account frame: %w", err)
	}
	frame, err := decodeObject(b)
	if err != nil || len(frame) != 1 || frame["account"] == nil {
		return nil, errors.New("signal line 2: expected one account frame")
	}
	if _, err := decodeObject(frame["account"]); err != nil {
		return nil, fmt.Errorf("signal account: %w", err)
	}
	return r, nil
}

// openRegular rejects nonregular inputs before an open can block on a FIFO.
func openRegular(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("signal %s: expected regular file", name)
	}
	return root.Open(name)
}

func (r *reader) Close() error {
	var err error
	if r.file != nil {
		err = r.file.Close()
	}
	return errors.Join(err, r.root.Close())
}
func decodeObject(b []byte) (map[string]jsontext.Value, error) {
	var obj map[string]jsontext.Value
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("expected JSON object")
	}
	return obj, nil
}
func (r *reader) next() ([]byte, error) {
	var b []byte
	for {
		part, err := r.lines.ReadSlice('\n')
		if len(b)+len(part) > maxRecord {
			return nil, fmt.Errorf("signal line %d: record limit (16 MiB)", r.line+1)
		}
		b = append(b, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(b) > 0 {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		r.line++
		return b, nil
	}
}
func (r *reader) budget(n int) error {
	if len(r.peers)+len(r.chats) >= r.maxEntries {
		return errors.New("lookup entries limit (100000)")
	}
	if r.strings+n > r.maxStrings {
		return errors.New("lookup strings limit (64 MiB)")
	}
	r.strings += n
	return nil
}
func (r *reader) frames(ctx context.Context, visit func(*chatItem, []byte, int) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := r.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = r.frame(b, visit); err != nil {
			return fmt.Errorf("signal line %d: %w", r.line, err)
		}
	}
}
func (r *reader) frame(b []byte, visit func(*chatItem, []byte, int) error) error {
	frame, err := decodeObject(b)
	if err != nil {
		return err
	}
	if len(frame) == 0 {
		r.empty++
		return nil
	}
	if len(frame) != 1 {
		return errors.New("expected one flat Frame kind")
	}
	for kind, raw := range frame {
		switch kind {
		case "account":
			return errors.New("duplicate account frame")
		case "recipient":
			var rec recipient
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			if rec.ID == "" || rec.ID == "0" {
				return errors.New("recipient id required")
			}
			p := recipientPeer(rec)
			if old, ok := r.peers[rec.ID]; ok {
				if old != p {
					return errors.New("conflicting recipient definition")
				}
				return nil
			}
			if rec.Self != nil {
				if p.key != "self" {
					return errors.New("self recipient has conflicting destinations")
				}
				if r.self != "" && r.self != rec.ID {
					return errors.New("multiple Self recipients")
				}
				r.self = rec.ID
			}
			if err := r.budget(len(p.key) + len(p.name) + len(p.kind) + len(rec.ID)); err != nil {
				return err
			}
			r.peers[rec.ID] = p
		case "chat":
			var c chat
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			if c.ID == "" || c.ID == "0" || c.RecipientID == "" {
				return errors.New("chat id and recipientId required")
			}
			ref := c.RecipientID
			if old, ok := r.chats[c.ID]; ok {
				if old != ref {
					return errors.New("conflicting chat definition")
				}
				return nil
			}
			if _, ok := r.peers[c.RecipientID]; !ok {
				return errors.New("chat recipient must precede chat")
			}
			if err := r.budget(len(c.ID) + len(c.RecipientID)); err != nil {
				return err
			}
			r.chats[c.ID] = ref
		case "chatItem":
			var m chatItem
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			return visit(&m, raw, r.line)
		case "adHocCall", "installedStickerPack", "hiddenStickerPack", "favoriteStickerList", "notificationProfile", "chatFolder":
			r.omitted[kind]++
		default:
			r.unknown++
		}
	}
	return nil
}

// mediaName caches a bounded index for each prefix.
func (r *reader) mediaName(ctx context.Context, hash string) (string, string, error) {
	prefix := hash[:2]
	result := func(suffix string, found bool) (string, string, error) {
		if !found {
			return "", "missing", nil
		}
		if suffix == ambiguousMedia {
			return "", "ambiguous", nil
		}
		return hash + suffix, "", nil
	}
	if m, ok := r.media[prefix]; ok {
		if m == nil {
			return "", "index_limit", nil
		}
		suffix, found := m[hash]
		return result(suffix, found)
	}
	if r.mediaExhausted {
		return "", "index_limit", nil
	}
	d, err := r.root.Open("files/" + prefix)
	if errors.Is(err, os.ErrNotExist) {
		r.media[prefix] = map[string]string{}
		return "", "missing", nil
	}
	if err != nil {
		return "", "missing_or_unsafe", nil
	}
	defer func() { _ = d.Close() }()
	m := map[string]string{}
	for {
		entries, err := d.ReadDir(min(128, r.maxMedia-r.mediaNames+1))
		for _, entry := range entries {
			if r.mediaNames >= r.maxMedia {
				r.mediaExhausted = true
				r.media[prefix] = nil
				return "", "index_limit", nil
			}
			r.mediaNames++
			name := entry.Name()
			if len(name) < 64 || (len(name) > 64 && name[64] != '.') {
				continue
			}
			stem := name[:64]
			if stem[:2] != prefix || strings.Trim(stem, "0123456789abcdef") != "" {
				continue
			}
			if _, dup := m[stem]; dup {
				m[stem] = ambiguousMedia
			} else {
				m[stem] = name[64:]
			}
		}
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "unreadable_directory", nil
		}
	}
	r.media[prefix] = m
	suffix, found := m[hash]
	return result(suffix, found)
}
