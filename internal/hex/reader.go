package hex

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/meetingimport"
)

const maxEvidenceBytes = 16 << 20

type Reader struct{ root *os.Root }

func Open(ctx context.Context, path string) (*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open Hex meetings: %w", err)
	}
	return &Reader{root: root}, nil
}
func (r *Reader) Close() error { return r.root.Close() }

type Scan struct {
	Meetings []Meeting
	Deferred int64
	Warnings int64
}

// ListMeetings ignores bad folders and reports their count. Held and unreadable
// locks defer the whole folder. No lock file or recovery output is created.
func (r *Reader) ListMeetings(ctx context.Context) (Scan, error) {
	var out Scan
	dir, err := r.root.Open(".")
	if err != nil {
		return out, err
	}
	defer func() { _ = dir.Close() }()
	folders, err := dir.ReadDir(-1)
	if err != nil {
		return out, err
	}
	// Deterministic order matters for --limit and repeatable scans.
	slices.SortFunc(folders, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !folder.IsDir() || folder.Type()&os.ModeSymlink != 0 {
			continue
		}
		id := folder.Name()
		lockPath := filepath.Join(r.root.Name(), id, "active.lock")
		if info, err := r.root.Lstat(filepath.Join(id, "active.lock")); err == nil {
			if !info.Mode().IsRegular() {
				out.Deferred++
				continue
			}
			lock := flock.New(lockPath, flock.SetFlag(os.O_RDONLY))
			ok, lockErr := lock.TryLock()
			if lockErr != nil || !ok {
				_ = lock.Close()
				out.Deferred++
				continue
			}
			_ = lock.Unlock()
			_ = lock.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			out.Deferred++
			continue
		}
		data, err := r.read(filepath.Join(id, "manifest.json"))
		if err != nil {
			out.Warnings++
			continue
		}
		var manifest Manifest
		if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.ID != id || !validID(id) || manifest.CreatedAtMS <= 0 {
			out.Warnings++
			continue
		}
		switch manifest.Status {
		case "starting", "recording", "transcribing":
			// Recover this stale active status after checking final publication.
		case "complete", "interrupted", "failed":
		default:
			out.Warnings++
			continue
		}
		m := Meeting{Manifest: manifest, TranscriptState: "missing"}
		data, err = r.read(filepath.Join(id, "transcript.ndjson"))
		if errors.Is(err, os.ErrNotExist) {
			data, err = r.read(filepath.Join(id, "transcript.rolling.ndjson"))
			m.TranscriptState = "rolling"
		} else {
			m.TranscriptState = "final"
		}
		if errors.Is(err, os.ErrNotExist) {
			m.TranscriptState = "missing"
		} else if err != nil {
			out.Warnings++
			m.TranscriptState = "unreadable"
		} else {
			reader := bufio.NewReader(strings.NewReader(string(data)))
			for {
				line, readErr := reader.ReadBytes('\n')
				if readErr != nil {
					break
				}
				var header struct {
					Covered jsontext.Value `json:"covered_ms"`
				}
				_ = json.Unmarshal(line, &header)
				if len(header.Covered) > 0 {
					continue
				}
				var entry Entry
				if json.Unmarshal(line, &entry) != nil || !validEntry(entry) {
					out.Warnings++
					continue
				}
				m.Entries = append(m.Entries, entry)
			}
		}

		finalPublished := false
		if m.TranscriptState == "final" {
			if _, err := r.root.Stat(filepath.Join(id, "transcript.md")); err == nil {
				finalPublished = true
			}
		}
		switch {
		case finalPublished && (m.Manifest.Status == "transcribing" || m.Manifest.Status == "interrupted" || m.Manifest.Status == "failed"):
			m.Manifest.Status = "complete"
		case !finalPublished && m.Manifest.Status == "complete":
			m.Manifest.Status = "interrupted"
		case m.Manifest.Status == "starting" || m.Manifest.Status == "recording" || m.Manifest.Status == "transcribing":
			m.Manifest.Status = "interrupted"
		}
		// Apply the same field bounds used by remote transfer before local archival.
		if _, err := (RemoteRequest{Action: "upsert", Source: meetingimport.Source{Identifier: "validation", AccountEmail: "recorder@example.com"}, Meeting: &m}).Normalize(); err != nil {
			out.Warnings++
			continue
		}
		out.Meetings = append(out.Meetings, m)
	}
	return out, nil
}
func (r *Reader) read(path string) ([]byte, error) {
	f, err := r.root.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxEvidenceBytes+1))
	if len(b) > maxEvidenceBytes {
		return nil, errors.New("Hex evidence exceeds 16 MiB")
	}
	return b, err
}
func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00") && strings.IndexFunc(id, unicode.IsControl) < 0
}
func validEntry(e Entry) bool {
	return (e.Source == "system" || e.Source == "microphone") && e.StartMS >= 0 && e.EndMS >= e.StartMS
}
