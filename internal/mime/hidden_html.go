package mime

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aymerick/douceur/css"
	"github.com/aymerick/douceur/parser"
	"github.com/gorilla/css/scanner"
	"golang.org/x/image/colornames"
	"golang.org/x/net/html"
)

// HTMLHasHiddenText reports whether inline styles or hidden attributes may hide letters or digits.
// Stylesheets and browser layout are outside this check.
func HTMLHasHiddenText(rawHTML string) bool {
	rawHTML = strings.TrimSpace(strings.TrimPrefix(rawHTML, "\ufeff"))
	if rawHTML == "" {
		return false
	}
	doc, err := html.Parse(strings.NewReader(rawHTML))
	return err != nil || htmlHasHiddenText(doc, htmlTextState{})
}

// htmlTextState is what an element passes to its descendants. Hidden covers the whole subtree;
// invisible and tiny come from inherited properties that a descendant can reset.
type htmlTextState struct {
	hidden, invisible, tiny bool
	color, background       string
}

func htmlHasHiddenText(node *html.Node, state htmlTextState) bool {
	if node.Type == html.ElementNode {
		state = hiddenHTMLElement(node, state)
	}
	if state.hidden || state.invisible || state.tiny || state.color == "transparent" || state.color != "" && state.color == state.background {
		if node.Type == html.TextNode && readableHTMLText(node.Data) {
			return true
		}
		for _, attr := range node.Attr {
			if (node.Data == "img" && attr.Key == "alt" || node.Data == "a" && attr.Key == "href") && readableHTMLText(attr.Val) {
				return true
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if htmlHasHiddenText(child, state) {
			return true
		}
	}
	return false
}

func readableHTMLText(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

func inlineHTMLStyle(node *html.Node) map[string]string {
	style := make(map[string]string)
	important := make(map[string]bool)
	for _, attr := range node.Attr {
		if value := inlineHTMLColor(attr.Val); value != "" {
			switch {
			case attr.Key == "color" && node.Data == "font":
				style["color"] = value
			case attr.Key == "bgcolor":
				style["background-color"] = value
			}
		}
	}
	for _, attr := range node.Attr {
		if attr.Key != "style" {
			continue
		}
		for _, declaration := range inlineHTMLDeclarations(attr.Val) {
			// Decode escapes after parsing so an escaped ";" or "!" stays a value character.
			name := strings.ToLower(decodeInlineCSSEscapes(declaration.Property))
			value := strings.ToLower(declaration.Value)
			// Douceur misses "! important" with whitespace after the bang.
			isImportant := declaration.Important || inlineCSSImportant.MatchString(value)
			value = strings.ToLower(decodeInlineCSSEscapes(inlineCSSImportant.ReplaceAllString(value, "")))
			if name == "background" {
				name = "background-color"
			}
			if name == "color" || name == "background-color" {
				switch {
				case name == "color" && (value == "currentcolor" || slices.Contains(inheritCSSKeywords, value)):
					// The parent's color shows through, so an earlier declaration no longer applies.
				case slices.Contains(inheritCSSKeywords, value) || value == "initial":
					// A CSS-wide keyword replaces an earlier background or color with one that adds no color.
					value = "transparent"
					if name == "color" {
						value = "initial"
					}
				default:
					value = inlineHTMLColor(value)
				}
			} else if !validInlineCSSValue(name, value) {
				value = ""
			}
			// The last valid declaration wins, and an important one beats normal ones.
			if value == "" || important[name] && !isImportant {
				continue
			}
			important[name] = isImportant
			style[name] = value
		}
	}
	return style
}

func inlineHTMLDeclarations(value string) []*css.Declaration {
	var clean strings.Builder
	var chunks []string
	depth := 0
	scan := scanner.New(value)
	for token := scan.Next(); token.Type != scanner.TokenEOF && token.Type != scanner.TokenError; token = scan.Next() {
		if token.Type == scanner.TokenComment {
			clean.WriteString(" ")
			continue
		}
		if token.Type == scanner.TokenFunction || token.Type == scanner.TokenChar && token.Value == "(" {
			depth++
		} else if token.Type == scanner.TokenChar && token.Value == ")" && depth > 0 {
			depth--
		}
		if token.Type == scanner.TokenChar && token.Value == ";" && depth == 0 {
			if declaration := strings.TrimSpace(clean.String()); declaration != "" {
				chunks = append(chunks, declaration+";")
			}
			clean.Reset()
			continue
		}
		clean.WriteString(token.Value)
	}
	// Douceur needs a terminator to retain the last declaration's value.
	if declaration := strings.TrimSpace(clean.String()); declaration != "" {
		chunks = append(chunks, declaration+";")
	}
	declarations, err := parser.ParseDeclarations(strings.Join(chunks, ""))
	if err == nil {
		return declarations
	}
	for _, chunk := range chunks {
		parsed, err := parser.ParseDeclarations(chunk)
		if err == nil {
			declarations = append(declarations, parsed...)
		}
	}
	return declarations
}

var inlineCSSImportant = regexp.MustCompile(`\s*!\s*important$`)

// inlineCSSValues lists the keywords each checked property accepts. <length> also admits lengths and functions.
var inlineCSSValues = map[string]string{
	"display":     "none block inline inline-block flex inline-flex grid inline-grid table inline-table table-row table-cell table-row-group table-header-group table-footer-group table-column table-column-group table-caption list-item contents flow-root -webkit-box",
	"visibility":  "visible hidden collapse",
	"mso-hide":    "all none",
	"opacity":     "<length>",
	"font-size":   "<length> xx-small x-small small medium large x-large xx-large xxx-large smaller larger",
	"height":      "<length> auto",
	"max-height":  "<length> none",
	"overflow":    "visible hidden clip scroll auto",
	"overflow-y":  "visible hidden clip scroll auto",
	"position":    "static relative absolute fixed sticky",
	"left":        "<length> auto",
	"top":         "<length> auto",
	"text-indent": "<length>",
	"margin-left": "<length> auto",
	"margin-top":  "<length> auto",
	"clip":        "<length> auto",
}

// inheritCSSKeywords keep the parent's value of an inherited property.
var inheritCSSKeywords = []string{"inherit", "unset", "revert", "revert-layer"}

func validInlineCSSValue(property, value string) bool {
	keywords, checked := inlineCSSValues[property]
	if !checked || value == "initial" || slices.Contains(strings.Fields(keywords), value) || slices.Contains(inheritCSSKeywords, value) {
		return true
	}
	_, _, length := inlineCSSLength(value)
	return strings.HasPrefix(keywords, "<length>") && (length || strings.HasSuffix(value, ")"))
}

var inlineCSSEscape = regexp.MustCompile(`\\(?:[0-9a-fA-F]{1,6}(?:\r\n|[ \t\r\n\f])?|[^\r\n\f])`)

func decodeInlineCSSEscapes(value string) string {
	return inlineCSSEscape.ReplaceAllStringFunc(value, func(escape string) string {
		number, err := strconv.ParseUint(strings.TrimSpace(escape[1:]), 16, 32)
		if err != nil {
			return escape[1:]
		}
		if number == 0 || number > unicode.MaxRune || !utf8.ValidRune(rune(number)) {
			return "\ufffd"
		}
		return string(rune(number))
	})
}

func hiddenHTMLElement(node *html.Node, state htmlTextState) htmlTextState {
	for _, attr := range node.Attr {
		state.hidden = state.hidden || attr.Key == "hidden"
	}
	var clipped, zeroHeight, positioned, offscreen bool
	for property, value := range inlineHTMLStyle(node) {
		switch property {
		case "color":
			switch {
			case value == "initial":
				state.color = ""
			case value != "currentcolor" && !slices.Contains(inheritCSSKeywords, value):
				state.color = value
			}
		case "background-color":
			if value != "transparent" {
				state.background = value
			}
		case "display":
			state.hidden = state.hidden || value == "none"
		case "mso-hide":
			state.hidden = state.hidden || value == "all"
		case "opacity":
			state.hidden = state.hidden || zeroCSSLength(value)
		case "visibility":
			if !slices.Contains(inheritCSSKeywords, value) {
				state.invisible = value == "hidden" || value == "collapse"
			}
		case "font-size":
			// Relative sizes scale the parent's size, so they keep a zero font at zero.
			number, unit, valid := inlineCSSLength(value)
			relative := valid && slices.Contains([]string{"em", "ex", "ch", "%"}, unit) || value == "smaller" || value == "larger" || slices.Contains(inheritCSSKeywords, value)
			if valid && (number == 0 || unit == "px" && number > 0 && number < 1) {
				state.tiny = true
			} else if !relative {
				state.tiny = false
			}
		case "height", "max-height":
			zeroHeight = zeroHeight || zeroCSSLength(value)
		case "overflow", "overflow-y":
			clipped = clipped || value == "hidden" || value == "clip"
		case "position":
			positioned = value == "absolute" || value == "fixed"
		case "left", "top":
			offscreen = offscreen || offscreenCSSLength(value)
		case "text-indent", "margin-left", "margin-top":
			state.hidden = state.hidden || offscreenCSSLength(value)
		case "clip":
			state.hidden = state.hidden || emptyCSSRect(value)
		default:
			// Other properties do not contribute to this check.
		}
	}
	state.hidden = state.hidden || clipped && zeroHeight || positioned && offscreen
	return state
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

func offscreenCSSLength(value string) bool {
	number, unit, valid := inlineCSSLength(value)
	return valid && (unit == "px" && number <= -1000 || (unit == "em" || unit == "rem") && number <= -100)
}

func emptyCSSRect(value string) bool {
	if !strings.HasPrefix(value, "rect(") || !strings.HasSuffix(value, ")") {
		return false
	}
	fields := strings.Fields(strings.ReplaceAll(value[5:len(value)-1], ",", " "))
	if len(fields) != 4 {
		return false
	}
	var numbers [4]float64
	var units [4]string
	for i, field := range fields {
		number, unit, valid := inlineCSSLength(field)
		if !valid {
			return false
		}
		if number == 0 {
			unit = ""
		}
		numbers[i], units[i] = number, unit
	}
	return numbers[0] == numbers[2] && units[0] == units[2] || numbers[1] == numbers[3] && units[1] == units[3]
}

func inlineHTMLColor(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "transparent" {
		return value
	}
	if color, ok := colornames.Map[value]; ok {
		return fmt.Sprintf("#%02x%02x%02x", color.R, color.G, color.B)
	}
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
		if function == "rgb" && len(channels) != 3 || function == "rgba" && len(channels) != 4 {
			return ""
		}
		var rgb [3]int
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
			if i < 3 {
				rgb[i] = int(math.Round(255 * number / maximum))
			} else if number == 0 {
				return "transparent"
			}
		}
		return fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])
	}
	return ""
}
