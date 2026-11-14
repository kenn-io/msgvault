package draftlint

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, channel, kind, code, severity, field string
		to, cc, bcc                                      []string
		attachments, size                                int
		allow                                            bool
	}{
		{channel: "chat", name: "placeholder", body: "Hi FIRST_NAME,\n\nThanks", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{channel: "chat", name: "angle placeholder", body: "Hello <URL>", code: CodeUnresolvedPlaceholder, severity: "warn", field: "body"},
		{channel: "chat", name: "single letter angle", body: "Use <A> or List<T>"},
		{channel: "chat", name: "brace", body: "Use {id}", code: CodeUnresolvedPlaceholder, severity: "warn"},
		{channel: "chat", name: "todo", body: "[TODO]", code: CodeUnresolvedPlaceholder, severity: "error"},
		{channel: "chat", name: "allowed", body: "Hi FIRST_NAME", code: CodeUnresolvedPlaceholder, severity: "warn", allow: true},
		{name: "attachment", body: "See attached.", channel: "email", to: []string{"a@example.com"}, code: CodeAttachmentMentionedMissing, severity: "warn"},
		{name: "attached", body: "See attached.", channel: "email", to: []string{"a@example.com"}, attachments: 1},
		{name: "quoted", body: "> old\n> text\n", channel: "email", kind: "reply", to: []string{"a@example.com"}, code: CodeQuotedOnlyBody, severity: "error"},
		{channel: "chat", name: "empty", body: " \n", code: CodeEmptyBody, severity: "error"},
		{channel: "chat", name: "invalid UTF8", body: "a\xff", code: CodeControlCharacters, severity: "error", field: "body"},
		{channel: "chat", name: "control", body: "a\x07b", code: CodeControlCharacters, severity: "error", field: "body"},
		{name: "subject", body: "hello", channel: "email", kind: "compose", to: []string{"a@example.com"}, code: CodeEmptySubject, severity: "warn"},
		{name: "bcc only", body: "hello", channel: "email", bcc: []string{"a@example.com"}, code: CodeEmptyTo, severity: "warn"},
		{name: "large", body: "hello", channel: "email", to: []string{"a@example.com"}, size: 500*1024 + 1, code: CodeOversize, severity: "warn"},
		{name: "chat attachment", body: "see attached", channel: "chat"},
		{name: "forward empty", channel: "email", kind: "forward", to: []string{"a@example.com"}},
		{name: "quoted attachment", channel: "email", body: "Sure.\n> see attached", to: []string{"a@example.com"}},
		{name: "literal braces", channel: "email", body: "use {1,2}", to: []string{"a@example.com"}},
		{name: "fenced body", channel: "email", body: "```\n{x}\n```", to: []string{"a@example.com"}},
		{name: "bottom posted reply", channel: "email", kind: "reply", body: "On Mon, A wrote:\n> q\n\nyes, thursday", to: []string{"a@example.com"}},
		{name: "clean", channel: "chat", body: "Thanks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			r := Check(Input{Kind: tc.kind, Channel: tc.channel, Body: tc.body, To: tc.to, Cc: tc.cc, Bcc: tc.bcc, AttachmentCount: tc.attachments, BuiltSize: tc.size, AllowPlaceholders: tc.allow})
			if tc.code == "" {
				assert.Nil(r.Warnings)
				return
			}
			require.Len(t, r.Warnings, 1)
			w := r.Warnings[0]
			assert.Equal(tc.code, w.Code)
			assert.Equal(tc.severity, w.Severity)
			if tc.field != "" {
				assert.Equal(tc.field, w.Field)
			}
		})
	}
}

func TestCheckSpanAndExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name, body, excerpt string
		spans               []Span
		tokens              []string
	}{
		{"unicode", "😂 привет {{name}}", "😂 привет {{name}}", []Span{{9, 17}}, []string{"{{name}}"}},
		{"late finding", strings.Repeat("界", 100) + " {{name}} trailing", "", []Span{{101, 109}}, []string{"{{name}}"}},
		{"monotonic cursors", "界\xff\a " + strings.Repeat("FIRST_NAME 界 ", 3) + "\a", "", []Span{{4, 14}, {17, 27}, {30, 40}, {1, 2}, {2, 3}, {43, 44}}, []string{"FIRST_NAME", "FIRST_NAME", "FIRST_NAME"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			result := Check(Input{Channel: "chat", Body: tc.body})
			require.Len(result.Warnings, len(tc.spans))
			for i, warning := range result.Warnings {
				require.NotNil(warning.Span)
				assert.Equal(&tc.spans[i], warning.Span)
				if i < len(tc.tokens) {
					assert.Equal(tc.tokens[i], string([]rune(tc.body)[warning.Span.Start:warning.Span.End]))
					assert.Contains(warning.Excerpt, tc.tokens[i])
					assert.LessOrEqual(len([]rune(warning.Excerpt)), 80)
				}
				if tc.excerpt != "" {
					assert.Equal(tc.excerpt, warning.Excerpt)
				}
			}
		})
	}
}

func TestCheckCallerAuthoredSubject(t *testing.T) {
	for _, kind := range []string{"compose", "reply"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)

			got := Check(Input{Kind: kind, Channel: "email", Subject: "TODO: x\a password: abcdef123456", Body: "Thanks", To: []string{"alex@example.com"}})
			if kind == "compose" {
				assert.True(got.Blocking())
				require.Len(t, got.Warnings, 2)
				var codes []string
				for _, warning := range got.Warnings {
					codes = append(codes, warning.Code)
					assert.Equal("subject", warning.Field)
				}
				assert.ElementsMatch([]string{CodeUnresolvedPlaceholder, CodeControlCharacters}, codes)
			} else {
				assert.Empty(got.Warnings)
			}
		})
	}
}

func TestCheckRedactsSensitiveExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"body", Input{Channel: "chat", Body: "Hello FIRST_NAME, card 4111111111111111"}},
		{"subject", Input{Channel: "email", Kind: "compose", Subject: "TODO: card 4111111111111111", Body: "Thanks", To: []string{"a@example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			result := Check(tc.in)
			require.Len(t, result.Warnings, 1)
			assert.Equal(CodeUnresolvedPlaceholder, result.Warnings[0].Code)
			assert.NotContains(result.Warnings[0].Excerpt, "4111111111111111")
			assert.Contains(result.Warnings[0].Excerpt, "****************")
		})
	}
}
