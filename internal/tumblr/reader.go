package tumblr

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/export"
)

type exportReader struct {
	fsys    fs.FS
	close   func() error
	files   []string
	skipped int
}

func openExport(ctx context.Context, filename string) (*exportReader, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, errors.New("tumblr: nonregular input")
	}
	r := &exportReader{}
	if info.IsDir() {
		root, err := os.OpenRoot(filename)
		if err != nil {
			return nil, err
		}
		r.fsys, r.close = root.FS(), root.Close
	} else {
		f, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		r.close = f.Close
		err = checkZIP(ctx, f, info.Size(), maxEntries, maxMetadataBytes)
		var zr *zip.Reader
		if err == nil {
			zr, err = zip.NewReader(&contextReaderAt{ctx, f}, info.Size())
		}
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("tumblr: open export ZIP: %w", err)
		}
		r.fsys = zr
	}
	if err := r.discover(ctx, maxEntries, maxMetadataBytes); err != nil {
		_ = r.close()
		return nil, err
	}
	return r, nil
}

func (r *exportReader) discover(ctx context.Context, entries, pathBytes int) error {
	// ReadDir(n) bounds each allocation before a directory's size is known.
	scan := func(name string, visit func(string, fs.DirEntry) error) error {
		f, err := r.fsys.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		dir, ok := f.(fs.ReadDirFile)
		if !ok {
			return fmt.Errorf("tumblr: not a directory: %s", name)
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch, err := dir.ReadDir(256)
			for _, d := range batch {
				child := path.Join(name, d.Name())
				entries--
				pathBytes -= len(child)
				if entries < 0 || pathBytes < 0 {
					return errors.New("tumblr: export entry/path metadata limit exceeded")
				}
				if err := visit(child, d); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	root := "."
	if info, err := fs.Stat(r.fsys, "conversations"); err != nil || !info.IsDir() {
		roots := 0
		if err := scan(".", func(name string, d fs.DirEntry) error {
			if !d.IsDir() || strings.HasPrefix(d.Name(), ".") || d.Name() == "__MACOSX" {
				return nil
			}
			if info, err := fs.Stat(r.fsys, path.Join(name, "conversations")); err == nil && info.IsDir() {
				roots++
				root = name
			}
			return nil
		}); err != nil {
			return err
		}
		if roots != 1 {
			return errors.New("tumblr: expected one root or single wrapper with conversations/")
		}
	}
	var err error
	r.fsys, err = fs.Sub(r.fsys, root)
	if err != nil {
		return err
	}
	pending := []string{"conversations"}
	for len(pending) > 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := scan(name, func(child string, d fs.DirEntry) error {
			if d.IsDir() {
				pending = append(pending, child)
			} else if strings.EqualFold(path.Ext(child), ".html") {
				r.files = append(r.files, child)
			} else {
				r.skipped++
			}
			return nil
		}); err != nil {
			return fmt.Errorf("tumblr: discover conversations: %w", err)
		}
	}
	sort.Strings(r.files)
	return ctx.Err()
}

func (r *exportReader) conversation(ctx context.Context, name string) (*Conversation, error) {
	f, err := r.fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("tumblr: open conversation: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("tumblr: nonregular conversation")
	}
	data, err := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: f}, maxHTMLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHTMLBytes {
		return nil, errHTMLTooLarge
	}
	c, err := parseConversation(ctx, data)
	if c != nil {
		c.Filename = name
	}
	return c, err
}

var errHTMLTooLarge = errors.New("tumblr: HTML exceeds 64 MiB")
