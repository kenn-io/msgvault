package emlx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MergeAttachmentsFromFile considers sibling parts against current archived
// occupancy before reading them. A shrinking replacement frees space before
// additions; excluded growth preserves archived bytes and remains retryable.
// Each read is bounded by the source policy and holds at most one new part.
func MergeAttachmentsFromFile(ctx context.Context, original, archived []byte, messagePath string, maxBytes int64) (MergeResult, error) {
	result := MergeResult{Raw: archived}
	dir := attachmentsDir(messagePath)
	if dir == "" || !bytes.Contains(original, []byte(applePlaceholderHeader)) {
		return result, nil
	}
	orig, err := splitParts(original)
	if err != nil {
		return result, err
	}
	old, err := splitParts(archived)
	if err != nil {
		return result, err
	}
	if len(orig) != len(old) {
		return result, errors.New("incompatible EMLX attachment layout")
	}
	for i := range orig {
		if stablePartKey(orig[i]) != stablePartKey(old[i]) {
			return result, fmt.Errorf("incompatible EMLX part %d", i)
		}
	}
	type candidate struct {
		key   string
		index int
		size  int64
	}
	var shrinking, growing []candidate
	var restoreErr error
	for i := 1; i < len(orig)-1; i++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		lines := strings.Split(string(orig[i]), "\n")
		end := indexBlank(lines, 1)
		if end < 0 || !hasPlaceholder(lines[1:end]) {
			continue
		}
		header := lines[1:end]
		file, size, err := resolveAttachment(dir, strconv.Itoa(i), findFilename(header))
		if err != nil {
			result.Incomplete = true
			restoreErr = errors.Join(restoreErr, err)
			continue
		}
		if file == "" {
			continue
		}
		cr := ""
		if strings.HasSuffix(lines[0], "\r") {
			cr = "\r"
		}
		// Boundary, restored headers, separator and encoded body. Empty base64 also
		// emits one blank line, matching restoreSelectedAttachments exactly.
		encoded := encodedSize(size, len(cr)+1)
		if size == 0 {
			encoded = int64(len(cr) + 1)
		}
		freshSize := int64(len(lines[0])+1+len(strings.Join(attachmentHeaders(header, cr), "\n"))+1+len(cr)+1) + encoded
		if i == len(orig)-2 && len(orig[len(orig)-1]) == 0 {
			// At EOF the restorer emits no LF after the final encoded line.
			freshSize--
		}
		if int64(len(original))+freshSize-int64(len(orig[i])) > maxBytes {
			continue
		}
		c := candidate{key: strconv.Itoa(i), index: i, size: freshSize}
		if freshSize <= int64(len(old[i])) {
			shrinking = append(shrinking, c)
		} else {
			growing = append(growing, c)
		}
	}
	for _, c := range append(shrinking, growing...) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// old part lengths remain valid: each original key is considered once.
		delta := c.size - int64(len(old[c.index]))
		if delta > 0 && int64(len(result.Raw))+delta > maxBytes {
			result.Incomplete = true
			continue
		}
		restored, _, parts, err := restoreSelectedAttachments(original, messagePath, maxBytes, c.key)
		restoreErr = errors.Join(restoreErr, err)
		merged, err := MergeAttachments(original, restored, result.Raw, parts, maxBytes)
		if err != nil {
			return result, errors.Join(restoreErr, err)
		}
		// A post-Stat growth exclusion is retryable; it is not the source capability
		// decision made above. A post-read fingerprint also rejects publication.
		for _, part := range parts {
			if part.State == "source-excluded" {
				merged.Incomplete = true
			}
		}
		result.Raw = merged.Raw
		result.ChangedParts += merged.ChangedParts
		result.Incomplete = result.Incomplete || merged.Incomplete
	}
	return result, restoreErr
}
