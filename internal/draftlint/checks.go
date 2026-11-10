package draftlint

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/sensitive"
)

var placeholder = regexp.MustCompile(`\{\{[\pL_][\pL\pN_]*\}\}|\{[\pL_][\pL\pN_]*\}|\[(?:TODO|TBD|NAME|(?i:insert)[^\]\r\n]*)\]|<[A-Z][A-Z_]*>|\b(?:FIRST_NAME|LAST_NAME|COMPANY_NAME|XXX)\b|\bTODO:`)
var attachment = regexp.MustCompile(`(?i)\b(?:attached|attaching|enclosed|see attachment|find attached|PFA)\b`)
var greeting = regexp.MustCompile(`(?i)^(?:hi|hey|hello|dear|hiya|morning|good morning|good afternoon|yo)[ \t]+([\pL][\pL\pM'’-]*)`)

var genericGreeting = regexp.MustCompile(`(?i)^(?:all|everyone|everybody|team|there|folks)$`)

// maskQuotes blanks bytes rather than deleting them: all match offsets remain
// offsets into the submitted text, even when quoted text contains Unicode.
func maskQuotes(body string) string {
	masked := []byte(body)
	result := msgsegment.Split(body)
	limit := len(body)
	if result.Truncated {
		// Only the complete-line prefix was analyzed; scan the tail as authored.
		limit = strings.LastIndexByte(body[:msgsegment.MaxBodyBytes], '\n') + 1
	}
	for _, seg := range result.Segments {
		if seg.Kind == msgsegment.Quoted || seg.Kind == msgsegment.Forwarded {
			blank(masked, min(seg.Start, limit), min(seg.End, limit))
		}
	}
	return string(masked)
}
func blank(b []byte, start, end int) {
	for i := start; i < end; i++ {
		if b[i] != '\r' && b[i] != '\n' {
			b[i] = ' '
		}
	}
}
func maskFences(body string) string {
	b := []byte(body)
	offset := 0
	fenced := false
	for _, line := range strings.SplitAfter(body, "\n") {
		marker := strings.HasPrefix(strings.TrimSpace(line), "```")
		if marker || fenced {
			blank(b, offset, offset+len(line))
		}
		if marker {
			fenced = !fenced
		}
		offset += len(line)
	}
	return string(b)
}
func textWarning(code, severity, field, message, original string, start, end int) Warning {
	w := Warning{Code: code, Severity: severity, Field: field, Message: message}
	w.Span = &Span{utf8.RuneCountInString(original[:start]), utf8.RuneCountInString(original[:end])}
	lineStart := strings.LastIndexByte(original[:start], '\n') + 1
	lineEnd := len(original)
	if i := strings.IndexByte(original[end:], '\n'); i >= 0 {
		lineEnd = end + i
	}
	line := []rune(original[lineStart:lineEnd])
	offset := utf8.RuneCountInString(original[lineStart:start])
	windowStart := max(0, offset-20)
	w.Excerpt = string(line[windowStart:min(windowStart+80, len(line))])
	return w
}
func Check(ctx context.Context, in Input, names NameResolver) Result {
	var r Result
	add := func(code, severity, field, message string) {
		r.Warnings = append(r.Warnings, Warning{Code: code, Severity: severity, Field: field, Message: message})
	}
	own := in.Body
	if in.Channel == "email" {
		own = maskQuotes(in.Body)
	}
	if strings.TrimSpace(own) == "" && in.Kind != "forward" {
		code := CodeEmptyBody
		if in.Channel == "email" && strings.TrimSpace(in.Body) != "" {
			code = CodeQuotedOnlyBody
		}
		add(code, "error", "body", "The draft has no authored body text.")
	}
	for _, field := range []struct{ name, text, scan string }{{"subject", in.Subject, in.Subject}, {"body", in.Body, maskFences(own)}} {
		if field.name == "subject" && in.Kind != "compose" {
			continue
		}
		severity := "error"
		if in.AllowPlaceholders {
			severity = "warn"
		}
		for _, loc := range placeholder.FindAllStringIndex(field.scan, -1) {
			w := textWarning(CodeUnresolvedPlaceholder, severity, field.name, "The text contains a template placeholder.", field.text, loc[0], loc[1])
			w.Hint = "Replace the placeholder, or pass --allow-placeholders."
			r.Warnings = append(r.Warnings, w)
		}
		for i, c := range field.text {
			if c < 32 && c != '\t' && c != '\n' && c != '\r' || c == unicode.ReplacementChar {
				_, size := utf8.DecodeRuneInString(field.text[i:])
				r.Warnings = append(r.Warnings, textWarning(CodeControlCharacters, "error", field.name, "The text contains a control or replacement character.", field.text, i, i+size))
			}
		}
		for _, f := range sensitive.Scan(field.text) {
			w := Warning{Code: CodeSensitiveContent, Severity: "warn", Field: field.name, Flag: f.Kind, Excerpt: f.Hint, Message: "The text may contain sensitive content."}
			if field.name == "body" && strings.TrimSpace(own[f.Start:f.End]) == "" {
				w.Message += " (in quoted text)"
			}
			r.Warnings = append(r.Warnings, w)
		}
	}
	if in.Channel == "email" {
		if len(in.To) == 0 && len(in.Cc)+len(in.Bcc) > 0 {
			add(CodeEmptyTo, "warn", "to", "The email has no To recipient.")
		}
		if in.AttachmentCount == 0 && attachment.MatchString(maskFences(own)) {
			add(CodeAttachmentMentionedMissing, "warn", "attachments", "The text mentions an attachment, but none is attached.")
		}
		if in.Kind == "compose" && strings.TrimSpace(in.Subject) == "" {
			add(CodeEmptySubject, "warn", "subject", "The email has no subject.")
		}
		if in.BuiltSize > 500*1024 {
			add(CodeOversize, "warn", "body", "The built email exceeds 500 KB.")
		}
	}
	checkGreeting(ctx, in, names, own, &r)
	return r
}
func checkGreeting(ctx context.Context, in Input, names NameResolver, own string, r *Result) {
	start := 0
	for _, line := range strings.SplitAfter(own, "\n") {
		if strings.TrimSpace(line) == "" {
			start += len(line)
			continue
		}
		leading := len(line) - len(strings.TrimLeftFunc(line, unicode.IsSpace))
		loc := greeting.FindStringSubmatchIndex(line[leading:])
		if loc == nil {
			return
		}
		start += leading
		candidate := strings.ToLower(norm.NFC.String(line[leading+loc[2] : leading+loc[3]]))
		if genericGreeting.MatchString(candidate) {
			return
		}
		if names == nil {
			r.Skipped = append(r.Skipped, CodeGreetingNameMismatch)
			return
		}
		addresses := append(append([]string{}, in.To...), in.Cc...)
		resolved, err := names.CuratedNames(ctx, addresses)
		if err != nil || len(resolved) == 0 {
			r.Skipped = append(r.Skipped, CodeGreetingNameMismatch)
			return
		}
		for _, name := range resolved {
			value := strings.ToLower(norm.NFC.String(name.Text))
			if candidate == value || name.Given && utf8.RuneCountInString(candidate) >= 2 && strings.HasPrefix(value, candidate) {
				return
			}
		}
		r.Warnings = append(r.Warnings, textWarning(CodeGreetingNameMismatch, "warn", "body", "The greeting does not match a curated recipient name.", in.Body, start+loc[2], start+loc[3]))
		return
	}
}
