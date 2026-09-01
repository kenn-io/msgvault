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
