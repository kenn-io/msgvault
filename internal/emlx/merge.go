package emlx

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// MergeResult keeps archived parts when local Apple Mail cache entries vanish.
type MergeResult struct {
	Raw          []byte
	ChangedParts int
	Incomplete   bool
	SourceParts  map[string]string
}

// MergeAttachments overlays supported top-level parts using the original layout.
// It never charges preserved bytes against a lower new-material budget.
func MergeAttachments(original, restored, archived []byte, parts []RestorationPart, maxBytes int64, acknowledged map[string]string) (MergeResult, error) {
	result := MergeResult{Raw: archived, SourceParts: maps.Clone(acknowledged)}
	if len(archived) == 0 {
		result.Raw = original
	}
	if len(parts) == 0 {
		return result, nil
	}
	orig, err := splitParts(original)
	if err != nil {
		return result, err
	}
	fresh, err := splitParts(restored)
	if err != nil {
		return result, err
	}
	old, err := splitParts(result.Raw)
	if err != nil {
		return result, err
	}
	if len(orig) != len(fresh) || len(orig) != len(old) {
		return result, errors.New("incompatible EMLX attachment layout")
	}
	for i := range orig {
		if stablePartKey(orig[i]) != stablePartKey(fresh[i]) || stablePartKey(orig[i]) != stablePartKey(old[i]) {
			return result, fmt.Errorf("incompatible EMLX part %d", i)
		}
	}
	chosen := append([][]byte(nil), old...)
	// Apply shrinking replacements first so their released space can admit additions.
	var candidates []int
	for _, part := range parts {
		i, err := strconv.Atoi(part.Key)
		if err != nil || i <= 0 || i >= len(orig)-1 {
			return result, errors.New("invalid EMLX part key")
		}
		switch part.State {
		case "supplied":
			// A previously acknowledged source part cannot replay older bytes
			// over another occurrence's newer contribution to the archive.
			if part.ContentHash != "" && len(archived) > 0 && acknowledged[part.Key] == part.ContentHash {
				continue
			}
			if !bytes.Equal(fresh[i], old[i]) {
				candidates = append(candidates, i)
			}
		case "error", "excluded":
			result.Incomplete = true
		case "missing", "unsupported", "source-excluded":
		default:
			return result, errors.New("invalid EMLX restoration state")
		}
	}
	size := len(result.Raw)
	for _, i := range candidates {
		if len(fresh[i]) <= len(old[i]) {
			size += len(fresh[i]) - len(old[i])
			chosen[i] = fresh[i]
			result.ChangedParts++
		}
	}
	for _, i := range candidates {
		if len(fresh[i]) <= len(old[i]) {
			continue
		}
		next := size + len(fresh[i]) - len(old[i])
		if int64(next) > maxBytes {
			result.Incomplete = true
			continue
		}
		size = next
		chosen[i] = fresh[i]
		result.ChangedParts++
	}
	for _, part := range parts {
		if part.State != "supplied" || part.ContentHash == "" {
			continue
		}
		i, _ := strconv.Atoi(part.Key) // Every key was validated above.
		// Only selected bytes can become new contribution evidence. Rejected
		// replacements retain their previous acknowledgment and remain retryable.
		if bytes.Equal(chosen[i], fresh[i]) {
			if result.SourceParts == nil {
				result.SourceParts = make(map[string]string)
			}
			result.SourceParts[part.Key] = part.ContentHash
		}
	}
	result.Raw = bytes.Join(chosen, nil)
	return result, nil
}

// splitParts keeps every original byte, including mixed line endings and the
// preamble/epilogue. Closing delimiters may carry MIME trailing whitespace;
// EOF after the final part is retained without inventing a closing delimiter.
func splitParts(raw []byte) ([][]byte, error) {
	lines := strings.Split(string(raw), "\n")
	hdr := indexBlank(lines, 0)
	if hdr < 0 {
		return nil, errors.New("missing EMLX MIME headers")
	}
	boundary := findBoundary(lines[:hdr])
	if boundary == "" {
		return nil, errors.New("missing EMLX multipart boundary")
	}
	open, closeB := "--"+boundary, "--"+boundary+"--"
	var starts []int
	offset := 0
	closed := false
	for _, line := range lines {
		trimmed := strings.TrimSuffix(line, "\r")
		closing := strings.TrimRight(trimmed, " \t") == closeB
		if !closed && (trimmed == open || closing) {
			starts = append(starts, offset)
			if closing {
				closed = true
			}
		}
		offset += len(line) + 1
	}
	if len(starts) == 0 || (closed && len(starts) < 2) {
		return nil, errors.New("incomplete EMLX multipart layout")
	}
	out := [][]byte{raw[:starts[0]]}
	for i, start := range starts {
		end := len(raw)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out = append(out, raw[start:end])
	}
	if !closed {
		// Keep the final real part at its original index. An empty trailer lets
		// the merge use the same part bounds without manufacturing source bytes.
		out = append(out, nil)
	}
	return out, nil
}

func stablePartKey(part []byte) string {
	lines := strings.Split(string(part), "\n")
	end := indexBlank(lines, 0)
	if end < 0 {
		return string(part)
	}
	var out []string
	drop := false
	for _, line := range lines[:end] {
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			name, _, _ := strings.Cut(line, ":")
			drop = strings.EqualFold(name, "X-Apple-Content-Length") || strings.EqualFold(name, "Content-Transfer-Encoding")
		}
		if !drop {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
