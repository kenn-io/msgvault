package tiktokexport

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

const maxFileBytes int64 = 64 << 20
const maxArchiveBytes int64 = 256 << 20
const maxEntries = 10000

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func readArchive(ctx context.Context, filename string) (*archive, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	var source fs.FS
	if info.IsDir() {
		source = os.DirFS(filename)
	} else {
		if !info.Mode().IsRegular() {
			return nil, errors.New("archive must be a ZIP or directory")
		}
		zr, err := zip.OpenReader(filename)
		if err != nil {
			return nil, fmt.Errorf("open ZIP: %w", err)
		}
		defer func() { _ = zr.Close() }()
		if len(zr.File) > maxEntries {
			return nil, errors.New("archive has too many entries")
		}
		seen := map[string]bool{}
		for _, entry := range zr.File {
			name := strings.TrimSuffix(entry.Name, "/")
			if !fs.ValidPath(name) || strings.Contains(name, `\`) {
				return nil, errors.New("ZIP contains an unsafe member path")
			}
			if seen[name] {
				return nil, errors.New("ZIP contains duplicate member paths")
			}
			seen[name] = true
			if entry.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("ZIP contains a symbolic link")
			}
			if entry.UncompressedSize64 > uint64(maxFileBytes) {
				return nil, errors.New("ZIP member exceeds 64 MiB")
			}
		}
		source = zr
	}
	files := []string{}
	entries := 0
	err = fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > maxEntries {
			return errors.New("archive has too many entries")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("archive contains a symbolic link")
		}
		if !entry.IsDir() && (strings.EqualFold(path.Ext(name), ".json") || strings.EqualFold(path.Ext(name), ".txt")) {
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspect export member: %w", err)
			}
			if !info.Mode().IsRegular() {
				return errors.New("export member is not a regular file")
			}
			files = append(files, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan archive: %w", err)
	}
	slices.Sort(files)
	out := &archive{}
	text := &archive{}
	var total int64
	for _, ext := range []string{".json", ".txt"} {
		for _, name := range files {
			if strings.ToLower(path.Ext(name)) != ext {
				continue
			}
			kind := ""
			if ext == ".txt" {
				switch normalized(strings.TrimSuffix(path.Base(name), path.Ext(name))) {
				case "directmessage", "directmessages", "chathistory":
					kind = "dm"
				case "comment", "comments":
					kind = "comment"
				case "profile", "profileinformation":
					kind = "profile"
				default:
					out.UnknownSections++
					continue
				}
			}
			f, err := source.Open(name)
			if err != nil {
				return nil, fmt.Errorf("open export member: %w", err)
			}
			b, readErr := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: f}, maxFileBytes+1))
			closeErr := f.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read export member: %w", readErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close export member: %w", closeErr)
			}
			total += int64(len(b))
			if int64(len(b)) > maxFileBytes || total > maxArchiveBytes {
				return nil, errors.New("export exceeds 64 MiB per file or 256 MiB total")
			}
			if ext == ".json" {
				err = out.parseJSON(b)
			} else {
				err = text.parseTXT(b, kind)
			}
			if err != nil {
				return nil, fmt.Errorf("parse %s export member: %w", ext, err)
			}
		}
	}
	if out.Username == "" {
		out.Username = text.Username
	}
	if out.Nickname == "" {
		out.Nickname = text.Nickname
	}
	if text.Username != "" && out.Username != text.Username {
		return nil, errors.New("JSON and TXT profile usernames disagree")
	}
	fillTXTLinks(out.Messages, text.Messages)
	for _, m := range text.Messages {
		if m.Comment {
			if out.comments {
				continue
			}
		} else if out.chats[username(m.Chat)] {
			continue
		}
		out.Messages = append(out.Messages, m)
	}
	out.recognized = out.recognized || text.recognized
	if !out.recognized {
		return nil, errors.New("no supported TikTok export sections found")
	}
	if out.Username == "" {
		return nil, errors.New("profile Username is missing; include Profile Information in the export")
	}
	for i := range out.Messages {
		m := &out.Messages[i]
		if m.Comment {
			m.Sender = out.Username
			continue
		}
		if sender := username(m.Sender); sender != out.Username && sender != username(m.Chat) {
			return nil, errors.New("chat sender does not match profile or chat partner")
		}
	}
	return out, nil
}

// Enrich only one exact JSON record from one distinct TXT link. Repeated or
// conflicting JSON records and conflicting TXT links provide no safe match.
func fillTXTLinks(primary, fallback []message) {
	counts := map[string]int{}
	for _, m := range primary {
		counts[m.contentKey()]++
	}
	links := map[string]map[string]bool{}
	for _, m := range fallback {
		if m.Link == "" {
			continue
		}
		key := m.contentKey()
		if links[key] == nil {
			links[key] = map[string]bool{}
		}
		links[key][m.Link] = true
	}
	for i := range primary {
		m := &primary[i]
		if m.Link != "" {
			continue
		}
		key := m.contentKey()
		if counts[key] != 1 || len(links[key]) != 1 {
			continue
		}
		for link := range links[key] {
			m.Link = link
			m.LinkFromTXT = true
		}
	}
}
