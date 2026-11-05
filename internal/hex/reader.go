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
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/hex/evidence"
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
			ok, lockErr := lock.TryRLock()
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
		if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.ID != id || !evidence.ValidID(id) || manifest.CreatedAtMS <= 0 {
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
		inferred := manifest.TitleSource == ""
		manifest.ResolveTitleSource(time.Local)
		m := Meeting{Manifest: manifest, TranscriptState: "missing", TitleSourceInferred: inferred}
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
			continue
		} else {
			reader := bufio.NewReader(strings.NewReader(string(data)))
			for {
				line, readErr := reader.ReadBytes('\n')
				if readErr != nil {
					break
				}
				if strings.TrimSpace(string(line)) == "" {
					continue
				}
				var header struct {
					Covered jsontext.Value `json:"covered_ms"`
				}
				headerErr := json.Unmarshal(line, &header)
				if headerErr == nil && len(header.Covered) > 0 {
					continue
				}
				var entry Entry
				if json.Unmarshal(line, &entry) != nil || !evidence.ValidEntry(entry) {
					m.TranscriptPartial = true
					out.Warnings++
					continue
				}
				if strings.TrimSpace(entry.Text) != "" {
					m.Entries = append(m.Entries, entry)
				}
			}
		}

		finalPublished := m.TranscriptState == "final"
		switch {
		case finalPublished && (m.Manifest.Status == "transcribing" || m.Manifest.Status == "interrupted" || m.Manifest.Status == "failed"):
			m.Manifest.Status = "complete"
		case !finalPublished && m.Manifest.Status == "complete":
			m.Manifest.Status = "interrupted"
		case m.Manifest.Status == "starting" || m.Manifest.Status == "recording" || m.Manifest.Status == "transcribing":
			m.Manifest.Status = "interrupted"
		}
		if err := evidence.Meeting(m).Validate(); err != nil {
			out.Warnings++
			continue
		}
		slices.SortStableFunc(m.Entries, func(a, b Entry) int {
			if a.StartMS < b.StartMS {
				return -1
			}
			if a.StartMS > b.StartMS {
				return 1
			}
			return 0
		})
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
