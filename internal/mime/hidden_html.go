package mime

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/inbucket/html2text"
	"golang.org/x/net/html"
)

func visibleHTMLText(rawHTML string) (text string, hidden bool, err error) {
	doc, hidden, err := parseVisibleHTML(rawHTML)
	if err != nil {
		return "", hidden, err
	}
	text, err = html2text.FromHTMLNode(doc)
	if err != nil {
		return "", hidden, fmt.Errorf("convert visible HTML to text: %w", err)
	}
	return text, hidden, nil
}

// HTMLHasHiddenText reports whether inline styles hide letters or numbers.
// Stylesheets and browser layout are outside this check.
func HTMLHasHiddenText(rawHTML string) bool {
	_, hidden, _ := parseVisibleHTML(rawHTML)
	return hidden
}

func parseVisibleHTML(rawHTML string) (*html.Node, bool, error) {
	doc, err := html.Parse(strings.NewReader(strings.TrimPrefix(rawHTML, "\ufeff")))
	if err != nil {
		return nil, false, fmt.Errorf("parse HTML: %w", err)
	}
	return doc, pruneHiddenHTML(doc, htmlTextVisibility{}), nil
}

type htmlTextVisibility struct {
	invisible   bool
	zeroFont    bool
	transparent bool
}

func pruneHiddenHTML(node *html.Node, inherited htmlTextVisibility) bool {
	hidden := false
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		state := inherited
		switch child.Type {
		case html.ElementNode:
			style := inlineHTMLStyle(child)
			if hiddenHTMLElement(child, style) {
				hidden = htmlHasProse(child) || hidden
				node.RemoveChild(child)
				child = next
				continue
			}
			if value, ok := style["visibility"]; ok {
				state.invisible = value == "hidden"
			}
			if value, ok := style["font-size"]; ok {
				_, unit, valid := inlineCSSLength(value)
				relative := value == "smaller" || value == "larger" || valid && (unit == "em" || unit == "%")
				state.zeroFont = zeroCSSLength(value) || inherited.zeroFont && relative
			}
			if value, ok := style["color"]; ok {
				color := inlineHTMLColor(value)
				state.transparent = value == "transparent" || color != "" && color == inlineHTMLColor(style["background-color"])
			}
		case html.TextNode:
			if state.invisible || state.zeroFont || state.transparent {
				hidden = htmlHasProse(child) || hidden
				node.RemoveChild(child)
			}
		default:
			// Other node types keep the inherited visibility.
		}
		hidden = pruneHiddenHTML(child, state) || hidden
		child = next
	}
	return hidden
}

func htmlHasProse(node *html.Node) bool {
	if node.Type == html.TextNode && strings.ContainsFunc(node.Data, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
		return true
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if htmlHasProse(child) {
			return true
		}
	}
	return false
}

func inlineHTMLStyle(node *html.Node) map[string]string {
	style := make(map[string]string)
	for _, attr := range node.Attr {
		if attr.Key != "style" {
			continue
		}
		for declaration := range strings.SplitSeq(strings.ToLower(attr.Val), ";") {
			name, value, ok := strings.Cut(declaration, ":")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "!important"))
			switch name {
			case "background":
				name = "background-color"
			case "font":
				name = "font-size"
				for field := range strings.FieldsSeq(value) {
					size, _, _ := strings.Cut(field, "/")
					number, unit, valid := inlineCSSLength(size)
					if valid && (number == 0 || unit != "") || size == "smaller" || size == "larger" {
						value = size
						break
					}
				}
			}
			style[name] = value
		}
	}
	return style
}

func hiddenHTMLElement(node *html.Node, style map[string]string) bool {
	if style["display"] == "none" {
		return true
	}
	if opacity, err := strconv.ParseFloat(strings.TrimSuffix(style["opacity"], "%"), 64); err == nil && opacity == 0 {
		return true
	}
	clipped := style["overflow"] == "hidden" || style["overflow"] == "clip" || style["overflow-y"] == "hidden" || style["overflow-y"] == "clip"
	zeroHeight := zeroCSSLength(style["height"]) || zeroCSSLength(style["max-height"])
	zeroMinimum := style["min-height"] == "" || zeroCSSLength(style["min-height"])
	if clipped && blockHTMLElement(node, style["display"]) && zeroHeight && zeroMinimum {
		return true
	}
	if style["position"] == "absolute" || style["position"] == "fixed" {
		for _, property := range []string{"left", "top"} {
			value, unit, valid := inlineCSSLength(style[property])
			if valid && unit == "px" && value <= -1000 {
				return true
			}
		}
	}
	return false
}

func blockHTMLElement(node *html.Node, display string) bool {
	switch display {
	case "block", "inline-block", "flex", "inline-flex", "grid", "inline-grid", "table", "table-cell", "list-item":
		return true
	case "":
		switch node.Data {
		case "html", "body", "div", "p", "section", "article", "header", "footer", "main", "nav", "aside", "address", "form", "fieldset", "blockquote", "pre", "ul", "ol", "li", "dl", "dt", "dd", "h1", "h2", "h3", "h4", "h5", "h6", "table", "td", "th":
			return true
		}
	}
	return false
}

func inlineCSSLength(value string) (float64, string, bool) {
	numeric := strings.TrimRight(value, "abcdefghijklmnopqrstuvwxyz%")
	unit := value[len(numeric):]
	switch unit {
	case "", "px", "pt", "pc", "em", "rem", "ex", "ch", "%", "vh", "vw", "vmin", "vmax", "cm", "mm", "in":
		number, err := strconv.ParseFloat(numeric, 64)
		return number, unit, err == nil
	default:
		return 0, unit, false
	}
}

func zeroCSSLength(value string) bool {
	number, _, valid := inlineCSSLength(value)
	return valid && number == 0
}

func inlineHTMLColor(value string) string {
	if strings.HasPrefix(value, "#") {
		hex := value[1:]
		if len(hex) != 3 && len(hex) != 6 {
			return ""
		}
		if _, err := strconv.ParseUint(hex, 16, 24); err != nil {
			return ""
		}
		if len(hex) == 3 {
			return fmt.Sprintf("#%c%c%c%c%c%c", hex[0], hex[0], hex[1], hex[1], hex[2], hex[2])
		}
		return value
	}
	function, components, ok := strings.Cut(value, "(")
	if ok && (function == "rgb" || function == "rgba") && strings.HasSuffix(components, ")") {
		channels := strings.Split(strings.TrimSuffix(components, ")"), ",")
		if len(channels) != 3 && len(channels) != 4 {
			return ""
		}
		for i, channel := range channels {
			channel = strings.TrimSpace(channel)
			maximum := 255.0
			if i == 3 {
				maximum = 1
			}
			if strings.HasSuffix(channel, "%") {
				maximum = 100
			}
			number, err := strconv.ParseFloat(strings.TrimSuffix(channel, "%"), 64)
			if err != nil || !(number >= 0 && number <= maximum) {
				return ""
			}
		}
		return value
	}
	switch value {
	case "black", "silver", "gray", "grey", "white", "maroon", "red", "purple", "fuchsia", "green", "lime", "olive", "yellow", "navy", "blue", "teal", "aqua", "orange":
		return value
	default:
		return ""
	}
}
