// Package xarchive imports the supplied files in an extracted X account archive.
package xarchive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/export"
)

// A single event may contain up to 1 MiB of JSON. The ceiling applies before
// decoder allocation, including read-ahead; conversations have no size limit.
const maxEventBytes = 1 << 20

var errEventLimit = errors.New("xarchive: JSON event exceeds 1 MiB limit")
var archiveFile = regexp.MustCompile(`^direct-messages(-group)?(-part[0-9]+)?\.js$`)
var wrapper = regexp.MustCompile(`^window\.YTD\.([a-z_]+)\.part[0-9]+\s*=$`)
var numericID = regexp.MustCompile(`^[0-9]+$`)

type archive struct {
	root        *os.Root
	owner, name string
}
type record struct {
	Peer, Filename string
	Group          bool
	Raw            []byte
}

func (a *archive) Close() error { return a.root.Close() }

func openArchive(path string) (*archive, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if info, e := root.Stat("data"); e == nil && info.IsDir() {
		child, e := root.OpenRoot("data")
		_ = root.Close()
		if e != nil {
			return nil, e
		}
		root = child
	}
	return &archive{root: root}, nil
}

func (a *archive) readAccount(ctx context.Context) error {
	info, err := a.root.Stat("account.js")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("xarchive: account.js must be a regular file")
	}
	f, err := a.root.Open("account.js")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	input, err := unwrap(&export.ContextReader{Context: ctx, Reader: f}, "account")
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(input, maxEventBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxEventBytes {
		return errEventLimit
	}
	b = bytes.TrimSpace(b)
	b = bytes.TrimSuffix(b, []byte(";"))
	var rows []struct {
		Account struct {
			ID   string `json:"accountId"`
			Name string `json:"accountDisplayName"`
		} `json:"account"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return fmt.Errorf("xarchive: account.js: %w", err)
	}
	if len(rows) != 1 || !validID(rows[0].Account.ID) {
		return errors.New("xarchive: account.js must identify one numeric account")
	}
	a.owner, a.name = rows[0].Account.ID, rows[0].Account.Name
	return nil
}
func validID(s string) bool { return numericID.MatchString(s) && strings.TrimLeft(s, "0") != "" }

func (a *archive) events(ctx context.Context, visit func(record) error) error {
	entries, err := fs.ReadDir(a.root.FS(), ".")
	if err != nil {
		return fmt.Errorf("xarchive: list archive files: %w", err)
	}
	found := false
	for _, entry := range entries {
		name := entry.Name()
		if !archiveFile.MatchString(name) {
			continue
		}
		found = true
		info, err := a.root.Stat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("xarchive: archive input must be a regular file")
		}
		f, err := a.root.Open(name)
		if err != nil {
			return err
		}
		group := strings.HasPrefix(name, "direct-messages-group")
		expected := "direct_messages"
		if group {
			expected += "_group"
		}
		input, err := unwrap(&export.ContextReader{Context: ctx, Reader: f}, expected)
		if err == nil {
			err = readEvents(ctx, input, name, group, visit)
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return fmt.Errorf("xarchive: %s: %w", name, err)
		}
	}
	if !found {
		return errors.New("xarchive: no direct-messages JS files found")
	}
	return nil
}

func unwrap(input io.Reader, expected string) (io.Reader, error) {
	r := bufio.NewReaderSize(input, 256)
	var b strings.Builder
	for range 256 {
		c, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("xarchive: read JavaScript wrapper: %w", err)
		}
		b.WriteByte(c)
		if c == '=' {
			match := wrapper.FindStringSubmatch(strings.TrimSpace(b.String()))
			if len(match) != 2 || match[1] != expected {
				return nil, errors.New("xarchive: unsupported JavaScript wrapper")
			}
			return r, nil
		}
	}
	return nil, errors.New("xarchive: unsupported JavaScript wrapper")
}

// boundedInput counts all bytes pulled by the decoder, including read-ahead.
// Resetting from InputOffset rather than bytes read prevents buffered input
// from granting a subsequent value a larger allocation budget.
type boundedInput struct {
	io.Reader

	read, ceiling int64
}

func (r *boundedInput) Read(p []byte) (int, error) {
	remaining := r.ceiling - r.read
	if remaining <= 0 {
		return 0, errEventLimit
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.Reader.Read(p)
	r.read += int64(n)
	return n, err
}

type stream struct {
	d *jsontext.Decoder
	r *boundedInput
}

func (s stream) bound()                         { s.r.ceiling = s.d.InputOffset() + maxEventBytes }
func (s stream) token() (jsontext.Token, error) { s.bound(); return s.d.ReadToken() }
func (s stream) want(kind jsontext.Kind) error {
	t, e := s.token()
	if e == nil && t.Kind() != kind {
		e = fmt.Errorf("xarchive: expected %c", kind)
	}
	return e
}
func (s stream) value() (jsontext.Value, error) { s.bound(); v, e := s.d.ReadValue(); return v, e }
func (s stream) peek() jsontext.Kind            { s.bound(); return s.d.PeekKind() }
func (s stream) object(visit func(string) error) error {
	if err := s.want('{'); err != nil {
		return err
	}
	for s.peek() != '}' {
		t, err := s.token()
		if err != nil {
			return err
		}
		if t.Kind() != '"' {
			return errors.New("xarchive: expected property name")
		}
		if err := visit(t.String()); err != nil {
			return err
		}
	}
	return s.want('}')
}
func readEvents(ctx context.Context, input io.Reader, filename string, group bool, visit func(record) error) error {
	r := &boundedInput{Reader: input, ceiling: maxEventBytes}
	s := stream{d: jsontext.NewDecoder(r), r: r}
	if err := s.want('['); err != nil {
		return err
	}
	for s.peek() != ']' {
		conversationSeen := false
		if err := s.object(func(key string) error {
			if key != "dmConversation" {
				_, err := s.value()
				return err
			}
			conversationSeen = true
			peer := ""
			messagesSeen := false
			err := s.object(func(key string) error {
				switch key {
				case "conversationId":
					t, err := s.token()
					if err != nil {
						return err
					}
					if t.Kind() != '"' || t.String() == "" {
						return errors.New("xarchive: invalid conversationId")
					}
					peer = "direct:" + t.String()
					if group {
						peer = "group:" + t.String()
					}
					return nil
				case "messages":
					messagesSeen = true
					if peer == "" {
						return errors.New("xarchive: conversationId must precede messages")
					}
					if err := s.want('['); err != nil {
						return err
					}
					if err := visit(record{Peer: peer, Filename: filename, Group: group}); err != nil {
						return err
					}
					for s.peek() != ']' {
						if err := ctx.Err(); err != nil {
							return err
						}
						raw, err := s.value()
						if err != nil {
							return err
						}
						if err := visit(record{Peer: peer, Filename: filename, Group: group, Raw: raw}); err != nil {
							return err
						}
					}
					return s.want(']')
				default:
					_, err := s.value()
					return err
				}
			})
			if err == nil && (peer == "" || !messagesSeen) {
				err = errors.New("xarchive: dmConversation requires conversationId and messages")
			}
			return err
		}); err != nil {
			return err
		}
		if !conversationSeen {
			return errors.New("xarchive: record requires dmConversation")
		}
	}
	if err := s.want(']'); err != nil {
		return err
	}
	// Permit the archive's optional final semicolon, but no executable suffix.
	s.bound()
	tail, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(s.d.UnreadBuffer()), r), 257))
	if err != nil {
		return err
	}
	if len(tail) > 256 || strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(tail)), ";")) != "" {
		return errors.New("xarchive: trailing content after JSON")
	}
	return nil
}
