package mime

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeMessageIDRejectsMalformedBrackets(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "clean bracketed", input: "<valid@example.test>", want: "valid@example.test"},
		{name: "clean bare", input: "valid@example.test", want: "valid@example.test"},
		{name: "empty brackets", input: "<>"},
		{name: "nested brackets", input: "<<valid@example.test>>"},
		{name: "missing opening bracket", input: "valid@example.test>"},
		{name: "missing closing bracket", input: "<valid@example.test"},
		{name: "space inside opening bracket", input: "< valid@example.test>"},
		{name: "space inside closing bracket", input: "<valid@example.test >"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeMessageID(tt.input))
		})
	}
}

func TestNormalizeMessageIDSanitizesInvalidUTF8(t *testing.T) {
	got := NormalizeMessageID("<invalid-\x80@example.test>")
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, "invalid-\ufffd@example.test", got)
}

func TestNormalizeLegacyMessageID(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{"bracketed", "<Local@EXAMPLE.TEST>", "Local@EXAMPLE.TEST"},
		{"missing close", "<Local@EXAMPLE.TEST", "Local@EXAMPLE.TEST"},
		{"trailing parameters", `<Local@EXAMPLE.TEST> type="multipart/alternative"`, "Local@EXAMPLE.TEST"},
		{"leading text", "prefix <Local@example.test> type=alternative", ""},
		{"bare", "Local@EXAMPLE.TEST", "Local@EXAMPLE.TEST"},
		{"bare trailing text", "legacy-token type=alternative", ""},
		{"colon", "legacy:token", "legacy:token"},
		{"square", "<[legacy-token==@example.test]>", "[legacy-token==@example.test]"},
		{"outer whitespace", " \t<Local@example.test\r\n", "Local@example.test"},
		{"invalid UTF8", "<invalid-\x80@example.test", "invalid-\ufffd@example.test"},
		{"empty", "", ""},
		{"empty pair", "<>", ""},
		{"empty unclosed", "<", ""},
		{"nested", "<<Local@example.test>>", ""},
		{"nested unclosed", "<<Local@example.test", ""},
		{"extra close", "<Local@example.test>>", ""},
		{"missing open", "Local@example.test>", ""},
		{"multiple", "<one@example.test> <two@example.test>", ""},
		{"internal space", "<Local @example.test>", ""},
		{"opening space", "< Local@example.test>", ""},
		{"closing space", "<Local@example.test >", ""},
		{"unclosed parameters", "<Local@example.test type=alternative", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, NormalizeLegacyMessageID(test.input))
		})
	}
}

func TestParseThreadingHeaders(t *testing.T) {
	raw := []byte("Message-ID: <child@example.test>\r\nIn-Reply-To: <parent@example.test>\r\nReferences: <root@example.test>\r\n\t<parent@example.test>\r\n\r\nReferences: <body@example.test>\r\n")
	id, parent, refs := ParseThreadingHeaders(raw)
	assert.Equal(t, "child@example.test", id)
	assert.Equal(t, "parent@example.test", parent)
	assert.Equal(t, []string{"root@example.test", "parent@example.test"}, refs)
}

func TestReplyParentID(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"empty", "", ""},
		{"bracketed", "<parent@example.test>", "parent@example.test"},
		{"bare", "parent@example.test", "parent@example.test"},
		{"padded", "  <parent@example.test>\t", "parent@example.test"},
		{"list uses first", "<first@example.test> <second@example.test>", "first@example.test"},
		{"trailing comment", "<parent@example.test> (Sender's message of Mon)", "parent@example.test"},
		{"leading text", "Sender's message <parent@example.test>", "parent@example.test"},
		{"skips empty entry", "<> <parent@example.test>", "parent@example.test"},
		{"skips malformed entry", "< bad@example.test> <parent@example.test>", "parent@example.test"},
		{"unterminated", "<parent@example.test", ""},
		{"NUL", "<par\x00ent@example.test>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ReplyParentID(tc.input))
		})
	}
}

func TestReplyParentFromHeaders(t *testing.T) {
	raw := []byte("Message-ID: <child@example.test>\r\nIn-Reply-To: <first@example.test>\r\n\t<second@example.test>\r\n\r\nIn-Reply-To: <body@example.test>\r\n")
	assert.Equal(t, "first@example.test", ReplyParentFromHeaders(raw))
	assert.Empty(t, ReplyParentFromHeaders([]byte("Subject: no parent\r\n\r\nIn-Reply-To: <body@example.test>\r\n")))
}
