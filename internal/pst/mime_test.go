package pst

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"net/mail"
	"strings"
	"testing"
	"testing/quick"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/rotisserie/eris"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goPSTIteratorErr(cause error) error {
	err := eris.Wrap(cause, "failed to get attachment table context")
	err = eris.Wrap(err, "failed to get attachment table context")
	return eris.Wrap(err, "failed to get attachment count")
}

func TestNoAttachments(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "empty attachment table",
			err:  goPSTIteratorErr(pstlib.ErrTableContextNoRows),
			want: true,
		},
		{
			name: "empty attachment table rewrapped",
			err:  fmt.Errorf("get attachment iterator: %w", goPSTIteratorErr(pstlib.ErrTableContextNoRows)),
			want: true,
		},
		{
			name: "attachment flag unset",
			err:  pstlib.ErrAttachmentsNotFound,
			want: true,
		},
		{
			name: "same text without sentinel",
			err:  goPSTIteratorErr(errors.New("go-pst: there are no rows in this table context")),
			want: false,
		},
		{
			name: "table without columns",
			err:  goPSTIteratorErr(pstlib.ErrTableContextNoColumns),
			want: false,
		},
		{
			name: "missing local descriptor",
			err:  goPSTIteratorErr(eris.Wrap(pstlib.ErrLocalDescriptorNotFound, "failed to find attachment local descriptor")),
			want: false,
		},
		{
			name: "invalid attachment index",
			err:  goPSTIteratorErr(pstlib.ErrAttachmentIndexInvalid),
			want: false,
		},
		{
			name: "size cap",
			err:  errAttachmentTooLarge,
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, noAttachments(tt.err))
		})
	}
}

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
			assert.True(t, got.Equal(tt.want), "windowsFiletimeToTime(%d) = %v, want %v", tt.ft, got, tt.want)
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
		assert.Equal(t, tt.want, got, "extractCN(%q)", tt.dn)
	}
}

func TestIsExchangeDN(t *testing.T) {
	assert.True(t, isExchangeDN("/O=CORP/OU=EXCH/CN=user"), "expected true for /O= DN")
	assert.True(t, isExchangeDN("/o=corp/cn=user"), "expected true for /o= DN")
	assert.False(t, isExchangeDN("user@example.com"), "expected false for SMTP address")
}

func TestBuildRFC5322_SynthesizedHeaders(t *testing.T) {
	assert := assert.New(t)
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
	require.NoError(t, err, "BuildRFC5322")

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
	assert := assert.New(t)
	transportHeaders := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Test\r\nMessage-ID: <orig@example.com>\r\nDate: Mon, 15 Jan 2024 10:30:00 +0000\r\n"

	msg := &MessageEntry{
		EntryID:          "99",
		TransportHeaders: transportHeaders,
		BodyText:         "Body text here.",
		BodyHTML:         "<p>Body HTML here.</p>",
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

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
	assert := assert.New(t)
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
	require.NoError(t, err, "BuildRFC5322")

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
	require.NoError(t, err, "BuildRFC5322")

	s := string(raw)
	assert.Contains(t, s, "text/plain", "expected text/plain even for empty body")
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
		assert.Equal(t, tt.want, got, "sanitizeHeaderValue(%q)", tt.in)
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
	require.NoError(t, err, "BuildRFC5322")
	// Check that "Bcc:" does not appear as a separate header line (the actual
	// injection vector). A sanitized value may still contain "Bcc:" as a
	// substring within the From address, but not as a new header line.
	assert.NotContains(t, string(raw), "\r\nBcc:", "header injection: Bcc header was injected via SenderEmail")
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
		assert.Equal(t, tt.want, got, "sanitizeFilename(%q)", tt.in)
	}
}

func TestSanitizeContentID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc123@example.com", "abc123@example.com"},
		{"<injected>header\r\n", "injectedheader"},
	}
	for _, tt := range tests {
		got := sanitizeContentID(tt.in)
		assert.Equal(t, tt.want, got, "sanitizeContentID(%q)", tt.in)
	}
}

func TestWriteQP_TrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	writeQP(&buf, "hello \nworld")
	got := buf.String()
	assert.Contains(t, got, "hello=20\r\n", "trailing space not encoded: got %q", got)
}

func TestBuildRFC5322_TransportHeadersStripMIME(t *testing.T) {
	assert := assert.New(t)
	// Transport headers that include MIME headers — these should be stripped.
	transportHeaders := "From: alice@example.com\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=us-ascii\r\nContent-Transfer-Encoding: 7bit\r\nSubject: Old MIME\r\n"

	msg := &MessageEntry{
		TransportHeaders: transportHeaders,
		BodyText:         "Hello.",
	}

	raw, err := BuildRFC5322(msg, nil)
	require.NoError(t, err, "BuildRFC5322")

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

func TestBuildRFC5322_MAPIThreadingFallback(t *testing.T) {
	for _, tc := range []struct{ name, headers, wantID, wantParent, wantRefs string }{
		{"partial", "From: sender@example.test\r\nSubject: Thread\r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"conflicts", "mEsSaGe-iD: <original@example.test>\r\nIN-REPLY-TO: <original-parent@example.test>\r\nReferences: <original-root@example.test>\r\n", "<original@example.test>", "<original-parent@example.test>", "<original-root@example.test>"},
		{"empty", "Message-ID: \t\r\nIn-Reply-To:\r\nReferences: \r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"folded", "Message-ID:\r\n\t<folded@example.test>\r\nReferences: <original-root@example.test>\r\n <original-parent@example.test>\r\n", "<folded@example.test>", "<parent@example.test>", "<original-root@example.test> <original-parent@example.test>"},
		{"separator", "From: sender@example.test\r\n\r\nMessage-ID: <body@example.test>\r\n", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
		{"no transport", "", "<mapi@example.test>", "<parent@example.test>", "<root@example.test> <parent@example.test>"},
	} {
		for _, layout := range []string{"plain", "html", "alternative", "attachment"} {
			t.Run(tc.name+"/"+layout, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				entry := &MessageEntry{TransportHeaders: tc.headers, SenderEmail: "sender@example.test", MessageID: " mapi@example.test ", InReplyTo: " parent@example.test ", References: "<root@example.test> <parent@example.test>", BodyText: "Body"}
				var attachments []AttachmentEntry
				if layout == "html" {
					entry.BodyText = ""
					entry.BodyHTML = "<p>Body</p>"
				}
				if layout == "alternative" {
					entry.BodyHTML = "<p>Body</p>"
				}
				if layout == "attachment" {
					attachments = []AttachmentEntry{{Filename: "example.txt", Content: []byte("Attachment")}}
				}
				raw, err := BuildRFC5322(entry, attachments)
				require.NoError(err)
				parsed, err := mail.ReadMessage(bytes.NewReader(raw))
				require.NoError(err)
				assert.Equal(tc.wantID, strings.TrimSpace(parsed.Header.Get("Message-ID")))
				assert.Equal(tc.wantParent, strings.TrimSpace(parsed.Header.Get("In-Reply-To")))
				assert.Equal(tc.wantRefs, strings.TrimSpace(parsed.Header.Get("References")))
				for _, field := range []string{"Message-Id", "In-Reply-To", "References"} {
					assert.Len(parsed.Header[field], 1, field)
				}
				full, err := msgmime.Parse(raw)
				require.NoError(err)
				assert.Equal("Body", strings.TrimSpace(full.GetBodyText()))
			})
		}
	}
}

func TestBuildRFC5322_ThreadingHeaderPrecedenceProperty(t *testing.T) {
	err := quick.Check(func(mapiBytes, originalBytes []byte, useOriginal bool) bool {
		mapiID := "mapi-" + hex.EncodeToString(mapiBytes) + "@example.test"
		originalID := "original-" + hex.EncodeToString(originalBytes) + "@example.test"
		headers := "From: sender@example.test\r\n"
		want := "<" + mapiID + ">"
		if useOriginal {
			headers += "mEsSaGe-ID:\r\n\t<" + originalID + ">\r\n"
			want = "<" + originalID + ">"
		}
		raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: headers, MessageID: mapiID, BodyText: "Body"}, nil)
		if err != nil {
			return false
		}
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		return err == nil && strings.TrimSpace(parsed.Header.Get("Message-ID")) == want && len(parsed.Header["Message-Id"]) == 1
	}, nil)
	require.NoError(t, err)
}

func TestBuildRFC5322_ThreadingFallbackRejectsHeaderInjection(t *testing.T) {
	for _, headers := range []string{"", "From: sender@example.test\r\n"} {
		raw, err := BuildRFC5322(&MessageEntry{TransportHeaders: headers, MessageID: "id@example.test\r\nBcc: injected@example.test", InReplyTo: "parent@example.test\nBcc: injected@example.test", References: "<root@example.test>\r\nBcc: injected@example.test", BodyText: "Body"}, nil)
		require.NoError(t, err)
		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		require.NoError(t, err)
		assert.Empty(t, parsed.Header.Get("Bcc"))
	}
}
