package googlechat

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/export"
)

const maxJSONBytes int64 = 64 << 20
const maxEntries = 100000
const maxRoots = 128

type part struct {
	root         *os.Root
	path, groups string
}
type groupPart struct {
	part *part
	dir  string
}
type exportReader struct {
	parts       []*part
	groups      map[string][]groupPart
	users       []provenance
	memberships map[string]bool
	entries     int
}

// openExport indexes explicit, shallow Takeout layouts before any archive write.
// Files stay behind rooted handles throughout discovery and ingestion.
func openExport(ctx context.Context, roots []string, owner string) (reader *exportReader, retErr error) {
	reader = &exportReader{groups: map[string][]groupPart{}, memberships: map[string]bool{}}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, reader.Close())
		}
	}()
	if len(roots) == 0 || len(roots) > maxRoots {
		return reader, fmt.Errorf("googlechat: supply 1 to %d extracted part directories", maxRoots)
	}
	seen := map[string]bool{}
	seenGroups := map[string]bool{}
	for _, name := range roots {
		if err := ctx.Err(); err != nil {
			return reader, err
		}
		absolute, err := filepath.Abs(name)
		if err != nil {
			return reader, err
		}
		if seen[absolute] {
			continue
		}
		seen[absolute] = true
		info, err := os.Stat(absolute)
		if err != nil {
			return reader, err
		}
		if !info.IsDir() {
			return reader, errors.New("googlechat: extract archive parts first; input must be a directory")
		}
		root, err := os.OpenRoot(absolute)
		if err != nil {
			return reader, err
		}
		p := &part{root: root, path: absolute, groups: "Groups"}
		reader.parts = append(reader.parts, p)
		found := false
		if filepath.Base(absolute) == "Groups" {
			p.groups = "."
			found = true
		} else {
			for _, prefix := range []string{filepath.Join("Takeout", "Google Chat"), "Google Chat", "."} {
				candidate := filepath.Join(prefix, "Groups")
				_, groupErr := root.Stat(candidate)
				_, userErr := root.Stat(filepath.Join(prefix, "Users"))
				if groupErr == nil || userErr == nil {
					if prefix != "." {
						nested, err := root.OpenRoot(prefix)
						if err != nil {
							return reader, err
						}
						if err := root.Close(); err != nil {
							_ = nested.Close()
							return reader, err
						}
						p.root = nested
						p.path = filepath.Join(absolute, prefix)
					}
					found = true
					break
				}
			}
		}
		if !found {
			return reader, fmt.Errorf("googlechat: no Google Chat Groups/Users layout in supplied root %s", absolute)
		}
		groupIdentity, err := filepath.EvalSymlinks(filepath.Join(p.path, p.groups))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return reader, err
		}
		if !seenGroups[groupIdentity] {
			err = reader.directories(ctx, p, p.groups, func(entry fs.DirEntry) error {
				id := entry.Name()
				if !strings.HasPrefix(id, "DM ") && !strings.HasPrefix(id, "Space ") {
					return fmt.Errorf("googlechat: unsupported group directory %q", id)
				}
				reader.groups[id] = append(reader.groups[id], groupPart{part: p, dir: filepath.Join(p.groups, id)})
				return nil
			})
			if err != nil {
				return reader, err
			}
			seenGroups[groupIdentity] = true
		}

		if p.groups == "." {
			continue
		}
		err = reader.directories(ctx, p, "Users", func(entry fs.DirEntry) error {
			if !strings.HasPrefix(entry.Name(), "User ") {
				return nil
			}
			path := filepath.Join("Users", entry.Name(), "user_info.json")
			raw, err := readJSON(ctx, p.root, path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			var info userInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				return fmt.Errorf("googlechat: user JSON: %w", err)
			}
			if info.User.Email != "" {
				email, err := NormalizeOwner(info.User.Email)
				if err != nil || email != owner {
					return errors.New("googlechat: supplied owner profile disagrees with --me")
				}
			}
			reader.users = append(reader.users, provenance{Root: p.path, Path: path, Data: string(raw)})
			for _, membership := range info.Memberships {
				if membership.State == "MEMBER_JOINED" {
					reader.memberships[membership.Group] = true
				}
			}
			return nil
		})
		if err != nil {
			return reader, err
		}
	}
	return reader, nil
}

func (r *exportReader) Close() error {
	var err error
	for _, p := range r.parts {
		err = errors.Join(err, p.root.Close())
	}
	return err
}

func (r *exportReader) directories(ctx context.Context, p *part, path string, visit func(fs.DirEntry) error) error {
	dir, err := p.root.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for {
		entries, err := dir.ReadDir(256)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			r.entries++
			if r.entries > maxEntries {
				return fmt.Errorf("googlechat: index exceeds %d entries", maxEntries)
			}
			if entry.IsDir() {
				if err := visit(entry); err != nil {
					return err
				}
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

func readJSON(ctx context.Context, root *os.Root, path string) (jsontext.Value, error) {
	before, err := root.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("googlechat: stat JSON %s: %w", path, err)
	}
	if !before.Mode().IsRegular() || before.Size() > maxJSONBytes {
		return nil, fmt.Errorf("googlechat: JSON %s must be a regular file of at most %d bytes", path, maxJSONBytes)
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("googlechat: open JSON %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(&export.ContextReader{Context: ctx, Reader: file}, maxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxJSONBytes {
		return nil, errors.New("googlechat: JSON byte limit exceeded")
	}
	return data, nil
}

// groupFiles accepts identical repeated JSON files, and refuses conflicting
// snapshots rather than letting argument order decide which provider data wins.
func (r *exportReader) groupFiles(ctx context.Context, id, name string) (jsontext.Value, []provenance, error) {
	var first jsontext.Value
	var paths []provenance
	for _, candidate := range r.groups[id] {
		root, err := candidate.part.root.OpenRoot(candidate.dir)
		if err != nil {
			return nil, nil, err
		}
		raw, err := readJSON(ctx, root, name)
		err = errors.Join(err, root.Close())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if first != nil && !bytes.Equal(bytes.TrimSpace(first), bytes.TrimSpace(raw)) {
			return nil, nil, fmt.Errorf("googlechat: conflicting %s for %s", name, id)
		}
		first = raw
		entry := provenance{Root: candidate.part.path, Path: filepath.Join(candidate.dir, name)}
		if name == "group_info.json" {
			entry.Data = string(raw)
		}
		paths = append(paths, entry)
	}
	return first, paths, nil
}

func (r *exportReader) groupIDs() []string {
	ids := make([]string, 0, len(r.groups))
	for id := range r.groups {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
