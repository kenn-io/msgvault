package imap

import (
	"bytes"
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

// BuildCompose creates a new plain-text draft with the supplied envelope.
func BuildCompose(options ComposeOptions, now time.Time, messageID string) (ReplyDraft, error) {
	if !utf8.ValidString(options.From) || strings.ContainsAny(options.From, "\r\n") {
		return ReplyDraft{}, errors.New("invalid compose From address")
	}
	from, err := parseOneAddress(options.From)
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("invalid compose From address: %w", err)
	}
	if !utf8.ValidString(options.Body) || strings.ContainsAny(options.Body, "\x00") {
		return ReplyDraft{}, errors.New("invalid compose body")
	}
	to, err := parseComposeAddresses("To", options.To)
	if err != nil {
		return ReplyDraft{}, err
	}
	cc, err := parseComposeAddresses("Cc", options.Cc)
	if err != nil {
		return ReplyDraft{}, err
	}
	bcc, err := parseComposeAddresses("Bcc", options.Bcc)
	if err != nil {
		return ReplyDraft{}, err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return ReplyDraft{}, errors.New("compose requires at least one recipient")
	}
	if !validHeaderValue(options.Subject) {
		return ReplyDraft{}, errors.New("invalid compose Subject header")
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
		From: []*mail.Address{from}, To: to, Cc: cc, Bcc: bcc,
		Subject: options.Subject, MessageID: messageID,
	}, options.Body, now)
}

func parseComposeAddresses(name string, values []string) ([]*mail.Address, error) {
	var addresses []*mail.Address
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n") || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid compose %s address", name)
		}
		parsed, err := mail.ParseAddressList(value)
		if err != nil || len(parsed) == 0 {
			if err == nil {
				err = errors.New("address list is empty")
			}
			return nil, fmt.Errorf("invalid compose %s address: %w", name, err)
		}
		for _, address := range parsed {
			if err := validateAddress(address); err != nil {
				return nil, fmt.Errorf("invalid compose %s address: %w", name, err)
			}
		}
		addresses = append(addresses, parsed...)
	}
	return addresses, nil
}

func renderPlainTextDraft(envelope plainTextEnvelope, body string, now time.Time) (ReplyDraft, error) {
	if len(envelope.From) != 1 {
		return ReplyDraft{}, errors.New("draft requires one From address")
	}
	fromValue := formatAddresses(envelope.From)
	toValue := formatAddresses(envelope.To)
	ccValue := formatAddresses(envelope.Cc)
	bccValue := formatAddresses(envelope.Bcc)
	replyToValue := formatAddresses(envelope.ReplyTo)
	for _, value := range []string{fromValue, toValue, ccValue, bccValue, replyToValue, envelope.Subject} {
		if value != "" && !validHeaderValue(value) {
			return ReplyDraft{}, errors.New("invalid draft header")
		}
	}

	var raw bytes.Buffer
	writeHeader := func(name, value string) {
		_, _ = fmt.Fprintf(&raw, "%s: %s\r\n", name, value)
	}
	writeHeader("Date", now.UTC().Format(time.RFC1123Z))
	writeHeader("From", fromValue)
	if toValue != "" {
		writeHeader("To", toValue)
	}
	if ccValue != "" {
		writeHeader("Cc", ccValue)
	}
	if bccValue != "" {
		writeHeader("Bcc", bccValue)
	}
	if replyToValue != "" {
		writeHeader("Reply-To", replyToValue)
	}
	if envelope.Subject != "" {
		subject := envelope.Subject
		if !isASCII(subject) {
			subject = stdmime.QEncoding.Encode("UTF-8", subject)
		}
		writeHeader("Subject", subject)
	}
	writeHeader("Message-ID", "<"+envelope.MessageID+">")
	if len(envelope.InReplyTo) > 0 {
		writeHeader("In-Reply-To", formatMessageIDs(envelope.InReplyTo))
	}
	if len(envelope.References) > 0 {
		writeFoldedHeader(&raw, "References", formatMessageIDs(envelope.References))
	}
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", `text/plain; charset="utf-8"`)
	writeHeader("Content-Transfer-Encoding", "quoted-printable")
	raw.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&raw)
	if _, err := io.WriteString(qp, body); err != nil {
		return ReplyDraft{}, fmt.Errorf("encode draft body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return ReplyDraft{}, fmt.Errorf("close draft body: %w", err)
	}
	parsed, err := msgmime.Parse(raw.Bytes())
	if err != nil {
		return ReplyDraft{}, fmt.Errorf("parse composed draft: %w", err)
	}
	return ReplyDraft{Raw: raw.Bytes(), Parsed: parsed}, nil
}
