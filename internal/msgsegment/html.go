package msgsegment

import (
	"slices"
	"strings"

	"golang.org/x/net/html"

	"go.kenn.io/msgvault/internal/mime"
)

// Noncharacters mark quote container boundaries through mime.StripHTML.
// Unicode reserves them for internal use; markQuoteContainers removes any
// that the markup's text already holds.
const (
	blockquoteOpen  = '\uFDD0'
	blockquoteClose = '\uFDD1'
	gmailQuoteOpen  = '\uFDD2'
	gmailQuoteClose = '\uFDD3'
)

// HTMLText converts an HTML body to text with mime.StripHTML and prefixes
// each line from a quote container with '>' markers so Split labels it
// quoted. Quote containers match the Web UI's quote roots: blockquote and
// div elements whose class contains gmail_quote. A line gets one marker per
// enclosing blockquote, and one marker inside a Gmail quote outside any
// blockquote, which covers Gmail's attribution line. A "--" line outside
// quotes regains the trailing space of the "-- " signature delimiter.
func HTMLText(rawHTML string) string {
	text := mime.StripHTML(markQuoteContainers(rawHTML))
	var out strings.Builder
	blockquotes, gmailQuotes := 0, 0
	for line := range strings.SplitSeq(text, "\n") {
		depth, content := -1, strings.Builder{}
		for _, r := range line {
			switch r {
			case blockquoteOpen:
				blockquotes++
			case blockquoteClose:
				blockquotes = max(0, blockquotes-1)
			case gmailQuoteOpen:
				gmailQuotes++
			case gmailQuoteClose:
				gmailQuotes = max(0, gmailQuotes-1)
			default:
				if depth < 0 {
					depth = blockquotes
					if depth == 0 && gmailQuotes > 0 {
						depth = 1
					}
				}
				content.WriteRune(r)
			}
		}
		if depth < 0 && line != "" {
			// The line held only markers.
			continue
		}
		if trimmed := strings.TrimSpace(content.String()); trimmed != "" && depth > 0 {
			out.WriteString(strings.Repeat(">", depth))
			out.WriteByte(' ')
			out.WriteString(trimmed)
		} else if trimmed == "--" {
			// StripHTML trims the space from the "-- " signature delimiter.
			out.WriteString("-- ")
		} else {
			out.WriteString(trimmed)
		}
		out.WriteByte('\n')
	}
	result := out.String()
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

// markQuoteContainers inserts boundary markers inside each quote container's
// start and end tags, leaving the rest of the markup byte-for-byte intact.
func markQuoteContainers(rawHTML string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(rawHTML))
	var out strings.Builder
	type open struct {
		tag   string
		close rune
	}
	var stack []open
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			return out.String()
		}
		// TagName lowercases the tokenizer buffer in place, so copy first.
		raw := string(tokenizer.Raw())
		name, hasAttr := tokenizer.TagName()
		tag := string(name)
		switch {
		case tokenType == html.StartTagToken && tag == "blockquote":
			out.WriteString(raw)
			out.WriteRune(blockquoteOpen)
			stack = append(stack, open{tag, blockquoteClose})
		case tokenType == html.StartTagToken && tag == "div":
			out.WriteString(raw)
			var closeMarker rune
			for hasAttr {
				var key, value []byte
				key, value, hasAttr = tokenizer.TagAttr()
				if closeMarker == 0 && string(key) == "class" && strings.Contains(string(value), "gmail_quote") {
					out.WriteRune(gmailQuoteOpen)
					closeMarker = gmailQuoteClose
				}
			}
			stack = append(stack, open{tag, closeMarker})
		case tokenType == html.EndTagToken && (tag == "blockquote" || tag == "div"):
			for i, element := range slices.Backward(stack) {
				if element.tag != tag {
					continue
				}
				for _, element := range stack[i:] {
					if element.close != 0 {
						out.WriteRune(element.close)
					}
				}
				stack = stack[:i]
				break
			}
			out.WriteString(raw)
		case tokenType == html.TextToken:
			// Filter decoded text so entity-encoded markers cannot end a quote.
			out.WriteString(html.EscapeString(stripMarkers.Replace(string(tokenizer.Text()))))
		default:
			out.WriteString(raw)
		}
	}
}

var stripMarkers = strings.NewReplacer(
	string(blockquoteOpen), "", string(blockquoteClose), "",
	string(gmailQuoteOpen), "", string(gmailQuoteClose), "",
)
