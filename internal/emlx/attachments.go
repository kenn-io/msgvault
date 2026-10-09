package emlx

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// applePlaceholderHeader marks an attachment part whose body Apple Mail did
// not write into the .partial.emlx. The attachment bytes live next to the
// Messages/ directory instead, under Attachments/<msg>/<part-index>/<name>.
const applePlaceholderHeader = "X-Apple-Content-Length:"

var (
	boundaryRe = regexp.MustCompile(`(?i)boundary\s*=\s*"?([^";\s]+)"?`)
	filenameRe = regexp.MustCompile(`(?i)filename="?([^";]+)"?`)
)

// attachmentsDir returns Apple Mail's Attachments/<num> directory for the
// message at path, without checking whether the directory exists. It returns
// "" when path is not a Messages/<num>.partial.emlx file.
func attachmentsDir(path string) string {
	base := filepath.Base(path)
	if !IsPartial(base) {
		return ""
	}
	num := strings.TrimSuffix(base, ".partial.emlx")
	if _, err := strconv.Atoi(num); err != nil {
		return ""
	}
	msgDir := filepath.Dir(path)
	if filepath.Base(msgDir) != "Messages" {
		return ""
	}
	return filepath.Join(filepath.Dir(msgDir), "Attachments", num)
}

// RestoreAttachments fills top-level attachment parts carrying an
// X-Apple-Content-Length placeholder with cached bodies beside messagePath.
// Parts whose file cannot be found are left as they are, and so are parts
// whose base64-encoded size would push the message past maxBytes. All other
// bytes, including each line's ending, are preserved.
func RestoreAttachments(raw []byte, messagePath string, maxBytes int64) ([]byte, int, error) {
	restored, n, _, err := restoreAttachments(raw, messagePath, maxBytes)
	return restored, n, err
}

// RestorationPart records why each supported placeholder was or was not filled.
type RestorationPart struct {
	Key   string
	State string
	Err   error
}

func restoreAttachments(raw []byte, messagePath string, maxBytes int64) ([]byte, int, []RestorationPart, error) {
	return restoreSelectedAttachments(raw, messagePath, maxBytes, "")
}

// A nonempty selected key bounds restoration to one original top-level part.
func restoreSelectedAttachments(raw []byte, messagePath string, maxBytes int64, selected string) ([]byte, int, []RestorationPart, error) {
	if !bytes.Contains(raw, []byte(applePlaceholderHeader)) {
		return raw, 0, nil, nil
	}
	attDir := attachmentsDir(messagePath)
	if attDir == "" {
		return raw, 0, nil, nil
	}
	remaining := maxBytes - int64(len(raw))

	// Split on LF and keep any CR on its line: one message can mix both
	// endings, as when a relay folds a header it added with CRLF.
	lines := strings.Split(string(raw), "\n")

	// Locate the top-level boundary in the message header.
	hdrEnd := indexBlank(lines, 0)
	if hdrEnd < 0 {
		return raw, 0, nil, nil
	}
	boundary := findBoundary(lines[:hdrEnd])
	if boundary == "" {
		return raw, 0, nil, nil
	}
	open, closeB := "--"+boundary, "--"+boundary+"--"

	var out []string
	restored := 0
	var restoreErr error
	var parts []RestorationPart
	partIndex := 0
	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSuffix(line, "\r") != open {
			out = append(out, line)
			i++
			continue
		}
		// Start of a part: copy the boundary line, then read its header.
		partIndex++
		// Lines added to this part take the ending of its boundary line.
		cr := ""
		if strings.HasSuffix(line, "\r") {
			cr = "\r"
		}
		out = append(out, line)
		i++
		phEnd := indexBlank(lines, i)
		if phEnd < 0 {
			out = append(out, lines[i:]...)
			break
		}
		header := lines[i:phEnd]
		if !hasPlaceholder(header) || (selected != "" && selected != strconv.Itoa(partIndex)) {
			out = append(out, header...)
			i = phEnd
			continue
		}
		// Replace the encoding header, including folded continuations, to
		// match the base64 body written below.
		restoredHeader := attachmentHeaders(header, cr)
		// Skip files already known to exceed the budget, then bound the read
		// and charge its actual size in case the cache changed after Stat.
		file, size, err := resolveAttachment(attDir, strconv.Itoa(partIndex), findFilename(header))
		headerGrowth := len(strings.Join(restoredHeader, "\n")) - len(strings.Join(header, "\n"))
		// Include the separator's line ending when charging replacement bytes.
		headerGrowth += len(cr) - len(lines[phEnd])
		bodyEnd := phEnd + 1
		bodyBytes := int64(0)
		for bodyEnd < len(lines) {
			l := strings.TrimSuffix(lines[bodyEnd], "\r")
			if l == open || strings.TrimRight(l, " \t") == closeB {
				break
			}
			bodyBytes += int64(len(lines[bodyEnd]) + 1)
			bodyEnd++
		}
		if bodyEnd == len(lines) {
			bodyBytes-- // The original final line has no LF at EOF.
		}
		restoredCost := func(size int64) int64 {
			encoded := encodedSize(size, len(cr)+1)
			if size == 0 {
				encoded = int64(len(cr) + 1) // base64Lines still emits an empty line.
			}
			cost := encoded + int64(headerGrowth) - bodyBytes
			if bodyEnd == len(lines) {
				// Join writes no final LF when this part ends at EOF.
				cost--
			}
			return cost
		}
		cost := restoredCost(size)
		var content []byte
		if err == nil && file != "" && cost <= remaining {
			content, err = readAttachment(file, remaining-int64(headerGrowth)+bodyBytes)
			cost = restoredCost(int64(len(content)))
		}
		if err != nil || file == "" || cost > remaining {
			state := "source-excluded"
			if file == "" {
				state = "missing"
			}
			if err != nil {
				state = "error"
			}
			parts = append(parts, RestorationPart{Key: strconv.Itoa(partIndex), State: state, Err: err})
			restoreErr = errors.Join(restoreErr, err)
			out = append(out, header...)
			i = phEnd
			continue
		}
		remaining -= cost
		// Emit the header without the placeholder, then the base64 body,
		// and skip the original (empty) body up to the next boundary line.
		out = append(out, restoredHeader...)
		out = append(out, cr)
		for _, l := range base64Lines(content) {
			out = append(out, l+cr)
		}
		i = bodyEnd
		parts = append(parts, RestorationPart{Key: strconv.Itoa(partIndex), State: "supplied"})
		restored++
	}
	return []byte(strings.Join(out, "\n")), restored, parts, restoreErr
}

// attachmentHeaders replaces placeholder and encoding headers together with
// their folded continuations, preserving every other original header byte.
func attachmentHeaders(header []string, cr string) []string {
	var out []string
	drop := false
	for _, h := range header {
		if !strings.HasPrefix(h, " ") && !strings.HasPrefix(h, "\t") {
			name, _, _ := strings.Cut(h, ":")
			drop = strings.EqualFold(name, "X-Apple-Content-Length") || strings.EqualFold(name, "Content-Transfer-Encoding")
		}
		if !drop {
			out = append(out, h)
		}
	}
	return append(out, "Content-Transfer-Encoding: base64"+cr)
}

// readAttachment reads at most maxBytes+1 bytes. The extra byte ensures that
// truncating an oversized file cannot make its encoded content fit the budget.
func readAttachment(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxBytes+1))
}

// indexBlank returns the index of the first empty line at or after from.
// A line holding only the CR of a CRLF ending counts as empty.
func indexBlank(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if lines[i] == "" || lines[i] == "\r" {
			return i
		}
	}
	return -1
}

func hasPlaceholder(header []string) bool {
	for _, h := range header {
		if strings.HasPrefix(h, applePlaceholderHeader) {
			return true
		}
	}
	return false
}

// unfold joins folded header lines so regexps can match across continuations.
func unfold(header []string) string {
	var b strings.Builder
	for _, h := range header {
		if strings.HasPrefix(h, " ") || strings.HasPrefix(h, "\t") {
			b.WriteString(strings.TrimLeft(h, " \t"))
			continue
		}
		b.WriteString("\n")
		b.WriteString(h)
	}
	return b.String()
}

func findBoundary(header []string) string {
	for l := range strings.SplitSeq(unfold(header), "\n") {
		if !strings.HasPrefix(strings.ToLower(l), "content-type:") {
			continue
		}
		if m := boundaryRe.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

// findFilename returns the attachment's filename from the part header, or ""
// when there is none or when the value is not a plain file name. The header
// is sender-controlled, so anything with a path separator or a parent
// reference is rejected here rather than being joined onto a path later.
func findFilename(header []string) string {
	m := filenameRe.FindStringSubmatch(unfold(header))
	if m == nil {
		return ""
	}
	name := strings.TrimSpace(m[1])
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return ""
	}
	return name
}

// resolveAttachment returns the path and size of attDir/<partID>/<name>
// without reading it. When the exact name cannot be resolved but the part directory
// holds exactly one file, that file is used, since Apple Mail stores one file
// per part and may have decoded the name differently than the raw header
// spells it. name must already have passed findFilename's validation.
func resolveAttachment(attDir, partID, name string) (string, int64, error) {
	dir := filepath.Join(attDir, partID)
	var nameErr error
	if name != "" {
		full := filepath.Join(dir, name)
		if fi, err := os.Stat(full); err == nil && fi.Mode().IsRegular() {
			return full, fi.Size(), nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			// Encoded header names can be invalid on the local filesystem.
			// Try the cached filename before reporting this lookup failure.
			nameErr = err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, nil
		}
		return "", 0, err
	}
	var files []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e)
		}
	}
	if len(files) != 1 {
		return "", 0, nameErr
	}
	full := filepath.Join(dir, files[0].Name())
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return "", 0, err
	}
	return full, fi.Size(), nil
}

// encodedSize is the number of bytes size raw bytes occupy once base64-encoded
// in 76-character lines, each terminated by a newline of nlLen bytes.
func encodedSize(size int64, nlLen int) int64 {
	enc := int64(base64.StdEncoding.EncodedLen(int(size)))
	lines := enc / 76
	if enc%76 != 0 {
		lines++
	}
	return enc + lines*int64(nlLen)
}

// base64Lines encodes b as RFC 2045 base64 with 76-character lines.
func base64Lines(b []byte) []string {
	enc := base64.StdEncoding.EncodeToString(b)
	var lines []string
	for len(enc) > 76 {
		lines = append(lines, enc[:76])
		enc = enc[76:]
	}
	return append(lines, enc)
}
