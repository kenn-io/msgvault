package mime

import (
	"bytes"
	"io"
	stdmime "mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
	"unicode"
)

// This obsolete generator inserts a fixed, undeclared delimiter at the start
// of a CSV part's headers. Restrict recovery to that complete signature, after
// ordinary parsing failed; do not reinterpret arbitrary malformed messages.
var legacyCSVHeader = regexp.MustCompile(`(?m)^(--[^\r\n]{1,200}\r?\n)[ \t]*--------------040908010102080206010104[ \t]*\r?\n(Content-Type: text/csv;[^\r\n]*\r?\nContent-Transfer-Encoding: quoted-printable\r?\nContent-Disposition: attachment;[^\r\n]*\r?\n)`)

const legacyCSVRecoveryNotice = "Recovered legacy CSV MIME framing on a copy; original raw MIME preserved"

func recoverLegacyCSV(raw []byte) *Message {
	if len(raw) > 16*1024*1024 {
		return nil
	}
	matches := legacyCSVHeader.FindAllSubmatchIndex(raw, 2)
	if len(matches) != 1 {
		return nil
	}
	m := matches[0]
	delimiter := bytes.TrimRight(raw[m[2]:m[3]], "\r\n")
	boundary := string(delimiter[2:])
	if !declaresMultipartBoundary(raw, boundary) || declaresMultipartBoundary(raw, "------------040908010102080206010104") {
		return nil
	}
	// All edits use freshly allocated storage. The caller retains the exact
	// original slice for hashing, raw storage, and provider identity checks.
	fixed := make([]byte, 0, len(raw)+4)
	fixed = append(fixed, raw[:m[3]]...)
	fixed = append(fixed, raw[m[4]:]...)
	ranges := legacyCSVPartRanges(fixed, delimiter)
	if len(ranges) == 0 {
		return nil
	}
	var complete *Message
	if recovered, err := parse(fixed, nil); err == nil {
		complete = acceptLegacyCSVRecovery(recovered)
	}
	parts := make([][]byte, len(ranges))
	for i, span := range ranges {
		parts[i] = fixed[span[0]:span[1]]
	}
	csvIndex := -1
	var csvPayload []byte
	for i, part := range parts {
		headerEnd := bytes.Index(part, []byte("Content-Disposition: attachment;"))
		if !bytes.HasPrefix(bytes.TrimLeft(part, "\r\n"), []byte("Content-Type: text/csv;")) || headerEnd < 0 {
			continue
		}
		if csvIndex >= 0 {
			return nil
		}
		if complete != nil {
			payload, ok := legacyCSVBody(part)
			if !ok {
				return nil
			}
			csvIndex, csvPayload = i, trimLegacyPartNewline(payload)
			continue
		}
		lineEnd := bytes.IndexByte(part[headerEnd:], '\n')
		if lineEnd < 0 {
			return nil
		}
		bodyStart := headerEnd + lineEnd + 1
		payload := part[bodyStart:]
		if !bytes.HasPrefix(payload, []byte("\r\n")) && !bytes.HasPrefix(payload, []byte("\n")) {
			// Extra headers and folded lines make this boundary ambiguous.
			// Do not turn unfamiliar header layouts into attachment content.
			firstLine, _, _ := bytes.Cut(payload, []byte{'\n'})
			name, _, hasColon := bytes.Cut(firstLine, []byte{':'})
			if (hasColon && validHeaderName(name)) || bytes.HasPrefix(firstLine, []byte{' '}) || bytes.HasPrefix(firstLine, []byte{'\t'}) {
				return nil
			}
			parts[i] = append(append(bytes.Clone(part[:bodyStart]), '\r', '\n'), payload...)
		} else if bytes.HasPrefix(payload, []byte("\r\n")) {
			payload = payload[2:]
		} else {
			payload = payload[1:]
		}
		csvIndex, csvPayload = i, trimLegacyPartNewline(payload)
	}
	if csvIndex < 0 || !hasDecodableCSVPayload(csvPayload) {
		return nil
	}
	// One variant duplicates the same CSV under an unlabelled application/pdf
	// part and omits both header/body separators. Its entire payload must match
	// the CSV, allowing one extra terminal line ending in the CSV. That extra
	// line ending remains in the recovered attachment.
	repairedSibling := false
	for i, part := range parts {
		trimmed := bytes.TrimLeft(part, "\r\n")
		if !bytes.HasPrefix(trimmed, []byte("Content-Type: application/pdf\r\n")) &&
			!bytes.HasPrefix(trimmed, []byte("Content-Type: application/pdf\n")) {
			continue
		}
		bodyStart := len(part) - len(trimmed) + bytes.IndexByte(trimmed, '\n') + 1
		payload := part[bodyStart:]
		if bytes.HasPrefix(payload, []byte("\r\n")) || bytes.HasPrefix(payload, []byte("\n")) {
			continue
		}
		pdfPayload := trimLegacyPartNewline(payload)
		if !bytes.Equal(pdfPayload, csvPayload) && !bytes.Equal(pdfPayload, trimLegacyPartNewline(csvPayload)) {
			return nil
		}
		parts[i] = append(append(bytes.Clone(part[:bodyStart]), '\r', '\n'), payload...)
		repairedSibling = true
	}
	if complete != nil && !repairedSibling {
		return complete
	}
	var repaired bytes.Buffer
	start := 0
	for i, span := range ranges {
		repaired.Write(fixed[start:span[0]])
		repaired.Write(parts[i])
		start = span[1]
	}
	repaired.Write(fixed[start:])
	recovered, err := parse(repaired.Bytes(), nil)
	if err != nil {
		return nil
	}
	return acceptLegacyCSVRecovery(recovered)
}

func legacyCSVPartRanges(raw, delimiter []byte) [][2]int {
	closing := append(bytes.Clone(delimiter), '-', '-')
	var parts [][2]int
	start, offset := -1, 0
	for line := range bytes.SplitAfterSeq(raw, []byte{'\n'}) {
		marker := line
		hasNewline := bytes.HasSuffix(marker, []byte{'\n'})
		if hasNewline {
			marker = bytes.TrimSuffix(marker, []byte{'\n'})
			marker = bytes.TrimSuffix(marker, []byte{'\r'})
		}
		marker = bytes.TrimRight(marker, " \t")
		isClosing := bytes.Equal(marker, closing)
		if isClosing || (hasNewline && bytes.Equal(marker, delimiter)) {
			if start >= 0 {
				parts = append(parts, [2]int{start, offset})
			}
			if isClosing {
				return parts
			}
			start = offset + len(line)
		} else {
			// The parser also recognizes some boundary prefixes within lines.
			// Refuse those inputs before it can truncate recovered content.
			if bytes.Contains(line, closing) {
				return nil
			}
			for remaining := line; len(remaining) > 0; {
				_, suffix, found := bytes.Cut(remaining, delimiter)
				if !found {
					break
				}
				if len(suffix) > 0 && unicode.IsSpace(rune(suffix[0])) {
					return nil
				}
				remaining = suffix
			}
		}
		offset += len(line)
	}
	return nil
}

func legacyCSVBody(part []byte) ([]byte, bool) {
	for {
		line, rest, ok := bytes.Cut(part, []byte{'\n'})
		if !ok {
			return nil, false
		}
		if len(bytes.TrimSuffix(line, []byte{'\r'})) == 0 {
			return rest, true
		}
		part = rest
	}
}

func trimLegacyPartNewline(payload []byte) []byte {
	if bytes.HasSuffix(payload, []byte("\r\n")) {
		return payload[:len(payload)-2]
	}
	return bytes.TrimSuffix(payload, []byte{'\n'})
}

func acceptLegacyCSVRecovery(recovered *Message) *Message {
	if recovered.BodyHTML == "" || recovered.GetBodyText() == "" {
		return nil
	}
	nonempty, csvCount := 0, 0
	for _, attachment := range recovered.Attachments {
		if len(attachment.Content) > 0 {
			nonempty++
			if attachment.ContentType == "text/csv" && len(bytes.TrimSpace(attachment.Content)) > 0 {
				csvCount++
			}
		}
	}
	if nonempty != 1 || csvCount != 1 {
		return nil
	}
	recovered.Errors = append(recovered.Errors, legacyCSVRecoveryNotice)
	return recovered
}

func declaresMultipartBoundary(raw []byte, boundary string) bool {
	// Require a declared multipart parent, and refuse removal of an actual
	// declared delimiter. Folded or otherwise unfamiliar headers fail closed.
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		value, ok := strings.CutPrefix(string(bytes.TrimRight(line, "\r")), "Content-Type: ")
		if !ok {
			continue
		}
		kind, params, err := stdmime.ParseMediaType(value)
		if err == nil && strings.HasPrefix(kind, "multipart/") && params["boundary"] == boundary {
			return true
		}
	}
	return false
}

func hasDecodableCSVPayload(payload []byte) bool {
	// Attachment bytes are opaque to MIME, even when the declared type is CSV.
	// This generator also emits single-column/text payloads under that type.
	// Require a nonempty decodable payload, without rewriting or validating rows.
	decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(payload)))
	return err == nil && len(bytes.TrimSpace(decoded)) > 0
}
