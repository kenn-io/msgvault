package draftlint

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgsegment"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body, channel, kind, code, severity, field string
		to, cc, bcc                                      []string
		attachments, size                                int
		allow                                            bool
	}{
		{channel: "chat", name: "placeholder", body: "Hi FIRST_NAME,\n\nThanks", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{channel: "chat", name: "angle placeholder", body: "Hello <FIRST_NAME>", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{channel: "chat", name: "two letter angle placeholder", body: "Hello <AB>", code: CodeUnresolvedPlaceholder, severity: "error", field: "body"},
		{channel: "chat", name: "single letter angle", body: "Use <A> or List<T>"},
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

func TestCheckManyFindingSpans(t *testing.T) {
	body := "界\xff\a " + strings.Repeat("FIRST_NAME 界 ", 3) + "\a"
	result := Check(t.Context(), Input{Channel: "chat", Body: body}, nil)
	require.Len(t, result.Warnings, 6)
	for i, warning := range result.Warnings[:3] {
		assert.Equal(t, &Span{4 + 13*i, 14 + 13*i}, warning.Span)
		assert.LessOrEqual(t, len([]rune(warning.Excerpt)), 80)
	}
	for i, span := range []Span{{1, 2}, {2, 3}, {43, 44}} {
		assert.Equal(t, &span, result.Warnings[3+i].Span)
	}
}

type nameResolver []Name

func (n nameResolver) CuratedNames(context.Context, []string) ([]Name, error) { return n, nil }

func TestGreetingNameMismatch(t *testing.T) {
	for _, tc := range []struct {
		body       string
		names      NameResolver
		warn, skip bool
	}{
		{"Hi Sam,\nThanks", nameResolver{{Text: "Alex", Given: true}, {Text: "Al"}}, true, false},
		{"hey al,", nameResolver{{Text: "Alex", Given: true}}, false, false},
		{"Hi Bu,", nameResolver{{Text: "Buddy"}}, true, false},
		{"Hi Buddy,", nameResolver{{Text: "Buddy"}}, false, false},
		{"Hi A,", nameResolver{{Text: "Alex", Given: true}}, true, false},
		{"Hi Ål,", nameResolver{{Text: "Åland", Given: true}}, false, false},
		{"Hi O’Brien,", nameResolver{{Text: "O’Brien", Given: true}}, false, false},
		{"Hi Jose\u0301,", nameResolver{{Text: "JOSÉ", Given: true}}, false, false},
		{"Hi JOSÉ,", nameResolver{{Text: "Jose\u0301", Given: true}}, false, false},
		{"Hi Jose\u0301,", nameResolver{{Text: "Alex", Given: true}}, true, false},
		{"Hi Sam,", nameResolver{}, false, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			r := Check(t.Context(), Input{Channel: "email", Body: tc.body, To: []string{"alex@example.com"}}, tc.names)
			if tc.warn {
				require.Len(t, r.Warnings, 1)
				assert.Equal(t, CodeGreetingNameMismatch, r.Warnings[0].Code)
				assert.Equal(t, "warn", r.Warnings[0].Severity)
				assert.Equal(t, &Span{3, 3 + len([]rune(strings.TrimSuffix(strings.Fields(tc.body)[1], ",")))}, r.Warnings[0].Span)
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

func TestCheckUnexaminedQuoteTail(t *testing.T) {
	body := strings.Repeat("> old text\n", msgsegment.MaxBodyBytes/11+1) + "FIRST_NAME\n"
	got := Check(t.Context(), Input{Channel: "email", Kind: "reply", Body: body, To: []string{"alex@example.com"}}, nil)
	require.Len(t, got.Warnings, 1)
	assert.Equal(t, CodeUnresolvedPlaceholder, got.Warnings[0].Code)
	assert.True(t, got.Blocking())
	require.NotNil(t, got.Warnings[0].Span)
	assert.Equal(t, "FIRST_NAME", string([]rune(body)[got.Warnings[0].Span.Start:got.Warnings[0].Span.End]))
}
func TestGreetingApplicableSkips(t *testing.T) {
	for _, channel := range []string{"email", "chat"} {
		bodies := []string{"Hi Alex,"}
		if channel == "email" {
			bodies = []string{"Thanks", "Hi all,", "Hi everyone,", "Hi everybody,", "Hi team,", "Hi there,", "Hi folks,", "Hi Alex,"}
		}
		for _, body := range bodies {
			t.Run(channel+"/"+body, func(t *testing.T) {
				got := Check(t.Context(), Input{Channel: channel, Body: body, To: []string{"alex@example.com"}}, nil)
				assert.Empty(t, got.Warnings)
				if body == "Hi Alex," {
					assert.Equal(t, []string{CodeGreetingNameMismatch}, got.Skipped)
				} else {
					assert.Empty(t, got.Skipped)
				}
			})
		}
	}
}
