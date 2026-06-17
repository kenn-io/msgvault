// Package email provides test helpers for constructing raw RFC 2822 email messages.
package email

import (
	"sort"
	"strings"
)

// Options configures a raw RFC 2822 email message for testing.
type Options struct {
	From        string
	To          string
	Subject     string
	ContentType string
	Body        string
	Headers     map[string]string
}

// MakeRaw constructs an RFC 2822 compliant raw message with correct \r\n line endings.
func MakeRaw(opts Options) []byte {
	var b strings.Builder

	if opts.From == "" {
		opts.From = "sender@example.com"
	}
	if opts.To == "" {
		opts.To = "recipient@example.com"
	}
	if opts.Subject == "" {
		opts.Subject = "Test"
	}

	b.WriteString("From: " + opts.From + "\r\n")
	b.WriteString("To: " + opts.To + "\r\n")
	b.WriteString("Subject: " + opts.Subject + "\r\n")

	if opts.ContentType != "" {
		b.WriteString("Content-Type: " + opts.ContentType + "\r\n")
	}

	keys := make([]string, 0, len(opts.Headers))
	for k := range opts.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + ": " + opts.Headers[k] + "\r\n")
	}

	b.WriteString("\r\n")
	b.WriteString(opts.Body)

	return []byte(b.String())
}
