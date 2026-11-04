package mime

import (
	"bytes"
	"io"
	stdmime "mime"
	"mime/quotedprintable"
	"regexp"
	"strings"
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
	if recovered, err := parse(fixed, nil); err == nil {
		if accepted := acceptLegacyCSVRecovery(recovered); accepted != nil {
			return accepted
		}
	}
	parts := bytes.Split(fixed, delimiter)
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
		}
		csvIndex, csvPayload = i, bytes.Trim(payload, "\r\n")
	}
	if csvIndex < 0 || !hasDecodableCSVPayload(csvPayload) {
		return nil
	}
	// One variant duplicates the same CSV under an unlabelled application/pdf
	// part and omits both header/body separators. Only fix the second separator
	// when its entire payload is byte-identical to the identified CSV payload.
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
		if !bytes.Equal(bytes.Trim(payload, "\r\n"), csvPayload) {
			return nil
		}
		parts[i] = append(append(bytes.Clone(part[:bodyStart]), '\r', '\n'), payload...)
	}
	recovered, err := parse(bytes.Join(parts, delimiter), nil)
	if err != nil {
		return nil
	}
	return acceptLegacyCSVRecovery(recovered)
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
