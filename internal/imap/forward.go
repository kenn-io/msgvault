package imap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	msgmime "go.kenn.io/msgvault/internal/mime"
)

const (
	forwardMarkerHeader  = "X-Msgvault-Forward"
	forwardMarkerValue   = "1"
	forwardHTMLStart     = "<!-- msgvault-forward-note-start -->"
	forwardHTMLEnd       = "<!-- msgvault-forward-note-end -->"
	forwardHTMLNoteOpen  = `<div class="msgvault-forward-note">`
	forwardHTMLNoteClose = `</div>`
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
	From          string
	To            []string
	Cc            []string
	Bcc           []string
	Subject       string
	Body          string
	QuotedHeader  string
	QuotedText    string
	QuotedHTML    string
	Attachments   []ForwardAttachment
	OriginalText  string
	OriginalHTML  string
	HeaderSummary string
}

// BuildForward creates the multipart MIME shape used by draft-forward. The
// first text/plain part is the editable note; all later parts are quoted or
// retained archived content.
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
	quotedText := options.QuotedText
	if quotedText == "" {
		quotedText = options.OriginalText
	}
	quotedHTML := options.QuotedHTML
	if quotedHTML == "" {
		quotedHTML = options.OriginalHTML
	}
	headerSummary := options.QuotedHeader
	if headerSummary == "" {
		headerSummary = options.HeaderSummary
	}
	if strings.ContainsAny(headerSummary, "\x00") || !utf8.ValidString(headerSummary) {
		return ReplyDraft{}, errors.New("invalid forwarded header summary")
	}
	if strings.Contains(options.Body, forwardHTMLStart) || strings.Contains(options.Body, forwardHTMLEnd) ||
		strings.Contains(quotedHTML, forwardHTMLStart) || strings.Contains(quotedHTML, forwardHTMLEnd) {
		return ReplyDraft{}, errors.New("forward note markers are reserved")
	}
	for _, attachment := range options.Attachments {
		if err := validateForwardAttachment(attachment); err != nil {
			return ReplyDraft{}, err
		}
	}
	if err := validateForwardCIDReferences(quotedHTML, options.Attachments); err != nil {
		return ReplyDraft{}, err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writeForwardNote(writer, options.Body); err != nil {
		return ReplyDraft{}, err
	}
	if quotedHTML != "" {
		if err := writeQuotedRelated(writer, quotedText, quotedHTML, headerSummary, options.Attachments, options.Body); err != nil {
			return ReplyDraft{}, err
		}
	} else {
		if err := writeTextPart(writer, "text/plain; charset=utf-8", quotedTextWithHeader(headerSummary, quotedText), "quoted-printable", nil); err != nil {
			return ReplyDraft{}, err
		}
	}
	for _, attachment := range options.Attachments {
		if attachment.IsInline || attachment.ContentID != "" {
			continue
		}
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
	for len(subject) >= 4 && strings.EqualFold(subject[:4], "fwd:") {
		subject = strings.TrimSpace(subject[4:])
	}
	if subject == "" {
		return "Fwd:"
	}
	return "Fwd: " + subject
}

func validateForwardAttachment(attachment ForwardAttachment) error {
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
	if disposition := strings.ToLower(strings.TrimSpace(attachment.Disposition)); disposition != "" &&
		disposition != "inline" && disposition != "attachment" {
		return fmt.Errorf("unsupported forwarded attachment disposition %q", attachment.Disposition)
	}
	if attachment.ContentType != "" {
		if _, _, err := mime.ParseMediaType(attachment.ContentType); err != nil {
			return fmt.Errorf("invalid forwarded attachment media type: %w", err)
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
	raw.WriteString("\r\n")
}

func writeForwardNote(writer *multipart.Writer, note string) error {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", `text/plain; charset="utf-8"`)
	header.Set("Content-Disposition", "inline")
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("create forward note: %w", err)
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(note)); err != nil {
		return fmt.Errorf("write forward note: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("close forward note: %w", err)
	}
	return nil
}

func writeTextPart(writer *multipart.Writer, contentType, value, encoding string, extra map[string]string) error {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", contentType)
	header.Set("Content-Disposition", "inline")
	header.Set("Content-Transfer-Encoding", encoding)
	for key, item := range extra {
		header.Set(key, item)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	qp := quotedprintable.NewWriter(part)
	_, err = qp.Write([]byte(value))
	if err != nil {
		return err
	}
	return qp.Close()
}

func writeQuotedRelated(writer *multipart.Writer, quotedText, quotedHTML, headerSummary string, attachments []ForwardAttachment, note string) error {
	var nested bytes.Buffer
	nestedWriter := multipart.NewWriter(&nested)
	if err := writeTextPart(nestedWriter, "text/plain; charset=utf-8", quotedTextWithHeader(headerSummary, quotedText), "quoted-printable", nil); err != nil {
		return err
	}
	htmlBody := forwardHTMLBody(headerSummary, quotedHTML, note)
	if err := writeTextPart(nestedWriter, "text/html; charset=utf-8", htmlBody, "quoted-printable", nil); err != nil {
		return err
	}
	for _, attachment := range attachments {
		if !attachment.IsInline && attachment.ContentID == "" {
			continue
		}
		if err := writeBinaryPart(nestedWriter, attachment); err != nil {
			return err
		}
	}
	if err := nestedWriter.Close(); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", `multipart/related; boundary="`+nestedWriter.Boundary()+`"`)
	header.Set("Content-Disposition", "inline")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(nested.Bytes())
	return err
}

func writeBinaryPart(writer *multipart.Writer, attachment ForwardAttachment) error {
	header := textproto.MIMEHeader{}
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	params := map[string]string{"name": attachment.Filename}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	disposition := attachment.Disposition
	if disposition == "" {
		if attachment.IsInline || attachment.ContentID != "" {
			disposition = "inline"
		} else {
			disposition = "attachment"
		}
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": attachment.Filename}))
	if attachment.ContentID != "" {
		header.Set("Content-ID", "<"+strings.Trim(attachment.ContentID, "<>")+">")
	}
	header.Set("Content-Transfer-Encoding", "base64")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, part)
	if _, err := encoder.Write(attachment.Content); err != nil {
		return err
	}
	return encoder.Close()
}

func quotedTextWithHeader(headerSummary, text string) string {
	headerSummary = strings.TrimSpace(headerSummary)
	if headerSummary == "" {
		return text
	}
	if text == "" {
		return headerSummary + "\r\n"
	}
	return headerSummary + "\r\n\r\n" + text
}

func forwardHTMLBody(headerSummary, original, note string) string {
	var out strings.Builder
	out.WriteString(forwardHTMLStart)
	out.WriteString(forwardHTMLNoteOpen)
	out.WriteString(html.EscapeString(note))
	out.WriteString(forwardHTMLNoteClose)
	out.WriteString(forwardHTMLEnd)
	if headerSummary != "" {
		out.WriteString("<div class=\"msgvault-forward-header\">")
		out.WriteString(html.EscapeString(headerSummary))
		out.WriteString("</div>")
	}
	out.WriteString(original)
	return out.String()
}

func validateForwardParsedAttachments(parsed *msgmime.Message, expected []ForwardAttachment) error {
	if len(parsed.Attachments) != len(expected) {
		return fmt.Errorf("forwarded MIME attachment count mismatch: emitted %d, expected %d", len(parsed.Attachments), len(expected))
	}
	used := make([]bool, len(expected))
	for i, part := range parsed.Attachments {
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

func validateForwardCIDReferences(htmlBody string, attachments []ForwardAttachment) error {
	if htmlBody == "" {
		return nil
	}
	counts := make(map[string]int, len(attachments))
	for _, attachment := range attachments {
		contentID := strings.Trim(attachment.ContentID, "<>")
		if contentID != "" {
			counts[contentID]++
		}
	}
	lower := strings.ToLower(htmlBody)
	for offset := 0; ; {
		index := strings.Index(lower[offset:], "cid:")
		if index < 0 {
			return nil
		}
		start := offset + index + len("cid:")
		end := start
		for end < len(htmlBody) && !strings.ContainsRune("\t\r\n \"'<>)]", rune(htmlBody[end])) {
			end++
		}
		contentID := htmlBody[start:end]
		if contentID == "" {
			return errors.New("forwarded HTML contains an empty Content-ID reference")
		}
		if counts[contentID] == 0 {
			if decoded, decodeErr := url.PathUnescape(contentID); decodeErr == nil {
				contentID = decoded
			}
		}
		if counts[contentID] != 1 {
			return fmt.Errorf("forwarded HTML Content-ID %q is missing or ambiguous", contentID)
		}
		offset = end
	}
}

// BuildIMAPDraftReplacement updates the editable note of a generated forward.
// Plain drafts keep the existing replacement contract; all other multipart
// messages must carry the exact forward marker and shape.
func BuildIMAPDraftReplacement(currentRaw []byte, body string, now time.Time, messageID string) (ReplyDraft, error) {
	message, err := mail.ReadMessage(bytes.NewReader(currentRaw))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("read draft headers: %w", err)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid draft Content-Type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return BuildDraftReplacement(currentRaw, body, now, messageID)
	}
	if message.Header.Get(forwardMarkerHeader) != forwardMarkerValue || params["boundary"] == "" {
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

func rewriteForwardMultipart(body io.Reader, boundary, note string) ([]byte, string, error) {
	reader := multipart.NewReader(body, boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	first, err := reader.NextRawPart()
	if err != nil {
		return nil, "", errors.New("forward draft is missing its editable note part")
	}
	firstType, _, firstTypeErr := mime.ParseMediaType(first.Header.Get("Content-Type"))
	if firstTypeErr != nil || !strings.EqualFold(firstType, "text/plain") {
		return nil, "", errors.New("forward draft has an invalid editable note part")
	}
	header := cloneMIMEHeader(first.Header)
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	qp := quotedprintable.NewWriter(part)
	if _, err := qp.Write([]byte(note)); err != nil {
		return nil, "", err
	}
	if err := qp.Close(); err != nil {
		return nil, "", err
	}
	second, err := reader.NextRawPart()
	if err != nil {
		return nil, "", errors.New("forward draft is missing its quoted content")
	}
	secondHeader := cloneMIMEHeader(second.Header)
	secondType, secondParams, typeErr := mime.ParseMediaType(secondHeader.Get("Content-Type"))
	secondBody, err := io.ReadAll(second)
	if err != nil {
		return nil, "", fmt.Errorf("read quoted forward content: %w", err)
	}
	if typeErr == nil && strings.EqualFold(secondType, "multipart/related") && secondParams["boundary"] != "" {
		secondBody, secondParams["boundary"], err = rewriteForwardRelated(secondBody, secondParams["boundary"], note)
		if err != nil {
			return nil, "", err
		}
		secondHeader.Set("Content-Type", mime.FormatMediaType(secondType, secondParams))
	} else if typeErr != nil || !strings.EqualFold(secondType, "text/plain") {
		return nil, "", errors.New("forward draft has unsupported quoted content")
	}
	if err := validateForwardBodyPart(secondHeader, false); err != nil {
		return nil, "", err
	}
	part, err = writer.CreatePart(secondHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(secondBody); err != nil {
		return nil, "", err
	}
	for {
		next, nextErr := reader.NextRawPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, "", fmt.Errorf("read forwarded attachment: %w", nextErr)
		}
		nextBody, readErr := io.ReadAll(next)
		if readErr != nil {
			return nil, "", readErr
		}
		if err := validateForwardBodyPart(next.Header, true); err != nil {
			return nil, "", err
		}
		part, err = writer.CreatePart(cloneMIMEHeader(next.Header))
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(nextBody); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), writer.Boundary(), nil
}

func rewriteForwardRelated(body []byte, boundary, note string) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	plainParts, htmlParts := 0, 0
	partIndex := 0
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		partBody, err := io.ReadAll(part)
		if err != nil {
			return nil, "", err
		}
		header := cloneMIMEHeader(part.Header)
		mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
		if strings.EqualFold(mediaType, "text/plain") {
			if partIndex != 0 {
				return nil, "", errors.New("forward draft related text part is out of order")
			}
			plainParts++
		}
		if strings.EqualFold(mediaType, "text/html") {
			if partIndex != 1 {
				return nil, "", errors.New("forward draft related HTML part is out of order")
			}
			htmlParts++
			decoded, decodeErr := decodeTransfer(partBody, header.Get("Content-Transfer-Encoding"))
			if decodeErr != nil {
				return nil, "", fmt.Errorf("decode forward HTML note: %w", decodeErr)
			}
			decoded, replaceErr := replaceForwardHTMLNote(decoded, note)
			if replaceErr != nil {
				return nil, "", replaceErr
			}
			var encodeErr error
			partBody, encodeErr = encodeTransfer(decoded, header.Get("Content-Transfer-Encoding"))
			if encodeErr != nil {
				return nil, "", encodeErr
			}
		} else if partIndex >= 2 {
			if err := validateForwardBodyPart(header, true); err != nil {
				return nil, "", err
			}
		}
		created, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := created.Write(partBody); err != nil {
			return nil, "", err
		}
		partIndex++
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	if plainParts != 1 || htmlParts != 1 {
		return nil, "", errors.New("forward draft related content is incomplete")
	}
	return out.Bytes(), writer.Boundary(), nil
}

func validateForwardBodyPart(header textproto.MIMEHeader, attachment bool) error {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType == "" {
		return errors.New("forward draft contains unsupported multipart content")
	}
	if !attachment && !strings.EqualFold(mediaType, "text/plain") && !strings.EqualFold(mediaType, "multipart/related") {
		return errors.New("forward draft contains unsupported quoted content")
	}
	if attachment && strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return errors.New("forward draft contains unsupported multipart content")
	}
	if attachment {
		disposition, _, dispositionErr := mime.ParseMediaType(header.Get("Content-Disposition"))
		if dispositionErr != nil || (disposition != "inline" && disposition != "attachment") {
			return errors.New("forward draft contains an invalid attachment part")
		}
	}
	return nil
}

func replaceForwardHTMLNote(htmlBody []byte, note string) ([]byte, error) {
	value := string(htmlBody)
	start := strings.Count(value, forwardHTMLStart)
	end := strings.Count(value, forwardHTMLEnd)
	if start != 1 || end != 1 {
		return nil, errors.New("forward draft HTML note markers are missing or ambiguous")
	}
	startIndex := strings.Index(value, forwardHTMLStart) + len(forwardHTMLStart)
	endIndex := strings.Index(value, forwardHTMLEnd)
	if endIndex < startIndex {
		return nil, errors.New("forward draft HTML note markers are out of order")
	}
	return []byte(value[:startIndex] + forwardHTMLNoteOpen + html.EscapeString(note) + forwardHTMLNoteClose + value[endIndex:]), nil
}

func decodeTransfer(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data)))
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
	default:
		return data, nil
	}
}

func encodeTransfer(data []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		var out bytes.Buffer
		encoder := base64.NewEncoder(base64.StdEncoding, &out)
		if _, err := encoder.Write(data); err != nil {
			return nil, err
		}
		if err := encoder.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	case "quoted-printable":
		var out bytes.Buffer
		qp := quotedprintable.NewWriter(&out)
		if _, err := qp.Write(data); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	default:
		return data, nil
	}
}

func cloneMIMEHeader(header textproto.MIMEHeader) textproto.MIMEHeader {
	clone := make(textproto.MIMEHeader, len(header))
	for key, values := range header {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}
