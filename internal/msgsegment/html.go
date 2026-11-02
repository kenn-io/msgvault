package msgsegment

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// A separate token cap bounds markup-heavy inputs independently of byte size.
const maxHTMLTokens = 16384

type htmlPart struct {
	start, end int
	typ        html.TokenType
	token      html.Token
	close      int
}

func htmlParts(body string) ([]htmlPart, int) {
	z := html.NewTokenizer(strings.NewReader(body))
	parts := make([]htmlPart, 0)
	end := 0
	stack := make([]int, 0)
	positions := make(map[string][]int)
	for len(parts) < maxHTMLTokens {
		typ := z.Next()
		if typ == html.ErrorToken {
			return parts, len(body)
		}
		p := htmlPart{start: end, end: end + len(z.Raw()), typ: typ, token: z.Token(), close: -1}
		end = p.end
		switch typ {
		case html.StartTagToken:
			if !voidHTML(p.token.Data) {
				positions[p.token.Data] = append(positions[p.token.Data], len(stack))
				stack = append(stack, len(parts))
			}
		case html.EndTagToken:
			if matches := positions[p.token.Data]; len(matches) > 0 {
				j := matches[len(matches)-1]
				for _, index := range stack[j:] {
					// Closing an ancestor implicitly closes every still-open
					// descendant in this token stack as well.
					parts[index].close = len(parts)
					tag := parts[index].token.Data
					positions[tag] = positions[tag][:len(positions[tag])-1]
				}
				stack = stack[:j]
			}
		default:
			// Other tokens do not change the open-tag stack.
		}
		parts = append(parts, p)
	}
	return parts, end
}
func voidHTML(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	}
	return false
}
func htmlAttr(t html.Token, key string) string {
	for _, a := range t.Attr {
		if a.Key == key {
			return strings.ToLower(a.Val)
		}
	}
	return ""
}
func htmlClass(t html.Token, class string) bool {
	for c := range strings.FieldsSeq(htmlAttr(t, "class")) {
		if c == class {
			return true
		}
	}
	return false
}
func structuralHTMLBasis(t html.Token) string {
	if t.Data == "blockquote" && htmlAttr(t, "type") == "cite" {
		return "blockquote_cite"
	}
	if t.Data == "div" || t.Data == "blockquote" {
		for _, c := range []string{"gmail_quote", "gmail_attr", "yahoo_quoted", "protonmail_quote"} {
			if htmlClass(t, c) {
				return c
			}
		}
		if htmlAttr(t, "id") == "mail-editor-reference-message-container" {
			return "reference_container"
		}
	}
	return ""
}
func htmlHeaderLines(parts []htmlPart, start, end int) []line {
	var b strings.Builder
	for j := start; j < end; j++ {
		p := parts[j]
		if p.typ == html.TextToken {
			limit := min(len(p.token.Data), 4096-b.Len())
			for limit > 0 && limit < len(p.token.Data) && !utf8.RuneStart(p.token.Data[limit]) {
				limit--
			}
			b.WriteString(p.token.Data[:limit])
		}
		if p.typ == html.StartTagToken || p.typ == html.EndTagToken || p.typ == html.SelfClosingTagToken {
			switch p.token.Data {
			case "br", "div", "p", "tr":
				b.WriteByte('\n')
			}
		}
		if b.Len() >= 4096 {
			break
		}
	}
	var lines []line
	for _, l := range linesOf(b.String()) {
		if strings.TrimSpace(l.text) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
func htmlHeaderEnd(parts []htmlPart, start, end int) int {
	for j := start; j < end; j++ {
		p := parts[j]
		if p.typ != html.StartTagToken && p.typ != html.SelfClosingTagToken {
			continue
		}
		if p.token.Data == "hr" || p.token.Data == "blockquote" || structuralHTMLBasis(p.token) != "" {
			return j
		}
	}
	return end
}
func htmlHeaderFollows(parts []htmlPart, start, end int) bool {
	end = htmlHeaderEnd(parts, start, end)
	ls := htmlHeaderLines(parts, start, end)
	for i := 0; i < len(ls); {
		if headerBlock(ls, i) {
			return true
		}
		marker := strings.TrimSpace(ls[i].text)
		if !forwardMarker(marker) && !originalMarker(marker) {
			return false
		}
		i++
	}
	return false
}
func htmlHeaderLinesBeforeNestedQuote(parts []htmlPart, start, end int) []line {
	for j := start; j < end; j++ {
		p := parts[j]
		if p.typ != html.StartTagToken && p.typ != html.SelfClosingTagToken {
			continue
		}
		if p.token.Data == "blockquote" {
			end = j
			break
		}
		if basis := structuralHTMLBasis(p.token); basis != "" && basis != "gmail_attr" {
			end = j
			break
		}
		if p.token.Data == "div" {
			id := htmlAttr(p.token, "id")
			if id == "divrplyfwdmsg" || id == "appendonsend" {
				end = j
				break
			}
		}
	}
	return htmlHeaderLines(parts, start, end)
}
func bareHTMLQuote(parts []htmlPart, i int, prior string, lastContent int) bool {
	p := parts[i]
	if p.token.Data != "blockquote" || p.close < 0 || !attributionLine(prior) {
		return false
	}
	return p.close >= lastContent
}
func htmlLineBoundary(p htmlPart) bool {
	if p.typ != html.StartTagToken && p.typ != html.EndTagToken && p.typ != html.SelfClosingTagToken {
		return false
	}
	switch p.token.Data {
	case "br", "div", "p", "tr", "blockquote", "hr", "li", "ul", "ol", "table", "section", "article":
		return true
	}
	return false
}
func flushHTMLLine(current *strings.Builder, previous *string) {
	if text := strings.TrimSpace(current.String()); text != "" {
		*previous = text
	}
	current.Reset()
}
func appendHTMLTextLine(current *strings.Builder, previous *string, text string) {
	for {
		newline := strings.IndexByte(text, '\n')
		if newline < 0 {
			current.WriteString(text)
			return
		}
		current.WriteString(text[:newline])
		flushHTMLLine(current, previous)
		text = text[newline+1:]
	}
}
func splitHTML(r *Result, body string) {
	parts, end := htmlParts(body)
	// quoteUntil tracks the end token of each explicit quote container. An
	// unclosed container extends to the examined end, matching browser mail rendering.
	type context struct {
		until             int
		basis, confidence string
		depth             int
		kind              Kind
		claimed           *Claimed
	}
	var stack []context
	priorText := ""
	var visibleLine strings.Builder
	var outlook []context
	activeContext := func() (context, bool) {
		var current context
		found := false
		if len(stack) > 0 {
			current = stack[len(stack)-1]
			found = true
		}
		if len(outlook) > 0 {
			candidate := outlook[len(outlook)-1]
			if !found || candidate.depth > current.depth {
				current = candidate
				found = true
			}
		}
		return current, found
	}
	lastContent := -1
	for i, p := range parts {
		if p.typ == html.StartTagToken || p.typ == html.SelfClosingTagToken || (p.typ == html.TextToken && strings.TrimSpace(p.token.Data) != "") {
			lastContent = i
		}
	}
	for i, p := range parts {
		for len(stack) > 0 && stack[len(stack)-1].until < i {
			stack = stack[:len(stack)-1]
		}
		for len(outlook) > 0 && outlook[len(outlook)-1].until < i {
			outlook = outlook[:len(outlook)-1]
		}
		if htmlLineBoundary(p) {
			flushHTMLLine(&visibleLine, &priorText)
		}
		if p.typ == html.StartTagToken || p.typ == html.SelfClosingTagToken {
			basis := structuralHTMLBasis(p.token)
			confidence := "marker"
			id := htmlAttr(p.token, "id")
			outlookScope := -1
			if p.token.Data == "div" && (id == "divrplyfwdmsg" || id == "appendonsend") {
				headerEnd := min(len(parts), i+100)
				if p.close >= 0 {
					headerEnd = min(headerEnd, p.close)
				}
				if htmlHeaderFollows(parts, i+1, headerEnd) {
					basis = "outlook_header"
					outlookScope = len(parts)
					if parent, ok := activeContext(); ok {
						outlookScope = parent.until
					}
				}
			}
			if p.token.Data == "hr" && htmlHeaderFollows(parts, i+1, min(len(parts), i+100)) {
				basis = "outlook_header"
				outlookScope = len(parts)
				if parent, ok := activeContext(); ok {
					outlookScope = parent.until
				}
			}
			if basis == "" && bareHTMLQuote(parts, i, priorText, lastContent) {
				basis = "trailing_blockquote"
				confidence = "heuristic"
			}
			if basis != "" {
				until := p.close
				if until < 0 {
					until = len(parts)
				}
				if outlookScope >= 0 {
					until = min(until, outlookScope)
				}
				depth := 1
				kind := Quoted
				var claimed *Claimed
				if parent, ok := activeContext(); ok {
					depth = parent.depth + 1
					kind = parent.kind
					claimed = parent.claimed
				}
				ls := htmlHeaderLinesBeforeNestedQuote(parts, i+1, min(until, len(parts), i+100))
				if len(ls) > 0 && forwardMarker(strings.TrimSpace(ls[0].text)) {
					kind = Forwarded
					claimed = claimedHeaders(ls, 1)
				}
				c := context{until, basis, confidence, depth, kind, claimed}
				stack = append(stack, c)
				if outlookScope >= 0 {
					c.until = outlookScope
					c.basis = "outlook_header"
					outlook = append(outlook, c)
				}
			}
		}
		s := segment(Authored, p.start, p.end, "unmarked", 0)
		if c, ok := activeContext(); ok {
			s = segment(c.kind, p.start, p.end, c.basis, c.depth)
			s.Confidence = c.confidence
			s.Claimed = c.claimed
		}
		addSegment(r, s)
		if p.typ == html.TextToken {
			appendHTMLTextLine(&visibleLine, &priorText, p.token.Data)
		}
	}
	// Tokenizer errors may leave a trailing incomplete tag or comment. Preserve
	// those exact bytes; they provide no positive quote marker.
	if end < len(body) {
		r.Truncated = true
		addSegment(r, segment(Authored, end, len(body), "unexamined_tail", 0))
	} else if last := lastHTMLEnd(parts); last < len(body) {
		addSegment(r, segment(Authored, last, len(body), "unmarked", 0))
	}
}
func lastHTMLEnd(parts []htmlPart) int {
	if len(parts) == 0 {
		return 0
	}
	return parts[len(parts)-1].end
}
