package mime

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailLinkageExplicitHeaders(t *testing.T) {
	assertions := assert.New(t)
	raw := []byte("Message-ID: <Child@example.test>\r\nIn-Reply-To: (old <false@example.test>) <parent@example.test>\r\nReferences: <root@example.test>\r\n <parent@example.test>\r\nSubject: Fwd: unrelated\r\n\r\nMessage-ID: <body@example.test>\r\nIn-Reply-To: <ignored@example.test>\r\n")
	got := ExtractEmailLinkage(raw)
	assertions.Equal([]string{"parent@example.test"}, got.Replies)
	assertions.Equal([]string{"root@example.test", "parent@example.test"}, got.References)
	assertions.Empty(got.AttachedOriginals)
	assertions.Equal("complete", got.Outcome)
}

func TestEmailLinkageAttachedOriginal(t *testing.T) {
	original := "Message-ID: <received@example.test>\r\nFrom: sender@example.test\r\nTo: owner@example.test\r\nSubject: Original\r\n\r\nOriginal body.\r\n"
	for _, encoding := range []string{"7bit", "base64"} {
		t.Run(encoding, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			payload := original
			if encoding == "base64" {
				payload = base64.StdEncoding.EncodeToString([]byte(original))
			}
			raw := []byte(fmt.Sprintf("Message-ID: <forward@example.test>\r\nFrom: second-owner@example.test\r\nTo: destination@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=qa\r\n\r\n--qa\r\nContent-Type: text/plain\r\n\r\nPlease handle this.\r\n--qa\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=original.eml\r\nContent-Transfer-Encoding: %s\r\n\r\n%s\r\n--qa--\r\n", encoding, payload))
			parsed, err := Parse(raw)
			requirements.NoError(err)
			assertions.Equal("<forward@example.test>", parsed.MessageID)
			requirements.Len(parsed.Attachments, 1)
			assertions.Equal("message/rfc822", parsed.Attachments[0].ContentType)
			assertions.Contains(string(parsed.Attachments[0].Content), "Message-ID: <received@example.test>")
			got := ExtractEmailLinkage(raw)
			assertions.Equal([]string{"received@example.test"}, got.AttachedOriginals)
			assertions.Equal("complete", got.Outcome)
		})
	}
}

func TestEmailLinkageDoesNotInferFromInlineForwardOrReport(t *testing.T) {
	for _, body := range []string{
		"Subject: Fwd: Original\r\nX-Msgvault-Forward: 1\r\n\r\n---------- Forwarded message ----------\r\nMessage-ID: <received@example.test>\r\n",
		"MIME-Version: 1.0\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=report\r\n\r\n--report\r\nContent-Type: text/plain\r\n\r\nDelivery failed.\r\n--report\r\nContent-Type: message/rfc822\r\n\r\nMessage-ID: <received@example.test>\r\n\r\nOriginal.\r\n--report--\r\n",
	} {
		got := ExtractEmailLinkage([]byte("Message-ID: <notice@example.test>\r\n" + body))
		assert.Empty(t, got.AttachedOriginals)
		assert.Empty(t, got.Replies)
	}
}

func TestEmailLinkageMalformedAndRepeatedIdentity(t *testing.T) {
	for _, headers := range []string{
		"Message-ID: <nested<bad@example.test>>\r\n",
		"Message-ID: <first@example.test>\r\nMessage-ID: <second@example.test>\r\n",
		"Message-ID: <one@example.test> <two@example.test>\r\n",
		"Message-ID: <same@example.test> <same@example.test>\r\n",
	} {
		got := ExtractEmailLinkage([]byte(headers + "\r\nBody"))
		assert.Equal(t, "invalid_headers", got.Outcome)
	}
	got := ExtractEmailLinkage([]byte("Message-ID: <valid@example.test>\r\nIn-Reply-To: <same@example.test> <same@example.test>\r\n\r\nBody"))
	assert.Equal(t, []string{"same@example.test"}, got.Replies)
}

func TestEmailLinkageBoundedHeaders(t *testing.T) {
	got := ExtractEmailLinkage([]byte("X-Long: " + strings.Repeat("x", 256*1024) + "\r\nMessage-ID: <too-late@example.test>\r\n\r\nBody"))
	assert.Equal(t, "header_limit", got.Outcome)
}

func TestEmailLinkageMIMELimitsAndFailure(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{name: "missing boundary", raw: "Content-Type: multipart/mixed\r\n\r\nBody", want: "invalid_mime"},
		{name: "broken attached headers", raw: "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: message/rfc822\r\n\r\nnot a header\r\n\r\nBody\r\n--x--\r\n", want: "invalid_headers"},
		{name: "invalid reference", raw: "References: <nested<bad@example.test>>\r\n\r\nBody", want: "invalid_headers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractEmailLinkage([]byte(tc.raw))
			assert.Equal(t, tc.want, got.Outcome)
			assert.Empty(t, got.AttachedOriginals)
		})
	}
	body := "Content-Type: text/plain\r\n\r\nBody"
	for i := range 9 {
		boundary := fmt.Sprintf("depth%d", i)
		body = "Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n--" + boundary + "\r\n" + body + "\r\n--" + boundary + "--\r\n"
	}
	assert.Equal(t, "depth_limit", ExtractEmailLinkage([]byte(body)).Outcome)
	body = "Content-Type: multipart/mixed; boundary=x\r\n\r\n" + strings.Repeat("--x\r\nContent-Type: text/plain\r\n\r\nBody\r\n", 257) + "--x--\r\n"
	assert.Equal(t, "part_limit", ExtractEmailLinkage([]byte(body)).Outcome)
	assert.Equal(t, "size_limit", ExtractEmailLinkage(make([]byte, EmailLinkageMaxBytes+1)).Outcome)
}

func TestEmailLinkageIdentityGrammar(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
		valid bool
	}{
		{value: "(comment <fake@example.test>) <Actual@example.test>", want: []string{"Actual@example.test"}, valid: true},
		{value: "(nested (escaped \\))) <id@example.test>", want: []string{"id@example.test"}, valid: true},
		{value: "<id@[127.0.0.1]>", want: []string{"id@[127.0.0.1]"}, valid: true},
		{value: "<Case@example.test> <case@example.test>", want: []string{"Case@example.test", "case@example.test"}, valid: true},
		{value: "<abc123>", want: []string{"abc123"}, valid: true},
		{value: "(c) <a..b@example.test> <id@>", want: []string{"a..b@example.test", "id@"}, valid: true},
		{value: "<>"}, {value: "<nested<bad@example.test>>"}, {value: "<id@[>]>"},
		{value: "<id@example.test>garbage", want: []string{"id@example.test"}},
		{value: "junk <a@example.test> <b<c@example.test> <d@example.test>", want: []string{"a@example.test", "d@example.test"}},
		{value: "(unfinished <id@example.test>"},
		// NormalizeMessageID stores a value with no brackets whole, so a reply can name it.
		{value: "bare@example.test", want: []string{"bare@example.test"}, valid: true},
		{value: "bare-1@example.test bare-2@example.test", want: []string{"bare-1@example.test", "bare-2@example.test"}, valid: true},
		{value: "(note) bare@example.test"},
		{value: "bare@example.test <id@example.test>", want: []string{"id@example.test"}},
	} {
		got, valid := emailLinkageIDs(tc.value)
		assert.Equal(t, tc.want, got)
		assert.Equal(t, tc.valid, valid)
	}
}

func TestEmailLinkageRejectsUnsupportedOriginalEncoding(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: message/rfc822\r\nContent-Transfer-Encoding: x-unknown\r\n\r\nMessage-ID: <untrusted@example.test>\r\n\r\nBody\r\n--x--\r\n")
	got := ExtractEmailLinkage(raw)
	assert.Empty(t, got.AttachedOriginals)
	assert.Equal(t, "invalid_mime", got.Outcome)
}

// A Message-ID that NormalizeMessageID rejects stores no rfc822_message_id, so
// replies naming it cannot find the copy; the result must not read complete.
func TestEmailLinkageOwnIDTheStoreCannotMatchIsIncomplete(t *testing.T) {
	for header, want := range map[string]string{
		"Message-ID: <plain@example.test>":             "complete",
		"Message-ID: (archive) <wrapped@example.test>": "invalid_headers",
		"Message-ID: <trailing@example.test> (note)":   "invalid_headers",
	} {
		assert.Equal(t, want, ExtractEmailLinkage([]byte(header+"\r\n\r\n")).Outcome, header)
	}
}

// A header whose ID NormalizeMessageID would store links a reply, with or
// without an "@", so a reply can always find a copy the archive indexed.
// Lookups compare these keys with the canonical rfc822_message_id column.
func TestEmailLinkageAcceptsEveryStoredIDForm(t *testing.T) {
	assert := assert.New(t)
	for _, id := range []string{"abc123", "id@[]", "Mixed.Case+tag@Example.TEST", "no-at-sign", "Child@example.test", "id@[127.0.0.1]", "a.b+c@Example.Test"} {
		got := ExtractEmailLinkage([]byte("In-Reply-To: <" + id + ">\r\n\r\n"))
		assert.Equal([]string{id}, got.Replies)
		assert.Equal("complete", got.Outcome)
		assert.Equal(NormalizeMessageID("<"+id+">"), got.Replies[0], "the key is what the canonical column stores")
	}
	for _, id := range []string{"id@[>]", "id@[<@>]"} {
		got := ExtractEmailLinkage([]byte("In-Reply-To: <" + id + ">\r\n\r\n"))
		assert.Empty(got.Replies, id)
		assert.Equal("invalid_headers", got.Outcome, id)
	}
}

// A control byte cannot be stored (PostgreSQL TEXT rejects NUL), so the header
// reads as invalid and keeps its neighbors.
func TestEmailLinkageRejectsControlBytes(t *testing.T) {
	assert := assert.New(t)
	got := ExtractEmailLinkage([]byte("In-Reply-To: <a\x00b@x.test> <ok@x.test>\r\n\r\n"))
	assert.Equal([]string{"ok@x.test"}, got.Replies)
	assert.Equal("invalid_headers", got.Outcome)
}

// A digest part with no Content-Type is a message (RFC 2046 5.1.5).
func TestEmailLinkageDigestChildDefaultsToMessage(t *testing.T) {
	raw := "Content-Type: multipart/digest; boundary=dg\r\n\r\n" +
		"--dg\r\n\r\nMessage-ID: <child@example.test>\r\nSubject: Child\r\n\r\nBody\r\n" +
		"--dg\r\nContent-Type: text/plain\r\n\r\nMessage-ID: <not-a-message@example.test>\r\n" +
		"--dg--\r\n"
	got := ExtractEmailLinkage([]byte(raw))
	assert.Equal(t, []string{"child@example.test"}, got.AttachedOriginals)
	assert.Equal(t, "complete", got.Outcome)
}

func TestEmailLinkageRootAttachedOriginal(t *testing.T) {
	got := ExtractEmailLinkage([]byte("Message-ID: <wrapper@example.test>\r\nContent-Type: message/rfc822\r\n\r\nMessage-ID: <original@example.test>\r\n\r\nBody"))
	assert.Equal(t, []string{"original@example.test"}, got.AttachedOriginals)
}

// Past 128 identifiers, or past the byte limit for one, the outcome says so and
// keeps what fits. A malformed token marks the header invalid but keeps the
// valid IDs before it.
func TestEmailLinkageIdentityLimits(t *testing.T) {
	refs := func(count int, suffix string) string {
		var ids []string
		for i := range count {
			ids = append(ids, fmt.Sprintf("<id%d@example.test>", i))
		}
		return "References: " + strings.Join(ids, " ") + suffix + "\r\n\r\n"
	}
	long := strings.Repeat("a", 2048) + "@example.test"
	for _, tc := range []struct {
		name, raw, outcome string
		references         int
	}{
		{name: "129 references", raw: refs(129, ""), outcome: "identity_limit", references: 128},
		{name: "127 and junk", raw: refs(127, " junk"), outcome: "invalid_headers", references: 127},
		{name: "128 and junk", raw: refs(128, " junk"), outcome: "invalid_headers", references: 128},
		{name: "129 and junk", raw: refs(129, " junk"), outcome: "invalid_headers", references: 128},
		{name: "130 and junk", raw: refs(130, " junk"), outcome: "invalid_headers", references: 128},
		{name: "long Message-ID", raw: "Message-ID: <" + long + ">\r\n\r\n", outcome: "identity_limit"},
		{name: "long References", raw: "References: <" + long + ">\r\n\r\n", outcome: "identity_limit"},
		{name: "long In-Reply-To", raw: "In-Reply-To: <" + long + ">\r\n\r\n", outcome: "identity_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractEmailLinkage([]byte(tc.raw))
			assert.Equal(t, tc.outcome, got.Outcome)
			assert.Len(t, got.References, tc.references)
			assert.Empty(t, got.Replies)
		})
	}
}

// A header block that ends without a blank line reads the way net/mail reads
// it, so its readable keys survive.
func TestEmailLinkageReadsHeaderBlockWithoutBlankLine(t *testing.T) {
	got := ExtractEmailLinkage([]byte("Message-ID: <a@b>\r\nIn-Reply-To: <p@b>\r\nSubject: s\r\n"))
	assert.Equal(t, []string{"p@b"}, got.Replies)
	assert.Equal(t, "complete", got.Outcome)
}
