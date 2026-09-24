package imap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	stdmime "mime"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// ReplyDraft is the composed RFC822 message and its parsed representation.
type ReplyDraft struct {
	Raw    []byte
	Parsed *msgmime.Message
}

// ReplyOptions selects the recipients for a reply.
type ReplyOptions struct {
	ReplyAll      bool
	SelfAddresses []string
}

// ComposeOptions describes a new plain-text message.
type ComposeOptions struct {
	From    string
	To      []string
	Cc      []string
	Bcc     []string
	Subject string
	Body    string
}

type plainTextEnvelope struct {
	From, To, Cc, Bcc, ReplyTo []*mail.Address
	Subject                    string
	MessageID                  string
	InReplyTo, References      []string
}

// BuildReply composes a single plain-text reply from an archived message.
func BuildReply(parentRaw []byte, from, body string, now time.Time, messageID string) (ReplyDraft, error) {
	return BuildReplyWithOptions(parentRaw, from, body, ReplyOptions{}, now, messageID)
}

// BuildReplyWithOptions composes a plain-text reply with the requested
// recipient policy.
func BuildReplyWithOptions(parentRaw []byte, from, body string, options ReplyOptions, now time.Time, messageID string) (ReplyDraft, error) {
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
	to, cc, err := replyRecipients(parent.Header, fromAddress, options)
	if err != nil {
		return ReplyDraft{}, err
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

	return renderPlainTextDraft(plainTextEnvelope{
		From: []*mail.Address{fromAddress}, To: to, Cc: cc,
		Subject:   normalizeReplySubject(decodeHeader(parent.Header.Get("Subject"))),
		MessageID: messageID, InReplyTo: messageIDList(parentID), References: references,
	}, body, now)
}

func messageIDList(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

func replyRecipients(header mail.Header, from *mail.Address, options ReplyOptions) (to, cc []*mail.Address, err error) {
	replyTo := header.Get("Reply-To")
	if replyTo == "" {
		replyTo = header.Get("From")
	}
	primary, parseErr := mail.ParseAddressList(replyTo)
	if parseErr != nil || len(primary) == 0 {
		return nil, nil, errors.New("parent has invalid From or Reply-To")
	}
	for _, address := range primary {
		if err := validateAddress(address); err != nil {
			return nil, nil, fmt.Errorf("parent has invalid reply address: %w", err)
		}
	}
	if !options.ReplyAll {
		return primary, nil, nil
	}

	parentTo, err := parseDraftHeaderAddresses(header, "To", false)
	if err != nil {
		return nil, nil, err
	}
	parentCc, err := parseDraftHeaderAddresses(header, "Cc", false)
	if err != nil {
		return nil, nil, err
	}
	excluded := make(map[string]struct{}, len(options.SelfAddresses)+1)
	excluded[store.NormalizeIdentifierForCompare(from.Address)] = struct{}{}
	for _, address := range options.SelfAddresses {
		parsed, parseErr := mail.ParseAddress(strings.TrimSpace(address))
		key := strings.TrimSpace(address)
		if parseErr == nil && parsed != nil {
			key = parsed.Address
		}
		if key != "" {
			excluded[store.NormalizeIdentifierForCompare(key)] = struct{}{}
		}
	}

	seen := make(map[string]struct{}, len(primary)+len(parentTo)+len(parentCc))
	appendVisible := func(dst []*mail.Address, addresses []*mail.Address) []*mail.Address {
		for _, address := range addresses {
			key := store.NormalizeIdentifierForCompare(address.Address)
			if _, ok := excluded[key]; ok {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			dst = append(dst, address)
		}
		return dst
	}
	to = appendVisible(to, primary)
	to = appendVisible(to, parentTo)
	cc = appendVisible(cc, parentCc)
	if len(to) == 0 && len(cc) == 0 {
		return nil, nil, errors.New("reply-all has no recipients outside the sending account")
	}
	return to, cc, nil
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

// BuildDraftReplacement keeps the envelope and thread headers of a plain-text
// draft while replacing its body and assigning a new Date and Message-ID.
func BuildDraftReplacement(currentRaw []byte, body string, now time.Time, messageID string) (ReplyDraft, error) {
	if len(currentRaw) == 0 {
		return ReplyDraft{}, errors.New("draft message is empty")
	}
	if !utf8.ValidString(body) || strings.ContainsAny(body, "\x00") {
		return ReplyDraft{}, errors.New("invalid draft body")
	}
	message, err := mail.ReadMessage(bytes.NewReader(currentRaw))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("read draft headers: %w", err)
	}
	if err := validateSingletonHeaders(message.Header); err != nil {
		return ReplyDraft{}, err
	}
	contentType := message.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := stdmime.ParseMediaType(contentType)
		if err != nil {
			return ReplyDraft{}, fmt.Errorf("invalid draft Content-Type: %w", err)
		}
		if !strings.EqualFold(mediaType, "text/plain") {
			return ReplyDraft{}, errors.New("draft replacement requires a plain-text message")
		}
	}
	parsedCurrent, err := msgmime.Parse(currentRaw)
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse draft MIME: %w", err)
	}
	if len(parsedCurrent.Attachments) != 0 || parsedCurrent.BodyHTML != "" {
		return ReplyDraft{}, errors.New("draft replacement does not support multipart or attachments")
	}

	from, err := parseDraftHeaderAddresses(message.Header, "From", true)
	if err != nil {
		return ReplyDraft{}, err
	}
	to, err := parseDraftHeaderAddresses(message.Header, "To", false)
	if err != nil {
		return ReplyDraft{}, err
	}
	cc, err := parseDraftHeaderAddresses(message.Header, "Cc", false)
	if err != nil {
		return ReplyDraft{}, err
	}
	bcc, err := parseDraftHeaderAddresses(message.Header, "Bcc", false)
	if err != nil {
		return ReplyDraft{}, err
	}
	replyTo, err := parseDraftHeaderAddresses(message.Header, "Reply-To", false)
	if err != nil {
		return ReplyDraft{}, err
	}
	if len(from) != 1 || len(to)+len(cc)+len(bcc) == 0 {
		return ReplyDraft{}, errors.New("draft must contain one From and at least one recipient")
	}
	subject := decodeHeader(message.Header.Get("Subject"))
	if !validHeaderValue(subject) {
		return ReplyDraft{}, errors.New("invalid draft Subject header")
	}
	inReplyTo, err := parseMessageIDHeader(message.Header.Get("In-Reply-To"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid draft In-Reply-To: %w", err)
	}
	references, err := parseMessageIDHeader(message.Header.Get("References"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid draft References: %w", err)
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

	return renderPlainTextDraft(plainTextEnvelope{
		From: from, To: to, Cc: cc, Bcc: bcc, ReplyTo: replyTo,
		Subject: subject, MessageID: messageID,
		InReplyTo: inReplyTo, References: references,
	}, body, now)
}

func parseDraftHeaderAddresses(header mail.Header, name string, required bool) ([]*mail.Address, error) {
	values := headerValues(header, name)
	if len(values) == 0 {
		if required {
			return nil, fmt.Errorf("draft is missing %s header", name)
		}
		return nil, nil
	}
	addresses, err := mail.ParseAddressList(strings.Join(values, ", "))
	if err != nil {
		return nil, fmt.Errorf("invalid draft %s header: %w", name, err)
	}
	for _, address := range addresses {
		if err := validateAddress(address); err != nil {
			return nil, fmt.Errorf("invalid draft %s address: %w", name, err)
		}
	}
	return addresses, nil
}
