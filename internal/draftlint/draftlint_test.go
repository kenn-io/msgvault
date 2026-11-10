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
		name, body, channel, kind, code, severity, field string
		to, cc, bcc                                      []string
		attachments, size                                int
		allow                                            bool
	}{
		{name: "placeholder", body: "Hi FIRST_NAME,\n\nThanks", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{name: "allowed", body: "Hi FIRST_NAME", code: CodeUnresolvedPlaceholder, severity: "warn", allow: true},
		{name: "attachment", body: "See attached.", channel: "email", to: []string{"a@example.com"}, code: CodeAttachmentMentionedMissing, severity: "warn"},
		{name: "attached", body: "See attached.", channel: "email", to: []string{"a@example.com"}, attachments: 1},
		{name: "quoted", body: "> old\n> text\n", channel: "email", kind: "reply", to: []string{"a@example.com"}, code: CodeQuotedOnlyBody, severity: "error"},
		{name: "empty", body: " \n", code: CodeEmptyBody, severity: "error"},
		{name: "no recipients", body: "hello", channel: "email", code: CodeEmptyRecipients, severity: "error"},
		{name: "bad address", body: "hello", channel: "email", to: []string{"not an address"}, code: CodeInvalidAddress, severity: "error", field: "to"},
		{name: "invalid UTF8", body: "a\xff", code: CodeControlCharacters, severity: "error", field: "body"},
		{name: "control", body: "a\x07b", code: CodeControlCharacters, severity: "error", field: "body"},
		{name: "subject", body: "hello", channel: "email", kind: "compose", to: []string{"a@example.com"}, code: CodeEmptySubject, severity: "warn"},
		{name: "bcc only", body: "hello", channel: "email", bcc: []string{"a@example.com"}, code: CodeEmptyTo, severity: "warn"},
		{name: "sms", body: strings.Repeat("a", 481), channel: "sms", code: CodeSMSLength, severity: "warn"},
		{name: "three sms", body: strings.Repeat("a", 459), channel: "sms"},
		{name: "large", body: "hello", channel: "email", to: []string{"a@example.com"}, size: 500*1024 + 1, code: CodeOversize, severity: "warn"},
		{name: "chat attachment", body: "see attached", channel: "chat"},
		{name: "forward empty", channel: "email", kind: "forward", to: []string{"a@example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(t.Context(), Input{Kind: tc.kind, Channel: tc.channel, Body: tc.body, To: tc.to, Cc: tc.cc, Bcc: tc.bcc, AttachmentCount: tc.attachments, BuiltSize: tc.size, AllowPlaceholders: tc.allow}, nil)
			if tc.code == "" {
				assert.Empty(t, r.Warnings)
				return
			}
			require.Len(t, r.Warnings, 1)
			w := r.Warnings[0]
			assert.Equal(t, tc.code, w.Code)
			assert.Equal(t, tc.severity, w.Severity)
			if tc.field != "" {
				assert.Equal(t, tc.field, w.Field)
			}
			if tc.name == "placeholder" {
				assert.Equal(t, &Span{3, 13}, w.Span)
				assert.Equal(t, "Hi FIRST_NAME,", w.Excerpt)
			}
			if tc.name == "bad address" {
				assert.Equal(t, "not an address", w.Address)
			}
		})
	}
}
func TestCheckIgnoresFencedAndQuoted(t *testing.T) {
	for _, body := range []string{"Try this:\n```\nfmt.Printf(\"{x}\")\n```", "Sure.\n> see attached", "use {1,2}", "```\n{x}\n```"} {
		r := Check(t.Context(), Input{Body: body, Channel: "email", To: []string{"a@example.com"}}, nil)
		assert.Empty(t, r.Warnings)
	}
}
func TestCheckBottomPostedReply(t *testing.T) {
	r := Check(t.Context(), Input{Kind: "reply", Channel: "email", Body: "On Mon, A wrote:\n> q\n\nyes, thursday", To: []string{"a@example.com"}}, nil)
	assert.Empty(t, r.Warnings)
}
func TestCheckSpanCodePoints(t *testing.T) {
	body := "😂 привет {{name}}"
	r := Check(t.Context(), Input{Body: body}, nil)
	require.Len(t, r.Warnings, 1)
	assert.Equal(t, &Span{9, 17}, r.Warnings[0].Span)
	assert.Equal(t, "{{name}}", string([]rune(body)[r.Warnings[0].Span.Start:r.Warnings[0].Span.End]))
}
func TestCheckCleanReturnsNil(t *testing.T) {
	assert.Nil(t, Check(t.Context(), Input{Body: "Thanks"}, nil).Warnings)
}

type nameResolver map[string][]string

func (n nameResolver) CuratedNames(context.Context, []string) (map[string][]string, error) {
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
			r := Check(t.Context(), Input{Body: tc.body, To: []string{"alex@example.com"}}, tc.names)
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
func TestCheckSMSUnits(t *testing.T) {
	for _, tc := range []struct {
		body string
		warn bool
	}{{strings.Repeat("^", 230), true}, {strings.Repeat("Ж", 201), false}, {strings.Repeat("😂", 101), true}} {
		r := Check(t.Context(), Input{Body: tc.body, Channel: "sms"}, nil)
		assert.Equal(t, tc.warn, len(r.Warnings) > 0)
	}
}
