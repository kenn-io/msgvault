package mime

import (
	"testing"
	"time"

	"github.com/jhillyerd/enmime"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// emailOptions is an alias for testemail.Options for local convenience.
type emailOptions = testemail.Options

// makeRawEmail delegates to testemail.MakeRaw.
func makeRawEmail(opts emailOptions) []byte {
	return testemail.MakeRaw(opts)
}

// mustParse calls Parse and fails the test on error.
func mustParse(t *testing.T, raw []byte) *Message {
	t.Helper()
	msg, err := Parse(raw)
	requirepkg.NoError(t, err, "Parse() failed")
	return msg
}

// parseEmail combines makeRawEmail and mustParse into a single test helper.
func parseEmail(t *testing.T, opts emailOptions) *Message {
	t.Helper()
	return mustParse(t, makeRawEmail(opts))
}

// assertAddress checks that got has exactly wantLen elements and got[idx] has the expected email and (optionally) domain.
func assertAddress(t *testing.T, got []Address, wantLen, idx int, wantEmail, wantDomain string) {
	t.Helper()
	requirepkg.Len(t, got, wantLen, "Address slice length")
	requirepkg.GreaterOrEqual(t, idx, 0, "idx %d out of bounds for slice of length %d", idx, len(got))
	requirepkg.Less(t, idx, len(got), "idx %d out of bounds for slice of length %d", idx, len(got))
	assertpkg.Equal(t, wantEmail, got[idx].Email, "Address[%d].Email", idx)
	if wantDomain != "" {
		assertpkg.Equal(t, wantDomain, got[idx].Domain, "Address[%d].Domain", idx)
	}
}

func TestExtractDomain(t *testing.T) {
	tests := []struct {
		email  string
		domain string
	}{
		{"user@example.com", "example.com"},
		{"USER@EXAMPLE.COM", "example.com"},
		{"user@sub.domain.org", "sub.domain.org"},
		{"nodomain", ""},
		{"", ""},
		{"@domain.com", "domain.com"},
	}

	for _, tc := range tests {
		t.Run(tc.email, func(t *testing.T) {
			got := extractDomain(tc.email)
			assertpkg.Equal(t, tc.domain, got, "extractDomain(%q)", tc.email)
		})
	}
}

func TestParseReferences(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"<abc@example.com>", []string{"abc@example.com"}},
		{"<a@x.com> <b@y.com>", []string{"a@x.com", "b@y.com"}},
		{"<a@x.com>\n\t<b@y.com>", []string{"a@x.com", "b@y.com"}},
		{"", nil},
		{"   ", nil},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := parseReferences(tc.input)
			assertpkg.Equal(t, tc.want, got, "parseReferences(%q)", tc.input)
		})
	}
}

func TestHasNumericOffset(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Numeric offsets should return true
		{"+0700", true},
		{"-0700", true},
		{"+07:00", true},
		{"-07:00", true},
		{"Mon, 02 Jan 2006 15:04:05 -0700", true},
		{"Mon, 02 Jan 2006 15:04:05 +0000", true},
		{"2006-01-02T15:04:05-07:00", true},
		{"2006-01-02T15:04:05+00:00", true},

		// Z suffix (UTC) should return true
		{"2006-01-02T15:04:05Z", true},
		{"Z", true},

		// Named timezones should return false
		{"MST", false},
		{"Mon, 02 Jan 2006 15:04:05 MST", false},
		{"Mon, 02 Jan 2006 15:04:05 PST", false},
		{"Mon Jan 2 15:04:05 MST 2006", false},

		// Mixed: numeric offset with parenthesized named TZ should return true
		{"Mon, 02 Jan 2006 15:04:05 -0700 (PST)", true},
		{"Mon, 02 Jan 2006 15:04:05 +0700 (UTC)", true},

		// Empty and no timezone
		{"", false},
		{"Mon, 02 Jan 2006 15:04:05", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := hasNumericOffset(tc.input)
			assertpkg.Equal(t, tc.want, got, "hasNumericOffset(%q)", tc.input)
		})
	}
}

func TestToUTC(t *testing.T) {
	// Test with numeric offset: should perform proper timezone conversion
	t.Run("numeric offset converts to UTC", func(t *testing.T) {
		// Create a time at 15:04:05 in -0700 timezone
		loc := time.FixedZone("test", -7*60*60)
		input := time.Date(2006, 1, 2, 15, 4, 5, 0, loc)

		got := toUTC(input, true)

		// Should be 22:04:05 UTC (15:04:05 + 7 hours)
		want := time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)
		assertpkg.True(t, got.Equal(want), "toUTC() with numeric offset = %v, want %v", got, want)
	})

	// Test with named timezone: should keep same time values but mark as UTC
	t.Run("named timezone keeps same values as UTC", func(t *testing.T) {
		// Create a time at 15:04:05 with some location
		loc := time.FixedZone("test", -7*60*60)
		input := time.Date(2006, 1, 2, 15, 4, 5, 0, loc)

		got := toUTC(input, false)

		// Should be 15:04:05 UTC (same wall-clock time, different instant)
		want := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
		assertpkg.True(t, got.Equal(want), "toUTC() with named timezone = %v, want %v", got, want)
		assertpkg.Equal(t, time.UTC, got.Location(), "toUTC() location")
	})
}

func TestParseDate(t *testing.T) {
	// parseDate returns zero time (not error) for unparseable dates.
	// This is intentional - malformed dates are common in email and
	// shouldn't fail the entire parse.

	tests := []struct {
		name  string
		input string
		want  time.Time // Zero value means we expect parse failure
	}{
		// Valid RFC date formats with numeric offsets
		{"RFC1123Z", "Mon, 02 Jan 2006 15:04:05 -0700",
			time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)},
		{"no weekday", "02 Jan 2006 15:04:05 -0700",
			time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)},
		{"parenthesized zone", "Mon, 02 Jan 2006 15:04:05 -0700 (PST)",
			time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)},
		{"double space after comma", "Mon,  2 Dec 2024 11:42:03 +0000 (UTC)",
			time.Date(2024, 12, 2, 11, 42, 3, 0, time.UTC)},
		{"ISO 8601 offset", "2006-01-02T15:04:05-07:00",
			time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)},
		{"SQL-like with tz", "2006-01-02 15:04:05 -0700",
			time.Date(2006, 1, 2, 22, 4, 5, 0, time.UTC)},

		// Z suffix (UTC) - should work like numeric offset
		{"ISO 8601 UTC", "2006-01-02T15:04:05Z",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},

		// Named timezone handling - time values kept as-is, marked UTC
		// Named TZ offsets are platform-dependent, so we treat them as UTC
		{"RFC1123 named zone MST", "Mon, 2 Jan 2006 15:04:05 MST",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},
		{"single-digit day named zone", "Mon, 2 Jan 2006 15:04:05 PST",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},
		{"no weekday named zone", "02 Jan 2006 15:04:05 EST",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},
		{"UnixDate format", "Mon Jan  2 15:04:05 MST 2006",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},
		// Verify named TZ works regardless of system timezone (was broken on CST systems)
		{"RFC1123 named zone EST", "Mon, 02 Jan 2006 15:04:05 EST",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},
		{"RFC1123 named zone CST", "Mon, 02 Jan 2006 15:04:05 CST",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},

		// No timezone at all - treated like named TZ (no offset to convert)
		{"SQL-like no tz", "2006-01-02 15:04:05",
			time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)},

		// Edge case: parenthesized TZ differs from main TZ
		// The numeric offset (+0700) should be used for conversion
		{"numeric offset with different paren TZ", "Mon, 02 Jan 2006 15:04:05 +0700 (UTC)",
			time.Date(2006, 1, 2, 8, 4, 5, 0, time.UTC)},

		// Invalid/unparseable dates should return zero time
		{"empty", "", time.Time{}},
		{"garbage", "not a date", time.Time{}},
		{"date only", "2006-01-02", time.Time{}},
		{"spelled month", "January 2, 2006", time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			got := parseDate(tc.input)
			if tc.want.IsZero() {
				assert.True(got.IsZero(), "parseDate(%q) = %v, want zero time", tc.input, got)
				return
			}
			assert.True(got.Equal(tc.want), "parseDate(%q) = %v, want %v", tc.input, got, tc.want)
			assert.Equal(time.UTC, got.Location(), "parseDate(%q) location", tc.input)
		})
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Basic tag stripping
		{"paragraph", "<p>Hello</p>", "Hello"},
		{"nested_span", "<div><span>Nested</span></div>", "Nested"},
		{"no_tags", "No tags", "No tags"},
		{"inline_tags", "<b>Bold</b> and <i>italic</i>", "Bold and italic"},
		{"empty", "", ""},

		// Script/style removal (including content)
		{"script_removed", "<script>alert('xss')</script>Text", "Text"},
		{"style_removed", "<style>.class{color:red}</style>Content", "Content"},
		{"head_removed", "<head><title>Title</title></head>Body", "Body"},

		// Newline normalization
		{"crlf_to_lf", "Line1\r\nLine2\r\nLine3", "Line1\nLine2\nLine3"},
		{"collapse_newlines", "Multiple\n\n\n\nNewlines", "Multiple\n\nNewlines"},

		// HTML entities
		{"nbsp_entity", "Hello&nbsp;World", "Hello World"},
		{"amp_entity", "Tom &amp; Jerry", "Tom & Jerry"},
		{"lt_gt_entities", "5 &lt; 10 &gt; 3", "5 < 10 > 3"},
		{"quote_entity", "&quot;quoted&quot;", "\"quoted\""},
		{"numeric_entity", "&#169; 2024", "© 2024"},
		{"hex_entity", "&#x2022; bullet", "• bullet"},

		// Block elements create line breaks
		{"br_tag", "Line1<br>Line2", "Line1\nLine2"},
		{"br_self_close", "Line1<br/>Line2", "Line1\nLine2"},
		{"paragraph_breaks", "<p>Para1</p><p>Para2</p>", "Para1\n\nPara2"},
		{"div_breaks", "<div>Block1</div><div>Block2</div>", "Block1\n\nBlock2"},
		{"heading_breaks", "<h1>Title</h1><p>Content</p>", "Title\n\nContent"},

		// Complex HTML email
		{
			"complex_html",
			`<html><head><style>.x{}</style></head><body>
			<p>Hello,</p>
			<p>This is a <b>test</b> email with &amp; special chars.</p>
			<br>
			<p>Thanks!</p>
			</body></html>`,
			"Hello,\n\nThis is a test email with & special chars.\n\nThanks!",
		},

		// Whitespace collapse
		{"multiple_spaces", "Hello    World", "Hello World"},
		{"nbsp_spaces", "Hello&nbsp;&nbsp;&nbsp;World", "Hello World"},

		// Preformatted content - whitespace is NOT preserved (documented behavior)
		// This is acceptable for email preview where code formatting is secondary
		{"pre_whitespace_collapsed", "<pre>  code  here  </pre>", "code here"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripHTML(tc.input)
			assertpkg.Equal(t, tc.want, got, "StripHTML()")
		})
	}
}

func TestMessage_GetBodyText(t *testing.T) {
	// Prefers plain text
	msg := &Message{BodyText: "plain", BodyHTML: "<p>html</p>"}
	assertpkg.Equal(t, "plain", msg.GetBodyText())

	// Falls back to HTML
	msg = &Message{BodyHTML: "<p>html only</p>"}
	assertpkg.Equal(t, "html only", msg.GetBodyText())

	// Empty
	msg = &Message{}
	assertpkg.Empty(t, msg.GetBodyText())
}

func TestMessage_GetFirstFrom(t *testing.T) {
	msg := &Message{
		From: []Address{
			{Name: "Alice", Email: "alice@example.com", Domain: "example.com"},
			{Name: "Bob", Email: "bob@example.com", Domain: "example.com"},
		},
	}

	got := msg.GetFirstFrom()
	assertpkg.Equal(t, "alice@example.com", got.Email)

	// Empty
	msg = &Message{}
	got = msg.GetFirstFrom()
	assertpkg.Empty(t, got.Email)
}

// TestParse_MinimalMessage tests our Parse wrapper with a minimal valid message.
// This verifies our wrapper works, not enmime's parsing logic.
func TestParse_MinimalMessage(t *testing.T) {
	msg := parseEmail(t, emailOptions{
		Body: "Body text",
		Headers: map[string]string{
			"Date": "Mon, 02 Jan 2006 15:04:05 -0700",
		},
	})

	assertAddress(t, msg.From, 1, 0, "sender@example.com", "example.com")
	assertAddress(t, msg.To, 1, 0, "recipient@example.com", "")
	assertpkg.Equal(t, "Test", msg.Subject)

	assertpkg.Equal(t, "Body text", msg.BodyText)
}

// TestParse_InvalidCharset verifies enmime handles malformed charsets gracefully.
// Enmime should not fail on invalid charset - it attempts conversion and collects errors.
func TestParse_InvalidCharset(t *testing.T) {
	// Message with non-existent charset - enmime should handle this gracefully
	msg := parseEmail(t, emailOptions{
		ContentType: "text/plain; charset=invalid-charset-xyz",
		Body:        "Body text",
	})

	// Should still be able to access subject and addresses
	assertpkg.Equal(t, "Test", msg.Subject)

	// Body might be garbled or empty, but should not crash
	t.Logf("Body text with invalid charset: %q", msg.BodyText)
	t.Logf("Parsing errors: %v", msg.Errors)
}

// TestParse_Latin1Charset verifies Latin-1 (ISO-8859-1) charset is handled.
func TestParse_Latin1Charset(t *testing.T) {
	// Latin-1 encoded content with special characters.
	// This test uses raw bytes because the subject/body contain non-UTF-8 Latin-1 bytes.
	raw := []byte("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Caf\xe9\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\nCaf\xe9 au lait")

	msg := mustParse(t, raw)

	// enmime should convert Latin-1 to UTF-8
	// é in Latin-1 is 0xe9, in UTF-8 it's 0xc3 0xa9
	assertpkg.Equal(t, "Café au lait", msg.BodyText)
}

// TestParse_RFC2822GroupAddress verifies RFC 2822 group address syntax is handled.
// Group syntax: "group-name: addr1, addr2, ...;".
func TestParse_RFC2822GroupAddress(t *testing.T) {
	// Message with undisclosed-recipients group (common in BCC scenarios)
	msg := parseEmail(t, emailOptions{
		To:   "undisclosed-recipients:;",
		Body: "Body",
	})

	assertpkg.Equal(t, "Test", msg.Subject)

	// Group with no members should result in empty To list
	assertpkg.Empty(t, msg.To, "To should be empty for undisclosed-recipients group")
}

// TestParse_RFC2822GroupAddressWithMembers verifies group with actual addresses.
func TestParse_RFC2822GroupAddressWithMembers(t *testing.T) {
	msg := parseEmail(t, emailOptions{
		To:   "team: alice@example.com, bob@example.com;",
		Body: "Body",
	})

	assertpkg.Equal(t, "Test", msg.Subject)

	// Verify enmime flattens the group into individual recipients
	wantEmails := []string{"alice@example.com", "bob@example.com"}
	gotEmails := make([]string, len(msg.To))
	for i, addr := range msg.To {
		gotEmails[i] = addr.Email
	}
	assertpkg.Equal(t, wantEmails, gotEmails, "Group Members")
}

// TestIsBodyPart_ContentTypeWithParams tests that Content-Type with charset
// parameters is correctly identified as body content.
func TestIsBodyPart_ContentTypeWithParams(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		filename    string
		disposition string
		wantIsBody  bool
	}{
		// Content-Type with charset parameter should still be body
		{"text/plain with charset", "text/plain; charset=utf-8", "", "", true},
		{"text/html with charset", "text/html; charset=utf-8", "", "", true},
		{"text/plain with format", "text/plain; format=flowed", "", "", true},
		{"TEXT/PLAIN uppercase with charset", "TEXT/PLAIN; CHARSET=UTF-8", "", "", true},

		// Non-text types are not body parts
		{"application/pdf", "application/pdf", "", "", false},
		{"image/png", "image/png", "", "", false},

		// With filename → attachment, not body
		{"text/plain with filename", "text/plain; charset=utf-8", "file.txt", "", false},
		{"text/html with filename", "text/html; charset=utf-8", "page.html", "", false},

		// Explicit disposition: attachment (with or without params)
		{"attachment disposition", "text/plain", "", "attachment", false},
		{"attachment with params", "text/plain", "", "attachment; filename=\"x.txt\"", false},
		{"ATTACHMENT uppercase", "text/plain", "", "ATTACHMENT; filename=\"x.txt\"", false},

		// Inline disposition is still body
		{"inline disposition", "text/plain; charset=utf-8", "", "inline", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock enmime.Part (we only need the fields isBodyPart checks)
			part := &enmime.Part{
				ContentType: tt.contentType,
				FileName:    tt.filename,
				Disposition: tt.disposition,
			}
			got := isBodyPart(part)
			assertpkg.Equal(t, tt.wantIsBody, got)
		})
	}
}
