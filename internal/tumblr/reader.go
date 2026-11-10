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
	"strings"
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
		zr, err := zip.OpenReader(filename)
		if err != nil {
			return nil, fmt.Errorf("tumblr: open export ZIP: %w", err)
		}
		r.fsys, r.close = zr, zr.Close
		names := map[string]bool{}
		if len(zr.File) > maxEntries {
			_ = r.close()
			return nil, errors.New("tumblr: too many ZIP entries")
		}
		for _, f := range zr.File {
			name := strings.TrimSuffix(f.Name, "/")
			if !fs.ValidPath(name) || strings.ContainsAny(name, `\:`) || names[name] || (!f.Mode().IsRegular() && !f.Mode().IsDir()) {
				_ = r.close()
				return nil, fmt.Errorf("tumblr: unsafe or duplicate ZIP entry %q", f.Name)
			}
			names[name] = true
		}
		files := map[string]bool{}
		for _, entry := range zr.File {
			if !entry.FileInfo().IsDir() {
				files[entry.Name] = true
			}
		}
		for _, entry := range zr.File {
			for parent := path.Dir(strings.TrimSuffix(entry.Name, "/")); parent != "."; parent = path.Dir(parent) {
				if files[parent] {
					_ = r.close()
					return nil, fmt.Errorf("tumblr: ZIP file is also directory %q", parent)
				}
			}
		}
	}
	if err := r.discover(ctx); err != nil {
		_ = r.close()
		return nil, err
	}
	return r, nil
}

func (r *exportReader) discover(ctx context.Context) error {
	roots := []string{}
	count := 0
	err := fs.WalkDir(r.fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxEntries {
			return errors.New("tumblr: too many export entries")
		}
		if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return fmt.Errorf("tumblr: nonregular entry %q", name)
		}
		if d.IsDir() && path.Base(name) == "conversations" {
			roots = append(roots, path.Dir(name))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tumblr: discover export entries: %w", err)
	}
	if len(roots) != 1 || (roots[0] != "." && strings.Contains(roots[0], "/")) {
		return errors.New("tumblr: expected one root or single wrapper with conversations/")
	}
	r.fsys, err = fs.Sub(r.fsys, roots[0])
	if err != nil {
		return fmt.Errorf("tumblr: select export root: %w", err)
	}
	err = fs.WalkDir(r.fsys, "conversations", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if strings.EqualFold(path.Ext(name), ".html") {
				r.files = append(r.files, name)
			} else {
				r.skipped++
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tumblr: discover conversations: %w", err)
	}
	return nil
}

func (r *exportReader) conversation(ctx context.Context, name string) (*Conversation, error) {
	f, err := r.fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("tumblr: open conversation: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx, f}, maxHTMLBytes+1))
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

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
