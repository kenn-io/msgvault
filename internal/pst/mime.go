package pst

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// BuildRFC5322 constructs raw RFC 5322/MIME bytes from a MessageEntry and its
// attachments.
//
// Strategy:
//  1. If TransportHeaders is non-empty, use those verbatim as the message
//     headers (stripping any existing MIME content headers we'll replace).
//  2. Otherwise, synthesize headers from MAPI properties.
//  3. Build the body as multipart/alternative when both text and HTML are
//     present, or as a simple text/plain or text/html part when only one
//     exists.
//  4. When attachments are present, wrap the body in multipart/mixed.
func BuildRFC5322(msg *MessageEntry, attachments []AttachmentEntry) ([]byte, error) {
	// headerBuf holds all RFC 5322 header lines (without the trailing blank line).
	// bodyBuf holds the MIME body (MIME-Version, Content-Type, blank line, body).
	var headerBuf, bodyBuf bytes.Buffer

	// Write message-identifying headers.
	if msg.TransportHeaders != "" {
		writeTransportHeaders(&headerBuf, msg.TransportHeaders)
	} else {
		writeSynthesizedHeaders(&headerBuf, msg)
	}

	hasText := msg.BodyText != ""
	hasHTML := msg.BodyHTML != ""
	hasAtts := len(attachments) > 0

	switch {
	case hasAtts:
		// Wrap body and attachments in multipart/mixed.
		mw := multipart.NewWriter(&bodyBuf)
		fmt.Fprintf(&headerBuf, "MIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n\tboundary=%q\r\n\r\n", mw.Boundary())

		// Body sub-part.
		bh := make(textproto.MIMEHeader)
		if hasText && hasHTML {
			// Build the multipart/alternative sub-part into a buffer so we know
			// the boundary before writing the outer part header.
			var innerBuf bytes.Buffer
			altW := multipart.NewWriter(&innerBuf)
			writeTextPart(altW, msg.BodyText)
			writeHTMLPart(altW, msg.BodyHTML)
			_ = altW.Close()

			bh.Set("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, altW.Boundary()))
			pw, _ := mw.CreatePart(bh)
			_, _ = pw.Write(innerBuf.Bytes())
		} else if hasHTML {
			bh.Set("Content-Type", "text/html; charset=utf-8")
			bh.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, _ := mw.CreatePart(bh)
			writeQP(pw, msg.BodyHTML)
		} else {
			bh.Set("Content-Type", "text/plain; charset=utf-8")
			bh.Set("Content-Transfer-Encoding", "quoted-printable")
			pw, _ := mw.CreatePart(bh)
			writeQP(pw, msg.BodyText)
		}

		// Attachment parts.
		for i := range attachments {
			att := &attachments[i]
			ah := make(textproto.MIMEHeader)
			ct := att.MIMEType
			if ct == "" {
				ct = "application/octet-stream"
			} else if _, _, err := mime.ParseMediaType(ct); err != nil {
				ct = "application/octet-stream"
			}
			fname := sanitizeFilename(att.Filename)
			if fname != "" {
				ah.Set("Content-Type", mime.FormatMediaType(ct, map[string]string{"name": fname}))
			} else {
				ah.Set("Content-Type", ct)
			}
			if att.ContentID != "" {
				cid := sanitizeContentID(att.ContentID)
				ah.Set("Content-Id", "<"+cid+">")
				ah.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", fname))
			} else {
				ah.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fname))
			}
			ah.Set("Content-Transfer-Encoding", "base64")
			pw, _ := mw.CreatePart(ah)
			enc := base64.NewEncoder(base64.StdEncoding, pw)
			_, _ = enc.Write(att.Content)
			_ = enc.Close()
		}
		_ = mw.Close()

	case hasText && hasHTML:
		// multipart/alternative with no attachments.
		mw := multipart.NewWriter(&bodyBuf)
		fmt.Fprintf(&headerBuf, "MIME-Version: 1.0\r\nContent-Type: multipart/alternative;\r\n\tboundary=%q\r\n\r\n", mw.Boundary())
		writeTextPart(mw, msg.BodyText)
		writeHTMLPart(mw, msg.BodyHTML)
		_ = mw.Close()

	case hasHTML:
		headerBuf.WriteString("MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&bodyBuf, msg.BodyHTML)

	default:
		// text/plain only, or empty body.
		headerBuf.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&bodyBuf, msg.BodyText)
	}

	return append(headerBuf.Bytes(), bodyBuf.Bytes()...), nil
}

// writeTextPart writes a text/plain MIME part to mw.
func writeTextPart(mw *multipart.Writer, text string) {
	th := make(textproto.MIMEHeader)
	th.Set("Content-Type", "text/plain; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, _ := mw.CreatePart(th)
	writeQP(pw, text)
}

// writeHTMLPart writes a text/html MIME part to mw.
func writeHTMLPart(mw *multipart.Writer, html string) {
	th := make(textproto.MIMEHeader)
	th.Set("Content-Type", "text/html; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	pw, _ := mw.CreatePart(th)
	writeQP(pw, html)
}

// writeTransportHeaders writes the original transport headers to buf,
// stripping any existing MIME content headers that we will replace.
func writeTransportHeaders(buf *bytes.Buffer, headers string) {
	// Normalise line endings.
	headers = strings.ReplaceAll(headers, "\r\n", "\n")
	headers = strings.ReplaceAll(headers, "\r", "\n")

	lines := strings.Split(headers, "\n")

	skipContinuation := false
	for _, line := range lines {
		if line == "" {
			// End of headers.
			break
		}
		// Folded header continuation lines start with whitespace.
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !skipContinuation {
				buf.WriteString(line)
				buf.WriteString("\r\n")
			}
			continue
		}
		// New header field — strip MIME content headers we'll rebuild.
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "mime-version:") ||
			strings.HasPrefix(lower, "content-type:") ||
			strings.HasPrefix(lower, "content-transfer-encoding:") {
			skipContinuation = true
			continue
		}
		skipContinuation = false
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}
}

// writeSynthesizedHeaders writes RFC 5322 headers synthesized from MAPI
// properties when TransportMessageHeaders is absent.
func writeSynthesizedHeaders(buf *bytes.Buffer, msg *MessageEntry) {
	writeHeader(buf, "From", formatAddr(msg.SenderName, msg.SenderEmail))

	if msg.DisplayTo != "" {
		writeHeader(buf, "To", formatDisplayList(msg.DisplayTo))
	}
	if msg.DisplayCc != "" {
		writeHeader(buf, "Cc", formatDisplayList(msg.DisplayCc))
	}
	if msg.DisplayBcc != "" {
		writeHeader(buf, "Bcc", formatDisplayList(msg.DisplayBcc))
	}

	t := msg.SentAt
	if t.IsZero() {
		t = msg.ReceivedAt
	}
	if t.IsZero() {
		t = msg.CreationTime
	}
	if !t.IsZero() {
		writeHeader(buf, "Date", t.Format(time.RFC1123Z))
	}

	if msg.Subject != "" {
		writeHeader(buf, "Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	}

	if msg.MessageID != "" {
		mid := sanitizeHeaderValue(msg.MessageID)
		if !strings.HasPrefix(mid, "<") {
			mid = "<" + mid + ">"
		}
		writeHeader(buf, "Message-Id", mid)
	}

	if msg.InReplyTo != "" {
		irt := sanitizeHeaderValue(msg.InReplyTo)
		if !strings.HasPrefix(irt, "<") {
			irt = "<" + irt + ">"
		}
		writeHeader(buf, "In-Reply-To", irt)
	}

	if msg.References != "" {
		writeHeader(buf, "References", sanitizeHeaderValue(msg.References))
	}

	writeHeader(buf, "X-Msgvault-Source", "pst")
	writeHeader(buf, "X-Msgvault-Synthesized", "true")
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

// sanitizeHeaderValue strips CR and LF characters to prevent header injection.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// sanitizeFilename strips path components and dangerous characters from
// attachment filenames sourced from PST data. Handles both Unix and Windows
// path separators since PST data originates on Windows.
func sanitizeFilename(name string) string {
	// Normalize Windows backslash separators before calling filepath.Base.
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	if name == "." {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
}

// sanitizeContentID strips characters that could break the Content-Id
// angle-bracket wrapper.
func sanitizeContentID(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '\r', '\n':
			return -1
		default:
			return r
		}
	}, s)
}

// formatAddr formats a display name + email as "Name <email>" or just email.
func formatAddr(name, email string) string {
	email = sanitizeHeaderValue(email)
	if name == "" && email == "" {
		return ""
	}
	if name == "" {
		return email
	}
	if email == "" {
		return mime.QEncoding.Encode("utf-8", name)
	}
	return fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", name), email)
}

// formatDisplayList converts a semicolon-separated PST display list to a
// comma-separated header value. PST DisplayTo/Cc/Bcc fields contain display
// names only (not email addresses), so we emit them without angle brackets.
func formatDisplayList(display string) string {
	parts := strings.Split(display, ";")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cleaned = append(cleaned, mime.QEncoding.Encode("utf-8", p))
		}
	}
	return strings.Join(cleaned, ", ")
}

// writeQP writes s as quoted-printable text to dst.
// Lines longer than 76 characters are soft-wrapped with "=\r\n".
// Trailing whitespace before line breaks is encoded per RFC 2045 §6.7.
func writeQP(dst interface{ Write([]byte) (int, error) }, s string) {
	const maxLine = 76
	var line strings.Builder

	encodeTrailingWS := func() {
		str := line.String()
		if len(str) == 0 {
			return
		}
		last := str[len(str)-1]
		if last == ' ' || last == '\t' {
			line.Reset()
			line.WriteString(str[:len(str)-1])
			fmt.Fprintf(&line, "=%02X", last)
		}
	}

	flush := func(soft bool) {
		if !soft {
			encodeTrailingWS()
		}
		if soft {
			_, _ = dst.Write([]byte(line.String() + "=\r\n"))
		} else {
			_, _ = dst.Write([]byte(line.String() + "\r\n"))
		}
		line.Reset()
	}

	for _, b := range []byte(s) {
		var encoded string
		switch {
		case b == '\r':
			continue
		case b == '\n':
			flush(false)
			continue
		case b == '=':
			encoded = "=3D"
		case b < 32 || b > 126:
			encoded = fmt.Sprintf("=%02X", b)
		default:
			encoded = string(rune(b))
		}

		if line.Len()+len(encoded) > maxLine {
			flush(true)
		}
		line.WriteString(encoded)
	}
	if line.Len() > 0 {
		flush(false)
	}
}
