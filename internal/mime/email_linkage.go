package mime

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	stdmime "mime"
	"mime/multipart"
	"net/mail"
	"regexp"
	"slices"
	"strings"
)

const (
	EmailLinkageVersion  = 2
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
// automatic linkage or handling cannot be treated as complete.
type EmailLinkage struct {
	Version           int      `json:"version"`
	MessageID         string   `json:"message_id,omitempty"`
	Replies           []string `json:"replies,omitempty"`
	References        []string `json:"references,omitempty"`
	AttachedOriginals []string `json:"attached_originals,omitempty"`
	Outcome           string   `json:"outcome"`
}

// The automatic-link grammar is intentionally separate from the historical
// NormalizeMessageID comparison contract. Unsupported obsolete forms remain
// visible with an incomplete outcome rather than acquiring a guessed identity.
var emailLinkageID = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*@(?:[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*|\\[[^\\[\\]\\\\\\s]*\\])$")

// ExtractEmailLinkage reads archived headers and explicit message/rfc822 parts.
// It does not recursively read attached originals' bodies or delivery reports.
func ExtractEmailLinkage(raw []byte) EmailLinkage {
	result := EmailLinkage{Version: EmailLinkageVersion, Outcome: "complete"}
	if len(raw) > EmailLinkageMaxBytes {
		result.Outcome = "size_limit"
		return result
	}
	header, err := emailLinkageHeader(bytes.NewReader(raw))
	if err != nil {
		result.Outcome = err.Error()
		return result
	}
	result.MessageID = result.singleID(header)
	result.Replies = result.ids(header["In-Reply-To"])
	result.References = result.ids(header["References"])
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		result.incomplete("invalid_headers")
		return result
	}
	parts := 0
	if kind, _, _ := stdmime.ParseMediaType(header.Get("Content-Type")); kind == "message/rfc822" {
		result.original(message.Body, header.Get("Content-Transfer-Encoding"))
	} else {
		result.walkParts(message.Body, header.Get("Content-Type"), 0, &parts)
	}
	return result
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
	ids, valid := parseEmailLinkageIDs(values[0], false)
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

// Ignore CFWS, including nested comments, outside actual bracketed IDs. A
// malformed token cannot create a valid ID by trimming arbitrary brackets.
func emailLinkageIDs(value string) ([]string, bool) {
	return parseEmailLinkageIDs(value, true)
}

func parseEmailLinkageIDs(value string, deduplicate bool) ([]string, bool) {
	var ids []string
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
				return nil, false
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
				return nil, false
			}
			id := value[i+1 : end]
			if strings.IndexFunc(id, func(r rune) bool { return r < 33 || r > 126 }) >= 0 || !emailLinkageID.MatchString(id) {
				return nil, false
			}
			if len(ids) <= emailLinkageMaxIDs && (!deduplicate || !slices.Contains(ids, id)) {
				ids = append(ids, id)
			}
			// Validate all remaining syntax, retaining at most one overflow ID.
			i = end + 1
		default:
			return nil, false
		}
	}
	return ids, true
}

type emailLinkageError string

func (e emailLinkageError) Error() string { return string(e) }

func emailLinkageHeader(reader io.Reader) (mail.Header, error) {
	buffer := bufio.NewReader(io.LimitReader(reader, maxRecoveryHeaderBytes+1))
	var header bytes.Buffer
	for {
		line, err := buffer.ReadString('\n')
		if header.Len()+len(line) > maxRecoveryHeaderBytes {
			return nil, emailLinkageError("header_limit")
		}
		header.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
		if err != nil {
			return nil, emailLinkageError("invalid_headers")
		}
	}
	message, err := mail.ReadMessage(bytes.NewReader(header.Bytes()))
	if err != nil {
		return nil, emailLinkageError("invalid_headers")
	}
	return message.Header, nil
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
	bounded := &emailLinkagePartReader{reader: bufio.NewReader(reader), boundary: "--" + params["boundary"]}
	parts := multipart.NewReader(bounded, params["boundary"])
	for {
		part, err := parts.NextRawPart()
		if err == io.EOF {
			return
		}
		if err != nil {
			if errors.Is(err, emailLinkageError("header_limit")) {
				e.incomplete("header_limit")
			} else {
				e.incomplete("invalid_mime")
			}
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
	header, err := emailLinkageHeader(reader)
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

// emailLinkagePartReader enforces the raw header byte bound before multipart's
// larger built-in allowance can allocate a MIME part header. It streams body
// lines in fixed chunks and recognizes only this multipart's boundary lines.
type emailLinkagePartReader struct {
	reader      *bufio.Reader
	boundary    string
	pending     []byte
	linePrefix  []byte
	lineInvalid bool
	whitespace  bool
	inHeaders   bool
	headerBytes int
	lineBytes   int
	lineFirst   byte
	err         error
}

func (r *emailLinkagePartReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		chunk, err := r.reader.ReadSlice('\n')
		r.err = err
		if errors.Is(err, bufio.ErrBufferFull) {
			r.err = nil
		}
		for _, c := range chunk {
			r.lineBytes++
			if r.lineBytes == 1 {
				r.lineFirst = c
			}
			if r.inHeaders {
				r.headerBytes++
				if r.headerBytes > maxRecoveryHeaderBytes {
					r.err = emailLinkageError("header_limit")
					return 0, r.err
				}
			} else if !r.lineInvalid {
				if len(r.linePrefix) < len(r.boundary) && c != '\r' && c != '\n' {
					r.linePrefix = append(r.linePrefix, c)
				} else if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					r.whitespace = true
				} else if r.whitespace || len(r.linePrefix) >= len(r.boundary)+2 {
					r.lineInvalid = true
				} else {
					r.linePrefix = append(r.linePrefix, c)
				}
			}
			if c == '\n' {
				if r.inHeaders {
					if r.lineBytes == 1 || (r.lineBytes == 2 && r.lineFirst == '\r') {
						r.inHeaders = false
					}
				} else if !r.lineInvalid && string(r.linePrefix) == r.boundary {
					r.inHeaders = true
					r.headerBytes = 0
				}
				r.lineBytes = 0
				r.lineFirst = 0
				r.linePrefix = r.linePrefix[:0]
				r.lineInvalid = false
				r.whitespace = false
			}
		}
		r.pending = chunk
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	if n > 0 {
		return n, nil
	}
	return 0, r.err
}
