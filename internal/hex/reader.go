package hex

import (
	"bytes"
	"context"
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("open Hex meetings: %w", err)
	}
	return &Reader{root: root}, nil
}
func (r *Reader) Close() error {
	if r.root == nil {
		return nil
	}
	return r.root.Close()
}

type Scan struct {
	Meetings []Meeting
	Deferred int64
	Warnings int64
}

// ListMeetings ignores bad folders and reports their count. Held and unreadable
// locks defer the whole folder. No lock file or recovery output is created.
func (r *Reader) ListMeetings(ctx context.Context) (Scan, error) {
	var out Scan
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if r.root == nil {
		return out, nil
	}
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
		if !folder.IsDir() || folder.Type()&os.ModeSymlink != 0 || strings.HasPrefix(folder.Name(), ".") {
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
		if json.Unmarshal(data, &manifest) != nil || manifest.ID != id {
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
			var warnings int64
			m.Entries, m.TranscriptPartial, warnings = decodeTranscript(data, m.TranscriptState == "rolling")
			out.Warnings += warnings
			if m.TranscriptState == "rolling" && len(m.Entries) == 0 {
				m.TranscriptState = "missing"
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
		return nil, errors.New("hex evidence exceeds 16 MiB")
	}
	return b, err
}

// decodeTranscript retains complete entries and marks discarded input partial.
func decodeTranscript(data []byte, rolling bool) (entries []Entry, partial bool, warnings int64) {
	for line := range bytes.SplitAfterSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var header struct {
			Covered *struct{} `json:"covered_ms"`
		}
		if rolling && line[len(line)-1] == '\n' && json.Unmarshal(line, &header) == nil && header.Covered != nil {
			continue
		}
		entry, ok := decodeEntry(line)
		if !ok {
			warnings++
			continue
		}
		entries = append(entries, entry)
	}
	return entries, warnings > 0, warnings
}

// decodeEntry accepts only a complete line carrying both timestamps, as
// Hex's serde requires; explicit zeros are legitimate, omitted or null are not.
func decodeEntry(line []byte) (Entry, bool) {
	var wire struct {
		Entry
		StartMS *int64 `json:"start_ms"`
		EndMS   *int64 `json:"end_ms"`
	}
	if line[len(line)-1] != '\n' || json.Unmarshal(line, &wire) != nil || wire.StartMS == nil || wire.EndMS == nil {
		return Entry{}, false
	}
	wire.Entry.StartMS, wire.Entry.EndMS = *wire.StartMS, *wire.EndMS
	return wire.Entry, evidence.ValidEntry(wire.Entry) && strings.TrimSpace(wire.Text) != ""
}
