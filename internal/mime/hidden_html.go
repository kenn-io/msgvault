package mime

import (
	"fmt"
	"math"
	"regexp"
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
	return err != nil || htmlHasHiddenText(doc, false, "", "")
}

func htmlHasHiddenText(node *html.Node, hidden bool, color, background string) bool {
	if node.Type == html.ElementNode {
		style := inlineHTMLStyle(node)
		hidden = hidden || hiddenHTMLElement(node, style)
		if values := style["color"]; len(values) > 0 && values[0] != "inherit" && values[0] != "currentcolor" {
			color = values[0]
		}
		if values := style["background-color"]; len(values) > 0 {
			if values[0] != "transparent" {
				background = values[0]
			}
		}
	}
	if hidden || color == "transparent" || color != "" && color == background {
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
		if htmlHasHiddenText(child, hidden, color, background) {
			return true
		}
	}
	return false
}

func readableHTMLText(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

func inlineHTMLStyle(node *html.Node) map[string][]string {
	style := make(map[string][]string)
	important := make(map[string]bool)
	for _, attr := range node.Attr {
		if value := inlineHTMLColor(attr.Val); value != "" {
			switch {
			case attr.Key == "color" && node.Data == "font":
				style["color"] = []string{value}
			case attr.Key == "bgcolor":
				style["background-color"] = []string{value}
			}
		}
	}
	for _, attr := range node.Attr {
		if attr.Key != "style" {
			continue
		}
		for _, declaration := range inlineHTMLDeclarations(attr.Val) {
			name := strings.ToLower(declaration.Property)
			value := strings.ToLower(declaration.Value)
			if name == "background" {
				name = "background-color"
			}
			// Keep every hiding declaration. Colors follow precedence so children can change the backdrop.
			if name == "color" || name == "background-color" {
				if name != "color" || value != "inherit" && value != "currentcolor" {
					value = inlineHTMLColor(value)
					if value == "" {
						continue
					}
				}
				if important[name] && !declaration.Important {
					continue
				}
				important[name] = declaration.Important
				style[name] = nil
			}
			style[name] = append(style[name], value)
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
		value := token.Value
		if token.Type != scanner.TokenString && token.Type != scanner.TokenURI {
			value = decodeInlineCSSEscapes(value)
		}
		clean.WriteString(value)
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

func hiddenHTMLElement(node *html.Node, style map[string][]string) bool {
	for _, attr := range node.Attr {
		if attr.Key == "hidden" {
			return true
		}
	}
	var clipped, zeroHeight, positioned, offscreen bool
	for property, values := range style {
		for _, value := range values {
			switch property {
			case "display", "visibility":
				if property == "display" && value == "none" || property == "visibility" && value == "hidden" {
					return true
				}
			case "mso-hide":
				if value == "all" {
					return true
				}
			case "opacity":
				if zeroCSSLength(value) {
					return true
				}
			case "font-size":
				number, unit, valid := inlineCSSLength(value)
				if valid && (number == 0 || unit == "px" && number > 0 && number < 1) {
					return true
				}
			case "height", "max-height":
				zeroHeight = zeroHeight || zeroCSSLength(value)
			case "overflow", "overflow-y":
				clipped = clipped || value == "hidden" || value == "clip"
			case "position":
				positioned = positioned || value == "absolute" || value == "fixed"
			case "left", "top":
				offscreen = offscreen || offscreenCSSLength(value)
			case "text-indent", "margin-left", "margin-top":
				if offscreenCSSLength(value) {
					return true
				}
			case "clip":
				if emptyCSSRect(value) {
					return true
				}
			default:
				// Other properties do not contribute to this check.
			}
		}
	}
	return clipped && zeroHeight || positioned && offscreen
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
