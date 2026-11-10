package draftlint

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, channel, kind, code, severity, field, address string
		to, cc, bcc                                               []string
		attachments, size                                         int
		allow                                                     bool
	}{
		{channel: "chat", name: "placeholder", body: "Hi FIRST_NAME,\n\nThanks", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{channel: "chat", name: "allowed", body: "Hi FIRST_NAME", code: CodeUnresolvedPlaceholder, severity: "warn", allow: true},
		{name: "attachment", body: "See attached.", channel: "email", to: []string{"a@example.com"}, code: CodeAttachmentMentionedMissing, severity: "warn"},
		{name: "attached", body: "See attached.", channel: "email", to: []string{"a@example.com"}, attachments: 1},
		{name: "quoted", body: "> old\n> text\n", channel: "email", kind: "reply", to: []string{"a@example.com"}, code: CodeQuotedOnlyBody, severity: "error"},
		{channel: "chat", name: "empty", body: " \n", code: CodeEmptyBody, severity: "error"},
		{name: "no recipients", body: "hello", channel: "email", code: CodeEmptyRecipients, severity: "error"},
		{name: "bad address", body: "hello", channel: "email", to: []string{"not an address"}, code: CodeInvalidAddress, severity: "error", field: "to", address: "not an address"},
		{channel: "chat", name: "invalid UTF8", body: "a\xff", code: CodeControlCharacters, severity: "error", field: "body"},
		{channel: "chat", name: "control", body: "a\x07b", code: CodeControlCharacters, severity: "error", field: "body"},
		{name: "subject", body: "hello", channel: "email", kind: "compose", to: []string{"a@example.com"}, code: CodeEmptySubject, severity: "warn"},
		{name: "bcc only", body: "hello", channel: "email", bcc: []string{"a@example.com"}, code: CodeEmptyTo, severity: "warn"},
		{name: "large", body: "hello", channel: "email", to: []string{"a@example.com"}, size: 500*1024 + 1, code: CodeOversize, severity: "warn"},
		{name: "chat attachment", body: "see attached", channel: "chat"},
		{name: "forward empty", channel: "email", kind: "forward", to: []string{"a@example.com"}},
		{name: "fenced placeholder", channel: "email", body: "Try this:\n```\nfmt.Printf(\"{x}\")\n```", to: []string{"a@example.com"}},
		{name: "quoted attachment", channel: "email", body: "Sure.\n> see attached", to: []string{"a@example.com"}},
		{name: "literal braces", channel: "email", body: "use {1,2}", to: []string{"a@example.com"}},
		{name: "fenced body", channel: "email", body: "```\n{x}\n```", to: []string{"a@example.com"}},
		{name: "bottom posted reply", channel: "email", kind: "reply", body: "On Mon, A wrote:\n> q\n\nyes, thursday", to: []string{"a@example.com"}},
		{name: "clean", channel: "chat", body: "Thanks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(t.Context(), Input{Kind: tc.kind, Channel: tc.channel, Body: tc.body, To: tc.to, Cc: tc.cc, Bcc: tc.bcc, AttachmentCount: tc.attachments, BuiltSize: tc.size, AllowPlaceholders: tc.allow}, nil)
			if tc.code == "" {
				assert.Nil(t, r.Warnings)
				return
			}
			require.Len(t, r.Warnings, 1)
			w := r.Warnings[0]
			assert.Equal(t, tc.code, w.Code)
			assert.Equal(t, tc.severity, w.Severity)
			if tc.field != "" {
				assert.Equal(t, tc.field, w.Field)
			}
			assert.Equal(t, tc.address, w.Address)
		})
	}
}

func TestCheckSpanAndExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name, body, excerpt string
		span                Span
	}{
		{"unicode", "😂 привет {{name}}", "😂 привет {{name}}", Span{9, 17}},
		{"late finding", strings.Repeat("界", 100) + " {{name}} trailing", "", Span{101, 109}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Check(t.Context(), Input{Channel: "chat", Body: tc.body}, nil)
			require.Len(t, result.Warnings, 1)
			warning := result.Warnings[0]
			require.NotNil(t, warning.Span)
			assert.Equal(t, &tc.span, warning.Span)
			assert.Equal(t, "{{name}}", string([]rune(tc.body)[warning.Span.Start:warning.Span.End]))
			assert.Contains(t, warning.Excerpt, "{{name}}")
			assert.LessOrEqual(t, len([]rune(warning.Excerpt)), 80)
			if tc.excerpt != "" {
				assert.Equal(t, tc.excerpt, warning.Excerpt)
			}
		})
	}
}

type nameResolver map[string][]string

func (n nameResolver) CuratedNames(context.Context, []string, []int64) (map[string][]string, error) {
	return n, nil
}
func TestGreetingNameMismatch(t *testing.T) {
	for _, tc := range []struct {
		body       string
		names      NameResolver
		warn, skip bool
	}{
		{"Hi Sam,\nThanks", nameResolver{"alex@example.com": {"Alex", "Al"}}, true, false},
		{"hey al,", nameResolver{"alex@example.com": {"Alex"}}, false, false},
		{"Hi team,", nameResolver{}, false, true}, {"Hi Alex,", nil, false, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r := Check(t.Context(), Input{Channel: "email", Body: tc.body, To: []string{"alex@example.com"}}, tc.names)
			if tc.warn {
				require.Len(t, r.Warnings, 1)
				assert.Equal(t, CodeGreetingNameMismatch, r.Warnings[0].Code)
				assert.Equal(t, "warn", r.Warnings[0].Severity)
				assert.Equal(t, &Span{3, 6}, r.Warnings[0].Span)
			} else {
				assert.Empty(t, r.Warnings)
			}
			assert.Equal(t, tc.skip, len(r.Skipped) > 0)
		})
	}
}
func TestCheckSensitiveContentMasked(t *testing.T) {
	r := Check(t.Context(), Input{Channel: "email", To: []string{"a@example.com"}, Body: "ok\n> password: abcdef123456"}, nil)
	require.Len(t, r.Warnings, 1)
	assert.Equal(t, CodeSensitiveContent, r.Warnings[0].Code)
	assert.NotContains(t, r.Warnings[0].Excerpt, "abcdef123456")
	assert.Contains(t, r.Warnings[0].Message, "in quoted text")
}

func TestCheckCallerAuthoredSubject(t *testing.T) {
	for _, kind := range []string{"compose", "reply", "forward", "edit"} {
		t.Run(kind, func(t *testing.T) {
			got := Check(t.Context(), Input{Kind: kind, Channel: "email", Subject: "TODO: x\a password: abcdef123456", Body: "Thanks", To: []string{"alex@example.com"}}, nil)
			if kind == "compose" {
				assert.True(t, got.Blocking())
				require.Len(t, got.Warnings, 3)
				var codes []string
				for _, warning := range got.Warnings {
					codes = append(codes, warning.Code)
					assert.Equal(t, "subject", warning.Field)
				}
				assert.ElementsMatch(t, []string{CodeUnresolvedPlaceholder, CodeControlCharacters, CodeSensitiveContent}, codes)
			} else {
				assert.Empty(t, got.Warnings)
			}
		})
	}
}
