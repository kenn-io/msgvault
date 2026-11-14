package draftlint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, code, severity string
	}{
		{name: "placeholder", body: "Hi FIRST_NAME,\n\nThanks", code: CodeUnresolvedPlaceholder, severity: "error"},
		{name: "double brace", body: "Hi {{name}}", code: CodeUnresolvedPlaceholder, severity: "error"},
		{name: "angle", body: "see <URL>", code: CodeUnresolvedPlaceholder, severity: "warn"},
		{name: "single letter angle", body: "Use <A> or List<T>"},
		{name: "brace", body: "use {id}", code: CodeUnresolvedPlaceholder, severity: "warn"},
		{name: "literal braces", body: "use {1,2}"},
		{name: "empty", body: " \n", code: CodeEmptyBody, severity: "error"},
		{name: "forward empty", kind: "forward"},
		{name: "clean", body: "Thanks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(Input{Kind: tc.kind, Channel: "chat", Body: tc.body})
			if tc.code == "" {
				assert.Nil(t, r.Warnings)
				return
			}
			require.Len(t, r.Warnings, 1)
			assert.Equal(t, tc.code, r.Warnings[0].Code)
			assert.Equal(t, tc.severity, r.Warnings[0].Severity)
		})
	}
}

func TestCheckSpansCountRunes(t *testing.T) {
	r := Check(Input{Body: "😂 привет FIRST_NAME and {{name}}"})
	require.Len(t, r.Warnings, 2)
	assert.Equal(t, &Span{9, 19}, r.Warnings[0].Span)
	assert.Equal(t, &Span{24, 32}, r.Warnings[1].Span)
}

func TestCheckIgnoresQuotedPlaceholders(t *testing.T) {
	r := Check(Input{Channel: "email", Body: "Thanks\n> Hello FIRST_NAME\n"})
	assert.Empty(t, r.Warnings)
}
