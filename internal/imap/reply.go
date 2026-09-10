package imap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	msgmime "go.kenn.io/msgvault/internal/mime"
)

// ReplyDraft is the composed RFC822 message and its parsed representation.
type ReplyDraft struct {
	Raw    []byte
	Parsed *msgmime.Message
}

// BuildReply composes a single plain-text reply from an archived message.
func BuildReply(parentRaw []byte, from, body string, now time.Time, messageID string) (ReplyDraft, error) {
	if !utf8.ValidString(from) || strings.ContainsAny(from, "\r\n") {
		return ReplyDraft{}, errors.New("invalid reply From address")
	}
	fromAddress, err := parseOneAddress(from)
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid reply From address: %w", err)
	}
	if !utf8.ValidString(body) || strings.ContainsAny(body, "\x00") {
		return ReplyDraft{}, errors.New("invalid reply body")
	}

	parent, err := mail.ReadMessage(bytes.NewReader(parentRaw))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("read parent headers: %w", err)
	}
	if err := validateSingletonHeaders(parent.Header); err != nil {
		return ReplyDraft{}, err
	}
	replyTo := parent.Header.Get("Reply-To")
	if replyTo == "" {
		replyTo = parent.Header.Get("From")
	}
	addresses, err := mail.ParseAddressList(replyTo)
	if err != nil || len(addresses) == 0 {
		return ReplyDraft{}, errors.New("parent has invalid From or Reply-To")
	}
	for _, address := range addresses {
		if err := validateAddress(address); err != nil {
			return ReplyDraft{}, fmt.Errorf("parent has invalid reply address: %w", err)
		}
	}

	if messageID == "" {
		messageID, err = newReplyMessageID()
		if err != nil {
			return ReplyDraft{}, err
		}
	}
	messageID, err = normalizeWireMessageID(messageID)
	if err != nil {
		return ReplyDraft{}, err
	}
	parentIDs, err := parseMessageIDHeader(parent.Header.Get("Message-ID"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid parent Message-ID: %w", err)
	}
	if len(parentIDs) > 1 {
		return ReplyDraft{}, errors.New("invalid parent Message-ID: more than one ID")
	}
	inReplyTo, err := parseMessageIDHeader(parent.Header.Get("In-Reply-To"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid parent In-Reply-To: %w", err)
	}
	references, err := parseMessageIDHeader(parent.Header.Get("References"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid parent References: %w", err)
	}
	if len(references) == 0 {
		references = inReplyTo
	}
	var parentID string
	if len(parentIDs) == 1 {
		parentID = parentIDs[0]
		references = append(references, parentID)
	}

	subject := normalizeReplySubject(decodeHeader(parent.Header.Get("Subject")))
	fromValue := fromAddress.String()
	toValue := formatAddresses(addresses)
	if !validHeaderValue(subject) || !validHeaderValue(fromValue) || !validHeaderValue(toValue) {
		return ReplyDraft{}, errors.New("invalid reply header")
	}
	var raw bytes.Buffer
	writeHeader := func(name, value string) {
		_, _ = fmt.Fprintf(&raw, "%s: %s\r\n", name, value)
	}
	writeHeader("Date", now.UTC().Format(time.RFC1123Z))
	writeHeader("From", fromValue)
	writeHeader("To", toValue)
	if subject != "" {
		if !isASCII(subject) {
			subject = stdmime.QEncoding.Encode("UTF-8", subject)
		}
		writeHeader("Subject", subject)
	}
	writeHeader("Message-ID", "<"+messageID+">")
	if parentID != "" {
		writeHeader("In-Reply-To", "<"+parentID+">")
	}
	if len(references) > 0 {
		writeFoldedHeader(&raw, "References", formatMessageIDs(references))
	}
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", `text/plain; charset="utf-8"`)
	writeHeader("Content-Transfer-Encoding", "quoted-printable")
	raw.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&raw)
	if _, err := io.WriteString(qp, body); err != nil {
		return ReplyDraft{}, fmt.Errorf("encode reply body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return ReplyDraft{}, fmt.Errorf("close reply body: %w", err)
	}

	parsed, err := msgmime.Parse(raw.Bytes())
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse composed reply: %w", err)
	}
	return ReplyDraft{Raw: raw.Bytes(), Parsed: parsed}, nil
}

func newReplyMessageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate reply Message-ID: %w", err)
	}
	return hex.EncodeToString(raw[:]) + "@msgvault.local", nil
}

func parseOneAddress(value string) (*mail.Address, error) {
	addresses, err := mail.ParseAddressList(value)
	if err != nil || len(addresses) != 1 {
		return nil, errors.New("expected exactly one address")
	}
	if err := validateAddress(addresses[0]); err != nil {
		return nil, err
	}
	return addresses[0], nil
}

func validateAddress(address *mail.Address) error {
	if address == nil || address.Address == "" || strings.ContainsAny(address.Address, "\r\n") || !utf8.ValidString(address.Address) {
		return errors.New("address is empty or malformed")
	}
	if _, err := mail.ParseAddress(address.String()); err != nil {
		return fmt.Errorf("parse address: %w", err)
	}
	return nil
}

func validHeaderValue(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func decodeHeader(value string) string {
	decoded, err := new(stdmime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func validateSingletonHeaders(header mail.Header) error {
	for _, name := range []string{"From", "Reply-To", "Message-ID", "In-Reply-To", "References", "Subject"} {
		if len(headerValues(header, name)) > 1 {
			return fmt.Errorf("parent contains duplicate %s headers", name)
		}
	}
	return nil
}

func headerValues(header mail.Header, name string) []string {
	var values []string
	for key, entries := range header {
		if strings.EqualFold(key, name) {
			values = append(values, entries...)
		}
	}
	return values
}

// normalizeWireMessageID accepts one message ID with or without angle
// brackets and returns it bare.
func normalizeWireMessageID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || value[0] != '<' || value[len(value)-1] != '>' {
		if strings.Contains(value, "<") || strings.Contains(value, ">") {
			return "", errors.New("malformed angle brackets")
		}
		value = "<" + value + ">"
	}
	return validBareMessageID(value[1 : len(value)-1])
}

func validBareMessageID(inner string) (string, error) {
	if inner == "" || strings.ContainsAny(inner, "\r\n<> \t()") || !utf8.ValidString(inner) {
		return "", errors.New("malformed Message-ID")
	}
	return inner, nil
}

// parseMessageIDHeader returns every message ID in a Message-ID, In-Reply-To,
// or References header value, bare and in order. RFC 5322 comments in
// parentheses are skipped, including nested and escaped ones. A bare token
// without angle brackets is accepted as one ID, as older mail software emits
// them. Any other text is an error.
func parseMessageIDHeader(value string) ([]string, error) {
	var ids []string
	depth := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case depth > 0:
			switch c {
			case '\\':
				i++
			case '(':
				depth++
			case ')':
				depth--
			}
		case c == '(':
			depth++
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
		case c == '<':
			end := strings.IndexByte(value[i:], '>')
			if end < 0 {
				return nil, errors.New("unterminated angle bracket")
			}
			id, err := validBareMessageID(value[i+1 : i+end])
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
			i += end
		case c == ')' || c == '>':
			return nil, errors.New("malformed angle brackets")
		default:
			end := strings.IndexAny(value[i:], " \t\r\n(<")
			if end < 0 {
				end = len(value) - i
			}
			id, err := validBareMessageID(value[i : i+end])
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
			i += end - 1
		}
	}
	if depth != 0 {
		return nil, errors.New("unterminated comment")
	}
	return ids, nil
}

func normalizeReplySubject(subject string) string {
	subject = strings.TrimSpace(subject)
	for len(subject) >= 3 && strings.EqualFold(subject[:3], "re:") {
		subject = strings.TrimSpace(subject[3:])
	}
	if subject == "" {
		return "Re:"
	}
	return "Re: " + subject
}

func isASCII(value string) bool {
	for i := range len(value) {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func formatAddresses(addresses []*mail.Address) string {
	values := make([]string, len(addresses))
	for i, address := range addresses {
		values[i] = address.String()
	}
	return strings.Join(values, ", ")
}

func formatMessageIDs(ids []string) string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = "<" + id + ">"
	}
	return strings.Join(values, " ")
}

// ReplaceDraftBody recomposes an existing draft's RFC822 bytes with a new body.
// It preserves From, To, Subject, In-Reply-To, and References headers from the
// existing draft, generates a new Message-ID, and replaces the text/plain body.
// The raw argument must be a single-part text/plain message (no attachments).
// body=="" is valid (an empty body); not providing body at all is a caller error.
func ReplaceDraftBody(raw []byte, body string, now time.Time) (ReplyDraft, error) {
	if !utf8.ValidString(body) || strings.ContainsAny(body, "\x00") {
		return ReplyDraft{}, errors.New("invalid reply body")
	}
	existing, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse existing draft: %w", err)
	}
	if err := validateSingletonHeaders(existing.Header); err != nil {
		return ReplyDraft{}, err
	}
	// Reject attachments and unsupported MIME structures.
	contentType := existing.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := stdmime.ParseMediaType(contentType)
		if err != nil {
			return ReplyDraft{}, errors.New("invalid_message")
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			return ReplyDraft{}, errors.New("invalid_message")
		}
		if mediaType != "" && mediaType != "text/plain" {
			return ReplyDraft{}, errors.New("invalid_message")
		}
	}
	// Collect preserved headers.
	fromValue := existing.Header.Get("From")
	if fromValue == "" {
		return ReplyDraft{}, errors.New("existing draft has no From header")
	}
	toValue := existing.Header.Get("To")
	subject := existing.Header.Get("Subject")
	inReplyTo := existing.Header.Get("In-Reply-To")
	references := existing.Header.Get("References")

	newMessageID, err := newReplyMessageID()
	if err != nil {
		return ReplyDraft{}, err
	}

	var out bytes.Buffer
	writeHeader := func(name, value string) {
		if value != "" {
			_, _ = fmt.Fprintf(&out, "%s: %s\r\n", name, value)
		}
	}
	writeHeader("Date", now.UTC().Format(time.RFC1123Z))
	writeHeader("From", fromValue)
	if toValue != "" {
		writeHeader("To", toValue)
	}
	if subject != "" {
		writeHeader("Subject", subject)
	}
	writeHeader("Message-ID", "<"+newMessageID+">")
	if inReplyTo != "" {
		writeHeader("In-Reply-To", inReplyTo)
	}
	if references != "" {
		writeFoldedHeader(&out, "References", references)
	}
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	out.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	out.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&out)
	if _, err := io.WriteString(qp, body); err != nil {
		return ReplyDraft{}, fmt.Errorf("encode draft body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return ReplyDraft{}, fmt.Errorf("close draft body encoder: %w", err)
	}

	parsed, err := msgmime.Parse(out.Bytes())
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse composed draft: %w", err)
	}
	return ReplyDraft{Raw: out.Bytes(), Parsed: parsed}, nil
}

func writeFoldedHeader(raw *bytes.Buffer, name, value string) {
	const width = 78
	line := name + ": "
	for token := range strings.FieldsSeq(value) {
		if len(line)+len(token)+1 > width && line != name+": " {
			raw.WriteString(line + "\r\n")
			line = " "
		}
		if line != " " && !strings.HasSuffix(line, " ") {
			line += " "
		}
		line += token
	}
	raw.WriteString(line + "\r\n")
}
