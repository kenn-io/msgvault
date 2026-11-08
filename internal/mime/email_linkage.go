package mime

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	stdmime "mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strings"
	"unicode"
)

const (
	EmailLinkageMaxBytes = 64 << 20
	emailLinkageMaxDepth = 8
	emailLinkageMaxParts = 256
	emailLinkageMaxIDs   = 128
	// Keep ASCII keys bounded for both archive backends' value indexes.
	emailLinkageMaxIDBytes = 1024
)

// EmailLinkage contains only explicit MIME identity evidence. It never derives
// identity from a subject, display name, forwarded-text marker or quoted body.
// Non-complete outcomes preserve the readable message while explaining why
// automatic linkage cannot be treated as complete. The copy's own Message-ID
// is not stored here; it is messages.rfc822_message_id.
type EmailLinkage struct {
	Replies           []string `json:"replies,omitempty"`
	References        []string `json:"references,omitempty"`
	AttachedOriginals []string `json:"attached_originals,omitempty"`
	Outcome           string   `json:"outcome"`
}

// A key is an ID that NormalizeMessageID would store: whatever sits inside one
// bracket pair, or a whole value with no brackets, with no further brackets and no stray space, control or invalid bytes.
// Keys are therefore equal to messages.rfc822_message_id for the same header,
// including historical IDs that have no "@". Comments and folding around the
// brackets are skipped. Anything else stays visible as an incomplete outcome.
func validEmailLinkageID(id string) bool {
	// PostgreSQL TEXT cannot hold NUL, and no Message-ID holds a control byte.
	return id != "" && strings.IndexFunc(id, unicode.IsControl) < 0 && NormalizeMessageID("<"+id+">") == id
}

// ExtractEmailLinkage reads archived headers and explicit message/rfc822 parts.
// It does not recursively read attached originals' bodies or delivery reports.
func ExtractEmailLinkage(raw []byte) EmailLinkage {
	result := EmailLinkage{Outcome: "complete"}
	if len(raw) > EmailLinkageMaxBytes {
		result.Outcome = "size_limit"
		return result
	}
	header, size, err := emailLinkageHeader(bytes.NewReader(raw))
	if err != nil {
		result.Outcome = err.Error()
		return result
	}
	result.checkOwnID(header)
	result.Replies = result.ids(header["In-Reply-To"])
	result.References = result.ids(header["References"])
	body := bytes.NewReader(raw[size:])
	parts := 0
	if kind, _, _ := stdmime.ParseMediaType(header.Get("Content-Type")); kind == "message/rfc822" {
		result.original(body, header.Get("Content-Transfer-Encoding"))
	} else {
		result.walkParts(body, header.Get("Content-Type"), 0, &parts)
	}
	return result
}

// checkOwnID marks the result incomplete when the copy's own Message-ID header
// cannot be matched later. The value itself is messages.rfc822_message_id,
// which NormalizeMessageID derives; a header it rejects, such as one wrapped
// in a comment, stores no ID, so replies naming it could not find this copy.
func (e *EmailLinkage) checkOwnID(header mail.Header) {
	id := e.singleID(header)
	if id != "" && NormalizeMessageID(header["Message-Id"][0]) != id {
		e.incomplete("invalid_headers")
	}
}

func (e *EmailLinkage) incomplete(reason string) {
	if e.Outcome == "complete" {
		e.Outcome = reason
	}
}

func (e *EmailLinkage) singleID(header mail.Header) string {
	values := header["Message-Id"]
	if len(values) == 0 {
		return ""
	}
	if len(values) != 1 {
		e.incomplete("invalid_headers")
		return ""
	}
	ids, valid := emailLinkageIDs(values[0])
	if !valid || len(ids) != 1 {
		e.incomplete("invalid_headers")
		return ""
	}
	if len(ids[0]) > emailLinkageMaxIDBytes {
		e.incomplete("identity_limit")
		return ""
	}
	return ids[0]
}

func (e *EmailLinkage) ids(values []string) []string {
	var result []string
	for _, value := range values {
		ids, valid := emailLinkageIDs(value)
		if !valid {
			e.incomplete("invalid_headers")
		}
		for _, id := range ids {
			if len(id) > emailLinkageMaxIDBytes {
				e.incomplete("identity_limit")
				continue
			}
			if !slices.Contains(result, id) {
				if len(result) == emailLinkageMaxIDs {
					e.incomplete("identity_limit")
					return result
				}
				result = append(result, id)
			}
		}
	}
	return result
}

// emailLinkageIDs returns every valid ID in value. It ignores CFWS, including
// nested comments, outside actual bracketed IDs. A malformed token cannot
// create a valid ID by trimming arbitrary brackets; it is skipped and reported
// through valid, so one bad token keeps its neighbors. Text that cannot be
// tokenized, an unfinished comment or bracket, ends the scan.
//
// A value with no brackets or comments holds bare IDs, which NormalizeMessageID
// stores as they are.
//
// It does not reuse parseReferences or ParseThreadingHeaders because it must
// yield exactly the keys NormalizeMessageID stores.
func emailLinkageIDs(value string) (ids []string, valid bool) {
	valid = true
	bare := !strings.ContainsAny(value, "<>(")
	for i := 0; i < len(value); {
		switch value[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '(':
			depth := 1
			i++
			for i < len(value) && depth > 0 {
				if value[i] == '\\' {
					i += 2
					continue
				}
				if value[i] == '(' {
					depth++
				}
				if value[i] == ')' {
					depth--
				}
				i++
			}
			if depth != 0 || i > len(value) {
				return ids, false
			}
		case '<':
			end := i + 1
			literal := false
			for ; end < len(value); end++ {
				if value[end] == '[' {
					literal = true
				}
				if value[end] == ']' {
					literal = false
				}
				if value[end] == '>' && !literal {
					break
				}
			}
			if end == len(value) {
				return ids, false
			}
			id := value[i+1 : end]
			i = end + 1
			if !validEmailLinkageID(id) {
				valid = false
				continue
			}
			ids = append(ids, id)
		default:
			start := i
			for i < len(value) && !strings.ContainsRune(" \t\r\n<(", rune(value[i])) {
				i++
			}
			if bare && validEmailLinkageID(value[start:i]) {
				ids = append(ids, value[start:i])
			} else {
				valid = false
			}
		}
	}
	return ids, valid
}

type emailLinkageError string

func (e emailLinkageError) Error() string { return string(e) }

// emailLinkageHeader reads one header block and reports its length in bytes,
// so a caller holding the whole message can find the body without parsing
// the headers again. A block that ends without a blank line reads as net/mail
// reads it.
func emailLinkageHeader(reader io.Reader) (mail.Header, int, error) {
	buffer := bufio.NewReader(io.LimitReader(reader, maxRecoveryHeaderBytes+1))
	var header bytes.Buffer
	for {
		line, err := buffer.ReadString('\n')
		if header.Len()+len(line) > maxRecoveryHeaderBytes {
			return nil, 0, emailLinkageError("header_limit")
		}
		header.WriteString(line)
		if line == "\r\n" || line == "\n" || err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, emailLinkageError("invalid_headers")
		}
	}
	message, err := mail.ReadMessage(bytes.NewReader(header.Bytes()))
	if err != nil {
		return nil, 0, emailLinkageError("invalid_headers")
	}
	return message.Header, header.Len(), nil
}

func (e *EmailLinkage) walkParts(reader io.Reader, contentType string, depth int, count *int) {
	if contentType == "" {
		return
	}
	kind, params, err := stdmime.ParseMediaType(contentType)
	if err != nil {
		e.incomplete("invalid_mime")
		return
	}
	if kind == "multipart/report" || !strings.HasPrefix(kind, "multipart/") {
		return
	}
	if depth >= emailLinkageMaxDepth {
		e.incomplete("depth_limit")
		return
	}
	if params["boundary"] == "" {
		e.incomplete("invalid_mime")
		return
	}
	parts := multipart.NewReader(reader, params["boundary"])
	for {
		part, err := parts.NextRawPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			e.incomplete("invalid_mime")
			return
		}
		*count++
		if *count > emailLinkageMaxParts {
			_ = part.Close()
			e.incomplete("part_limit")
			return
		}
		partType, _, typeErr := stdmime.ParseMediaType(part.Header.Get("Content-Type"))
		if typeErr != nil && part.Header.Get("Content-Type") != "" {
			e.incomplete("invalid_mime")
		}
		// A part of a digest with no Content-Type is a message (RFC 2046 5.1.5).
		if partType == "" && kind == "multipart/digest" {
			partType = "message/rfc822"
		}
		if partType == "message/rfc822" {
			e.original(part, part.Header.Get("Content-Transfer-Encoding"))
		} else {
			e.walkParts(part, part.Header.Get("Content-Type"), depth+1, count)
		}
		_ = part.Close()
	}
}

// original reads only the attached message's headers, never its body.
func (e *EmailLinkage) original(reader io.Reader, transferEncoding string) {
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, reader)
	case "", "7bit", "8bit", "binary":
	default:
		e.incomplete("invalid_mime")
		return
	}
	header, _, err := emailLinkageHeader(reader)
	if err != nil {
		e.incomplete(err.Error())
		return
	}
	if id := e.singleID(header); id != "" && !slices.Contains(e.AttachedOriginals, id) {
		if len(e.AttachedOriginals) >= emailLinkageMaxIDs {
			e.incomplete("identity_limit")
		} else {
			e.AttachedOriginals = append(e.AttachedOriginals, id)
		}
	}
}
