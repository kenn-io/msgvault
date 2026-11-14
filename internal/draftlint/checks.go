package draftlint

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector/preprocess"
)

var placeholder = regexp.MustCompile(`\{\{[\pL_][\pL\pN_]*\}\}|\{[\pL_][\pL\pN_]*\}|\[(?:TODO|TBD|NAME|(?i:insert)[^\]\r\n]*)\]|<[A-Z][A-Z_]+>|\b(?:FIRST_NAME|LAST_NAME|COMPANY_NAME|XXX)\b|\bTODO:`)

func Check(in Input) Result {
	var r Result
	if strings.TrimSpace(in.Body) == "" && in.Kind != "forward" {
		r.Warnings = append(r.Warnings, Warning{Code: CodeEmptyBody, Severity: "error", Message: "The draft has no authored body text."})
	}
	runes := 0
	offset := 0
	scan := in.Body
	if in.Channel == "email" {
		scan = preprocess.MaskReplyQuotes(scan)
	}
	for _, loc := range placeholder.FindAllStringIndex(scan, -1) {
		start := runes + utf8.RuneCountInString(in.Body[offset:loc[0]])
		end := start + utf8.RuneCountInString(in.Body[loc[0]:loc[1]])
		runes, offset = end, loc[1]
		// <URL> and {id} are common in technical text; only clear markers block.
		severity := "error"
		if match := in.Body[loc[0]:loc[1]]; strings.HasPrefix(match, "<") || strings.HasPrefix(match, "{") && !strings.HasPrefix(match, "{{") {
			severity = "warn"
		}
		r.Warnings = append(r.Warnings, Warning{Code: CodeUnresolvedPlaceholder, Severity: severity, Span: &Span{start, end}, Message: "The text contains a template placeholder."})
	}
	return r
}
