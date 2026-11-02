package mime

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// StripHTMLPrefix removes hidden element content from a bounded HTML prefix.
// Parsing the prefix first lets the HTML tree builder close unfinished head,
// style, and script elements at EOF before StripHTML extracts readable text.
func StripHTMLPrefix(rawHTML string) string {
	document, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return StripHTML(rawHTML)
	}
	removeHiddenHTMLNodes(document)

	var rendered bytes.Buffer
	if err := html.Render(&rendered, document); err != nil {
		return StripHTML(rawHTML)
	}
	return StripHTML(rendered.String())
}

func removeHiddenHTMLNodes(parent *html.Node) {
	for child := parent.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && (child.Data == "head" || child.Data == "style" || child.Data == "script") {
			parent.RemoveChild(child)
		} else {
			removeHiddenHTMLNodes(child)
		}
		child = next
	}
}
