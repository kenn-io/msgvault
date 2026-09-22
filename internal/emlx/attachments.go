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
// bytes, including the message's line-ending style, are preserved.
func RestoreAttachments(raw []byte, messagePath string, maxBytes int64) ([]byte, int, error) {
	if !bytes.Contains(raw, []byte(applePlaceholderHeader)) {
		return raw, 0, nil
	}
	attDir := attachmentsDir(messagePath)
	if attDir == "" {
		return raw, 0, nil
	}
	nl := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		nl = "\r\n"
	}
	remaining := maxBytes - int64(len(raw))
	if remaining <= 0 {
		return raw, 0, nil
	}
	lines := strings.Split(string(raw), nl)

	// Locate the top-level boundary in the message header.
	hdrEnd := indexBlank(lines, 0)
	if hdrEnd < 0 {
		return raw, 0, nil
	}
	boundary := findBoundary(lines[:hdrEnd])
	if boundary == "" {
		return raw, 0, nil
	}
	open, closeB := "--"+boundary, "--"+boundary+"--"

	var out []string
	restored := 0
	var restoreErr error
	partIndex := 0
	i := 0
	for i < len(lines) {
		line := lines[i]
		if line != open {
			out = append(out, line)
			i++
			continue
		}
		// Start of a part: copy the boundary line, then read its header.
		partIndex++
		out = append(out, line)
		i++
		phEnd := indexBlank(lines, i)
		if phEnd < 0 {
			out = append(out, lines[i:]...)
			break
		}
		header := lines[i:phEnd]
		if !hasPlaceholder(header) {
			out = append(out, header...)
			i = phEnd
			continue
		}
		// Replace the encoding header, including folded continuations, to
		// match the base64 body written below.
		var restoredHeader []string
		drop := false
		for _, h := range header {
			if !strings.HasPrefix(h, " ") && !strings.HasPrefix(h, "\t") {
				name, _, _ := strings.Cut(h, ":")
				drop = strings.EqualFold(name, "X-Apple-Content-Length") ||
					strings.EqualFold(name, "Content-Transfer-Encoding")
			}
			if !drop {
				restoredHeader = append(restoredHeader, h)
			}
		}
		restoredHeader = append(restoredHeader, "Content-Transfer-Encoding: base64")
		// Skip files already known to exceed the budget, then bound the read
		// and charge its actual size in case the cache changed after Stat.
		file, size, err := resolveAttachment(attDir, strconv.Itoa(partIndex), findFilename(header))
		headerGrowth := len(strings.Join(restoredHeader, nl)) - len(strings.Join(header, nl))
		cost := encodedSize(size, len(nl)) + int64(headerGrowth)
		var content []byte
		if err == nil && file != "" && cost <= remaining {
			content, err = readAttachment(file, remaining-int64(headerGrowth))
			cost = encodedSize(int64(len(content)), len(nl)) + int64(headerGrowth)
		}
		if err != nil || file == "" || cost > remaining {
			restoreErr = errors.Join(restoreErr, err)
			out = append(out, header...)
			i = phEnd
			continue
		}
		remaining -= cost
		// Emit the header without the placeholder, then the base64 body,
		// and skip the original (empty) body up to the next boundary line.
		out = append(out, restoredHeader...)
		out = append(out, "")
		out = append(out, base64Lines(content)...)
		i = phEnd + 1
		for i < len(lines) && lines[i] != open && lines[i] != closeB {
			i++
		}
		restored++
	}
	return []byte(strings.Join(out, nl)), restored, restoreErr
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
func indexBlank(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if lines[i] == "" {
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
