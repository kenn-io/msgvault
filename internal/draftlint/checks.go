package draftlint

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/sensitive"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

var placeholder = regexp.MustCompile(`\{\{[\pL_][\pL\pN_]*\}\}|\{[\pL_][\pL\pN_]*\}|\[(?:TODO|TBD|NAME|(?i:insert)[^\]\r\n]*)\]|<[A-Z][A-Z_]+>|\b(?:FIRST_NAME|LAST_NAME|COMPANY_NAME|XXX)\b|\bTODO:`)
var attachment = regexp.MustCompile(`(?i)\b(?:attached|attaching|enclosed|see attachment|find attached|PFA)\b`)

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

// runeCursor counts only the new bytes in an ordered scan of the original field.
type runeCursor struct{ byteOffset, runes int }

func (c *runeCursor) position(original string, offset int) int {
	c.runes += utf8.RuneCountInString(original[c.byteOffset:offset])
	c.byteOffset = offset
	return c.runes
}

func textWarning(code, severity, field, message, original, masked string, start, end int, cursor *runeCursor) Warning {
	w := Warning{Code: code, Severity: severity, Field: field, Message: message}
	w.Span = &Span{cursor.position(original, start), cursor.position(original, end)}
	windowStart := start
	for range 20 {
		if windowStart == 0 || original[windowStart-1] == '\n' {
			break
		}
		_, size := utf8.DecodeLastRuneInString(original[:windowStart])
		windowStart -= size
	}
	windowEnd := windowStart
	for range 80 {
		if windowEnd == len(original) || original[windowEnd] == '\n' {
			break
		}
		_, size := utf8.DecodeRuneInString(original[windowEnd:])
		windowEnd += size
	}
	w.Excerpt = string([]rune(masked[windowStart:windowEnd]))
	return w
}
func Check(in Input) Result {
	var r Result
	add := func(code, severity, field, message string) {
		r.Warnings = append(r.Warnings, Warning{Code: code, Severity: severity, Field: field, Message: message})
	}
	own := in.Body
	if in.Channel == "email" {
		own = preprocess.MaskReplyQuotes(in.Body)
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
		masked := []byte(field.text)
		for _, finding := range sensitive.Scan(field.text) {
			for i := finding.Start; i < finding.End; i++ {
				masked[i] = '*'
			}
		}
		maskedText := string(masked)
		severity := "error"
		if in.AllowPlaceholders {
			severity = "warn"
		}
		var placeholderCursor, controlCursor runeCursor
		for _, loc := range placeholder.FindAllStringIndex(field.scan, -1) {
			matchSeverity := severity
			match := field.scan[loc[0]:loc[1]]
			if strings.HasPrefix(match, "<") || strings.HasPrefix(match, "{") && !strings.HasPrefix(match, "{{") {
				matchSeverity = "warn"
			}
			w := textWarning(CodeUnresolvedPlaceholder, matchSeverity, field.name, "The text contains a template placeholder.", field.text, maskedText, loc[0], loc[1], &placeholderCursor)
			w.Hint = "Replace the placeholder, or pass --allow-placeholders."
			r.Warnings = append(r.Warnings, w)
		}
		for i, c := range field.text {
			if c < 32 && c != '\t' && c != '\n' && c != '\r' || c == unicode.ReplacementChar {
				_, size := utf8.DecodeRuneInString(field.text[i:])
				r.Warnings = append(r.Warnings, textWarning(CodeControlCharacters, "error", field.name, "The text contains a control or replacement character.", field.text, maskedText, i, i+size, &controlCursor))
			}
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
	return r
}
