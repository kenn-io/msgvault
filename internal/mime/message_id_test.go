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
		{"first bracket", "prefix <Local@example.test> type=alternative", "Local@example.test"},
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
