package pst

import (
	"bytes"
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestWindowsFiletimeToTime(t *testing.T) {
	tests := []struct {
		name string
		ft   int64
		want time.Time
	}{
		{
			name: "zero",
			ft:   0,
			want: time.Time{},
		},
		{
			name: "unix epoch",
			// 1970-01-01 00:00:00 UTC in Windows FILETIME
			ft:   116444736000000000,
			want: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "2024-01-15 10:30:00 UTC",
			// (2024-01-15T10:30:00 UTC - 1601-01-01) in 100ns intervals
			ft:   133497882000000000,
			want: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name: "negative",
			ft:   -1,
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := windowsFiletimeToTime(tt.ft)
			assertpkg.True(t, got.Equal(tt.want), "windowsFiletimeToTime(%d) = %v, want %v", tt.ft, got, tt.want)
		})
	}
}

func TestExtractCN(t *testing.T) {
	tests := []struct {
		dn   string
		want string
	}{
		{"/O=CORP/OU=EXCHANGE/CN=RECIPIENTS/CN=JSMITH", "JSMITH"},
		{"/o=Contoso/ou=Exchange/cn=Recipients/cn=jdoe", "jdoe"},
		{"user@example.com", "user@example.com"}, // not a DN
		{"", ""},
	}
	for _, tt := range tests {
		got := extractCN(tt.dn)
		assertpkg.Equal(t, tt.want, got, "extractCN(%q)", tt.dn)
	}
}

func TestIsExchangeDN(t *testing.T) {
	assertpkg.True(t, isExchangeDN("/O=CORP/OU=EXCH/CN=user"), "expected true for /O= DN")
	assertpkg.True(t, isExchangeDN("/o=corp/cn=user"), "expected true for /o= DN")
	assertpkg.False(t, isExchangeDN("user@example.com"), "expected false for SMTP address")
}

func TestBuildRFC5322_SynthesizedHeaders(t *testing.T) {
	assert := assertpkg.New(t)
	msg := &MessageEntry{
		EntryID:     "12345",
		FolderPath:  "Inbox",
		Subject:     "Hello World",
		BodyText:    "This is a test message.",
		SenderName:  "Alice",
		SenderEmail: "alice@example.com",
		DisplayTo:   "Bob",
		MessageID:   "<abc123@example.com>",
		SentAt:      time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	raw, err := BuildRFC5322(msg, nil)
	requirepkg.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(s, "From:", "missing From header")
	assert.Contains(s, "alice@example.com", "missing sender email")
	assert.Contains(s, "Subject:", "missing Subject header")
	assert.Contains(s, "Message-Id:", "missing Message-Id header")
	assert.Contains(s, "X-Msgvault-Synthesized: true", "missing X-Msgvault-Synthesized header")
	assert.Contains(s, "text/plain", "missing text/plain content type")
	assert.Contains(s, "This is a test message", "body text not found in output")
}

func TestBuildRFC5322_TransportHeaders(t *testing.T) {
	assert := assertpkg.New(t)
	transportHeaders := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Test\r\nMessage-ID: <orig@example.com>\r\nDate: Mon, 15 Jan 2024 10:30:00 +0000\r\n"

	msg := &MessageEntry{
		EntryID:          "99",
		TransportHeaders: transportHeaders,
		BodyText:         "Body text here.",
		BodyHTML:         "<p>Body HTML here.</p>",
	}

	raw, err := BuildRFC5322(msg, nil)
	requirepkg.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	// Original headers should be present.
	assert.Contains(s, "From: alice@example.com", "missing original From header")
	assert.Contains(s, "Message-ID: <orig@example.com>", "missing original Message-ID header")
	// Should NOT have synthesized header.
	assert.NotContains(s, "X-Msgvault-Synthesized", "should not have X-Msgvault-Synthesized when transport headers present")
	// Both text and HTML → multipart/alternative.
	assert.Contains(s, "multipart/alternative", "expected multipart/alternative for text+html body")
}

func TestBuildRFC5322_WithAttachments(t *testing.T) {
	assert := assertpkg.New(t)
	msg := &MessageEntry{
		EntryID:     "42",
		Subject:     "With attachment",
		BodyText:    "See attached.",
		SenderEmail: "sender@example.com",
	}
	attachments := []AttachmentEntry{
		{
			Filename: "report.pdf",
			MIMEType: "application/pdf",
			Content:  []byte("%PDF-1.4 test"),
		},
	}

	raw, err := BuildRFC5322(msg, attachments)
	requirepkg.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(s, "multipart/mixed", "expected multipart/mixed for message with attachments")
	assert.Contains(s, "report.pdf", "attachment filename not found")
	assert.Contains(s, "application/pdf", "attachment content type not found")
}

func TestBuildRFC5322_EmptyBody(t *testing.T) {
	msg := &MessageEntry{
		EntryID:     "1",
		SenderEmail: "a@b.com",
		Subject:     "No body",
	}

	raw, err := BuildRFC5322(msg, nil)
	requirepkg.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assertpkg.Contains(t, s, "text/plain", "expected text/plain even for empty body")
}

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"normal@example.com", "normal@example.com"},
		{"evil@example.com\r\nBcc: victim@evil.com", "evil@example.comBcc: victim@evil.com"},
		{"has\nnewline", "hasnewline"},
		{"has\rreturn", "hasreturn"},
	}
	for _, tt := range tests {
		got := sanitizeHeaderValue(tt.in)
		assertpkg.Equal(t, tt.want, got, "sanitizeHeaderValue(%q)", tt.in)
	}
}

func TestBuildRFC5322_HeaderInjection(t *testing.T) {
	msg := &MessageEntry{
		EntryID:     "1",
		SenderEmail: "evil@example.com\r\nBcc: victim@evil.com",
		Subject:     "Test",
		BodyText:    "body",
	}
	raw, err := BuildRFC5322(msg, nil)
	requirepkg.NoError(t, err, "BuildRFC5322")
	// Check that "Bcc:" does not appear as a separate header line (the actual
	// injection vector). A sanitized value may still contain "Bcc:" as a
	// substring within the From address, but not as a new header line.
	assertpkg.NotContains(t, string(raw), "\r\nBcc:", "header injection: Bcc header was injected via SenderEmail")
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\evil\payload.exe`, "payload.exe"},
		{"file\x00name.txt", "filename.txt"},
		{"normal.doc", "normal.doc"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.in)
		assertpkg.Equal(t, tt.want, got, "sanitizeFilename(%q)", tt.in)
	}
}

func TestSanitizeContentID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc123@example.com", "abc123@example.com"},
		{"<injected>header\r\n", "injectedheader"},
	}
	for _, tt := range tests {
		got := sanitizeContentID(tt.in)
		assertpkg.Equal(t, tt.want, got, "sanitizeContentID(%q)", tt.in)
	}
}

func TestWriteQP_TrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	writeQP(&buf, "hello \nworld")
	got := buf.String()
	assertpkg.Contains(t, got, "hello=20\r\n", "trailing space not encoded: got %q", got)
}

func TestBuildRFC5322_TransportHeadersStripMIME(t *testing.T) {
	assert := assertpkg.New(t)
	// Transport headers that include MIME headers — these should be stripped.
	transportHeaders := "From: alice@example.com\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=us-ascii\r\nContent-Transfer-Encoding: 7bit\r\nSubject: Old MIME\r\n"

	msg := &MessageEntry{
		TransportHeaders: transportHeaders,
		BodyText:         "Hello.",
	}

	raw, err := BuildRFC5322(msg, nil)
	requirepkg.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	// From and Subject should be present.
	assert.Contains(s, "From: alice@example.com", "From header missing")
	assert.Contains(s, "Subject: Old MIME", "Subject header missing")
	// The old Content-Type from transport headers should not appear verbatim.
	// (Our rebuilt MIME-Version and Content-Type replaces it.)
	// We expect exactly one Content-Type occurrence (ours, for text/plain).
	count := strings.Count(s, "Content-Type:")
	assert.Equal(1, count, "expected 1 Content-Type header")
}
