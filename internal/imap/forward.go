package imap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	msgmime "go.kenn.io/msgvault/internal/mime"
)

const (
	forwardMarkerHeader = "X-Msgvault-Forward"
	forwardMarkerValue  = "1"
)

// ForwardAttachment is one already verified archived attachment occurrence.
// Content is read by the daemon before BuildForward is called.
type ForwardAttachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Disposition string
	IsInline    bool
	Content     []byte
}

// ForwardOptions contains the selected envelope and the archived message
// projection to put in an IMAP draft.
type ForwardOptions struct {
	From         string
	To           []string
	Cc           []string
	Bcc          []string
	Subject      string
	Body         string
	QuotedHeader string
	QuotedText   string
	Attachments  []ForwardAttachment
}

// BuildForward creates the one MIME layout draft-forward uses: a
// multipart/mixed message whose first text/plain part is the editable note,
// followed by the quoted text and each archived attachment.
func BuildForward(options ForwardOptions, now time.Time, messageID string) (ReplyDraft, error) {
	from, to, cc, bcc, subject, err := validateForwardOptions(options)
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
	headerSummary := options.QuotedHeader
	if strings.ContainsAny(headerSummary, "\x00") || !utf8.ValidString(headerSummary) {
		return ReplyDraft{}, errors.New("invalid forwarded header summary")
	}
	for _, attachment := range options.Attachments {
		if err := ValidateForwardAttachment(attachment); err != nil {
			return ReplyDraft{}, err
		}
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writeTextPart(writer, options.Body); err != nil {
		return ReplyDraft{}, err
	}
	if err := writeTextPart(writer, quotedTextWithHeader(headerSummary, options.QuotedText)); err != nil {
		return ReplyDraft{}, err
	}
	for _, attachment := range options.Attachments {
		if err := writeBinaryPart(writer, attachment); err != nil {
			return ReplyDraft{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return ReplyDraft{}, fmt.Errorf("close forwarded MIME: %w", err)
	}

	var raw bytes.Buffer
	writeForwardHeaders(&raw, from, to, cc, bcc, subject, messageID, now, writer.Boundary())
	raw.Write(body.Bytes())
	parsed, err := msgmime.Parse(raw.Bytes())
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse forwarded draft: %w", err)
	}
	if err := validateForwardParsedAttachments(parsed, options.Attachments); err != nil {
		return ReplyDraft{}, err
	}
	return ReplyDraft{Raw: raw.Bytes(), Parsed: parsed}, nil
}

func validateForwardOptions(options ForwardOptions) (*mail.Address, []*mail.Address, []*mail.Address, []*mail.Address, string, error) {
	if !utf8.ValidString(options.From) || strings.ContainsAny(options.From, "\r\n") {
		return nil, nil, nil, nil, "", errors.New("invalid forward From address")
	}
	from, err := parseOneAddress(options.From)
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("invalid forward From address: %w", err)
	}
	if !utf8.ValidString(options.Body) || strings.ContainsAny(options.Body, "\x00") {
		return nil, nil, nil, nil, "", errors.New("invalid forward body")
	}
	to, err := parseComposeAddresses("To", options.To)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	cc, err := parseComposeAddresses("Cc", options.Cc)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	bcc, err := parseComposeAddresses("Bcc", options.Bcc)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return nil, nil, nil, nil, "", errors.New("forward requires at least one recipient")
	}
	if !validHeaderValue(options.Subject) {
		return nil, nil, nil, nil, "", errors.New("invalid forward Subject header")
	}
	return from, to, cc, bcc, normalizeForwardSubject(options.Subject), nil
}

func normalizeForwardSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	for {
		prefix, rest, found := strings.Cut(subject, ":")
		if !found || (!strings.EqualFold(prefix, "fw") && !strings.EqualFold(prefix, "fwd")) {
			break
		}
		subject = strings.TrimSpace(rest)
	}
	if subject == "" {
		return "Fwd:"
	}
	return "Fwd: " + subject
}

// ValidateForwardAttachment checks whether an archived part can be forwarded
// without changing its bytes over the client's non-binary IMAP APPEND path.
func ValidateForwardAttachment(attachment ForwardAttachment) error {
	if !utf8.ValidString(attachment.Filename) || strings.ContainsAny(attachment.Filename, "\x00\r\n") {
		return errors.New("invalid forwarded attachment filename")
	}
	if !utf8.ValidString(attachment.ContentType) || strings.ContainsAny(attachment.ContentType, "\x00\r\n") {
		return errors.New("invalid forwarded attachment media type")
	}
	if attachment.ContentID != "" && (!utf8.ValidString(attachment.ContentID) || strings.ContainsAny(attachment.ContentID, "\x00\r\n")) {
		return errors.New("invalid forwarded attachment Content-ID")
	}
	if attachment.Disposition != "" && !validHeaderValue(attachment.Disposition) {
		return errors.New("invalid forwarded attachment disposition")
	}
	if attachment.Disposition != "" {
		if _, _, err := mime.ParseMediaType(attachment.Disposition); err != nil {
			return fmt.Errorf("invalid forwarded attachment disposition: %w", err)
		}
	}
	if attachment.ContentType != "" {
		mediaType, _, err := mime.ParseMediaType(attachment.ContentType)
		if err != nil {
			return fmt.Errorf("invalid forwarded attachment media type: %w", err)
		}
		if mediaType == "message/rfc822" {
			// RFC 2046 forbids base64 for attached messages. Preserve their
			// bytes as 8bit; APPEND does not support binary literals.
			for line := range bytes.SplitSeq(attachment.Content, []byte("\r\n")) {
				if len(line) > 998 || bytes.ContainsAny(line, "\x00\r\n") {
					return errors.New("attached message requires unsupported binary transport")
				}
			}
		}
	}
	return nil
}

func writeForwardHeaders(raw *bytes.Buffer, from *mail.Address, to, cc, bcc []*mail.Address, subject, messageID string, now time.Time, boundary string) {
	write := func(name, value string) {
		_, _ = fmt.Fprintf(raw, "%s: %s\r\n", name, value)
	}
	write("Date", now.UTC().Format(time.RFC1123Z))
	write("From", from.String())
	if value := formatAddresses(to); value != "" {
		write("To", value)
	}
	if value := formatAddresses(cc); value != "" {
		write("Cc", value)
	}
	if value := formatAddresses(bcc); value != "" {
		write("Bcc", value)
	}
	if !isASCII(subject) {
		subject = mime.QEncoding.Encode("UTF-8", subject)
	}
	write("Subject", subject)
	write("Message-ID", "<"+messageID+">")
	write(forwardMarkerHeader, forwardMarkerValue)
	write("MIME-Version", "1.0")
	write("Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	write("Content-Transfer-Encoding", "8bit")
	raw.WriteString("\r\n")
}

func writeTextPart(writer *multipart.Writer, value string) error {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Set("Content-Disposition", "inline")
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create text part: %w", err)
	}
	return writeQuotedPrintable(part, value)
}

func writeQuotedPrintable(dst io.Writer, value string) error {
	qp := quotedprintable.NewWriter(dst)
	if _, err := qp.Write([]byte(value)); err != nil {
		return fmt.Errorf("write text part: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("close text part: %w", err)
	}
	return nil
}

func writeBinaryPart(writer *multipart.Writer, attachment ForwardAttachment) error {
	header := textproto.MIMEHeader{}
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	params := make(map[string]string)
	if attachment.Filename != "" {
		params["name"] = attachment.Filename
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if strings.HasPrefix(mediaType, "text/") {
		params["charset"] = "utf-8" // the archive stores text parts decoded to UTF-8
	}
	header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	disposition := strings.ToLower(strings.TrimSpace(attachment.Disposition))
	if disposition == "" {
		if attachment.IsInline || attachment.ContentID != "" {
			disposition = "inline"
		} else {
			disposition = "attachment"
		}
	} else if disposition != "inline" {
		disposition = "attachment" // RFC 2183: unknown dispositions are attachments.
	}
	dispositionParams := make(map[string]string)
	if attachment.Filename != "" {
		dispositionParams["filename"] = attachment.Filename
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, dispositionParams))
	if attachment.ContentID != "" {
		header.Set("Content-ID", "<"+strings.Trim(attachment.ContentID, "<>")+">")
	}
	header.Set("Content-Transfer-Encoding", "base64")
	if mediaType == "message/rfc822" {
		header.Set("Content-Transfer-Encoding", "8bit")
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create forwarded attachment: %w", err)
	}
	if mediaType == "message/rfc822" {
		_, err := part.Write(attachment.Content)
		return err
	}
	return writeBase64MIME(part, attachment.Content)
}

func writeBase64MIME(dst io.Writer, content []byte) error {
	var line [76]byte
	for len(content) > 0 {
		chunk := min(len(content), 57)
		base64.StdEncoding.Encode(line[:], content[:chunk])
		encoded := line[:base64.StdEncoding.EncodedLen(chunk)]
		written, err := dst.Write(encoded)
		if err != nil {
			return err
		}
		if written != len(encoded) {
			return io.ErrShortWrite
		}
		content = content[chunk:]
		if len(content) > 0 {
			written, err = dst.Write([]byte("\r\n"))
			if err != nil {
				return err
			}
			if written != 2 {
				return io.ErrShortWrite
			}
		}
	}
	return nil
}

func quotedTextWithHeader(headerSummary, text string) string {
	const separator = "---------- Forwarded message ----------"
	if headerSummary = strings.TrimSpace(headerSummary); headerSummary == "" {
		headerSummary = separator
	} else {
		headerSummary = separator + "\r\n" + headerSummary
	}
	if text == "" {
		return headerSummary + "\r\n"
	}
	return headerSummary + "\r\n\r\n" + text
}

func validateForwardParsedAttachments(parsed *msgmime.Message, expected []ForwardAttachment) error {
	parts := parsed.Attachments
	if len(parts) != len(expected) {
		return fmt.Errorf("forwarded MIME attachment count mismatch: emitted %d, expected %d", len(parts), len(expected))
	}
	used := make([]bool, len(expected))
	for i, part := range parts {
		match := -1
		for j, want := range expected {
			if used[j] || part.Filename != want.Filename || part.ContentID != strings.Trim(want.ContentID, "<>") || !bytes.Equal(part.Content, want.Content) {
				continue
			}
			match = j
			break
		}
		if match < 0 {
			return fmt.Errorf("forwarded MIME attachment %d does not match archived occurrence", i)
		}
		used[match] = true
	}
	return nil
}

// IsGeneratedForward reports whether raw carries draft-forward's marker.
func IsGeneratedForward(raw []byte) bool {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	return err == nil && strings.TrimSpace(message.Header.Get(forwardMarkerHeader)) == forwardMarkerValue
}

// BuildIMAPDraftReplacement updates the editable note of a generated forward.
// Plain drafts keep the existing replacement contract; a multipart message
// must carry the forward marker.
func BuildIMAPDraftReplacement(currentRaw []byte, body string, now time.Time, messageID string) (ReplyDraft, error) {
	message, err := mail.ReadMessage(bytes.NewReader(currentRaw))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("read draft headers: %w", err)
	}
	contentType := message.Header.Get("Content-Type")
	if contentType == "" {
		return BuildDraftReplacement(currentRaw, body, now, messageID)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid draft Content-Type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return BuildDraftReplacement(currentRaw, body, now, messageID)
	}
	if !strings.EqualFold(mediaType, "multipart/mixed") || message.Header.Get(forwardMarkerHeader) != forwardMarkerValue || params["boundary"] == "" {
		return ReplyDraft{}, errors.New("draft replacement does not support this multipart message")
	}
	if !utf8.ValidString(body) || strings.ContainsAny(body, "\x00") {
		return ReplyDraft{}, errors.New("invalid draft body")
	}
	newBody, boundary, err := rewriteForwardMultipart(message.Body, params["boundary"], body)
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
	if len(from) != 1 || len(to)+len(cc)+len(bcc) == 0 {
		return ReplyDraft{}, errors.New("forward draft must contain one From and at least one recipient")
	}
	subject := decodeHeader(message.Header.Get("Subject"))
	if !validHeaderValue(subject) {
		return ReplyDraft{}, errors.New("invalid draft Subject header")
	}
	var raw bytes.Buffer
	writeForwardHeaders(&raw, from[0], to, cc, bcc, subject, messageID, now, boundary)
	raw.Write(newBody)
	parsed, err := msgmime.Parse(raw.Bytes())
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse forwarded replacement: %w", err)
	}
	return ReplyDraft{Raw: raw.Bytes(), Parsed: parsed}, nil
}

// rewriteForwardMultipart replaces the first (note) part and copies every
// later part byte for byte.
func rewriteForwardMultipart(body io.Reader, boundary, note string) ([]byte, string, error) {
	reader := multipart.NewReader(body, boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	for index := 0; ; index++ {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) && index > 0 {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read forward draft part: %w", err)
		}
		created, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, "", fmt.Errorf("create forward draft part: %w", err)
		}
		if index == 0 {
			mediaType, _, typeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if typeErr != nil || !strings.EqualFold(mediaType, "text/plain") ||
				!strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
				return nil, "", errors.New("forward draft has an invalid editable note part")
			}
			err = writeQuotedPrintable(created, note)
		} else {
			_, err = io.Copy(created, part)
		}
		if err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close forwarded MIME: %w", err)
	}
	return out.Bytes(), writer.Boundary(), nil
}
