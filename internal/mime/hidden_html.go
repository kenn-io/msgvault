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
	return err != nil || htmlHasHiddenText(doc, htmlTextState{}, inlineStyles{})
}

// htmlTextState is what an element passes to its descendants. Hidden covers the whole subtree;
// invisible and tiny come from inherited properties that a descendant can reset.
type htmlTextState struct {
	hidden, invisible, tiny bool
	color, background       string
}

func htmlHasHiddenText(node *html.Node, state htmlTextState, styles inlineStyles) bool {
	if node.Type == html.ElementNode {
		state = hiddenHTMLElement(node, state, styles)
	}
	if state.hidden || state.invisible || state.tiny || state.color == "transparent" || state.color != "" && state.color == state.background {
		if node.Type == html.TextNode && readableHTMLText(node.Data) && node.Parent != nil && node.Parent.Data != "style" && node.Parent.Data != "script" {
			return true
		}
	}
	// A zero font size leaves an image or link visible.
	if state.hidden || state.invisible || state.color == "transparent" || state.color != "" && state.color == state.background {
		for _, attr := range node.Attr {
			if (node.Data == "img" && attr.Key == "alt" || node.Data == "a" && attr.Key == "href") && readableHTMLText(attr.Val) {
				return true
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if htmlHasHiddenText(child, state, styles) {
			return true
		}
	}
	return false
}

func readableHTMLText(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

func inlineHTMLStyle(node *html.Node, styles inlineStyles) map[string]string {
	style := make(map[string]string)
	important := make(map[string]bool)
	for _, attr := range node.Attr {
		if attr.Key == "background" && strings.TrimSpace(attr.Val) != "" {
			style["background-image"] = "image"
		}
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
		if attr.Key != "style" || len(attr.Val) > maxInlineStyleBytes {
			continue
		}
		// The last valid declaration wins, and an important one beats normal ones.
		for _, declaration := range styles.parse(attr.Val) {
			if important[declaration.name] && !declaration.important {
				continue
			}
			important[declaration.name] = declaration.important
			style[declaration.name] = declaration.value
		}
	}
	return style
}

// inlineDeclaration is a style declaration with escapes, shorthands, and invalid values resolved.
type inlineDeclaration struct {
	name, value string
	important   bool
}

// inlineStyles caches parsed style attributes for one document, since email repeats the same
// style on many elements.
type inlineStyles map[string][]inlineDeclaration

func (styles inlineStyles) parse(style string) []inlineDeclaration {
	if declarations, ok := styles[style]; ok {
		return declarations
	}
	var declarations []inlineDeclaration
	add := func(name, value string, isImportant bool) {
		if value != "" {
			declarations = append(declarations, inlineDeclaration{name, value, isImportant})
		}
	}
	for _, declaration := range inlineHTMLDeclarations(style) {
		// Decode escapes after parsing so an escaped ";" or "!" stays a value character.
		name := strings.ToLower(decodeInlineCSSEscapes(declaration.Property))
		value := strings.ToLower(declaration.Value)
		// Douceur misses "! important" with whitespace after the bang or an escaped keyword.
		isImportant := declaration.Important
		if bang := strings.LastIndex(value, "!"); bang > 0 && value[bang-1] != '\\' && strings.TrimSpace(decodeInlineCSSEscapes(value[bang+1:])) == "important" {
			isImportant, value = true, value[:bang]
		}
		value = strings.TrimSpace(strings.ToLower(decodeInlineCSSEscapes(value)))
		// The shorthand also resets the image, to none unless it names one.
		if name == "background" {
			color, valid := backgroundShorthandColor(value)
			if !valid {
				// Browsers ignore an invalid shorthand, keeping the earlier background.
				continue
			}
			// The shorthand resets the image and color it omits, to none and transparent.
			shorthandImage := "none"
			if strings.Contains(value, "url(") || strings.Contains(value, "gradient(") {
				shorthandImage = "image"
			}
			add("background-image", shorthandImage, isImportant)
			add("background-color", color, isImportant)
			continue
		}
		if name == "background-image" {
			switch {
			case value == "none" || value == "initial" || slices.Contains(inheritCSSKeywords, value):
				value = "none"
			case cssImageFunction(cssFunctionName(value)):
				value = "image"
			default:
				// Browsers ignore a value that names no image, keeping the earlier one.
				value = ""
			}
		}
		if name == "font" {
			name, value = "font-size", fontShorthandSize(value)
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
				parsed := inlineHTMLColor(value)
				// A valid color this check can't read still sets a background.
				if parsed == "" && name == "background-color" && unreadableCSSColor(value) {
					parsed = "unknown"
				}
				value = parsed
			}
		} else if !validInlineCSSValue(name, value) {
			value = ""
		}
		add(name, value, isImportant)
	}
	styles[style] = declarations
	return declarations
}

// maxInlineStyleBytes bounds the CSS parser's work on one attribute, which grows quadratically
// with unbalanced parentheses. Real inline styles are far smaller; a longer one cannot be
// checked and counts as hiding.
const maxInlineStyleBytes = 8 << 10

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

// fontShorthandSize returns the size the font shorthand sets: the first field, before any "/",
// that is a font-size keyword or a length with a unit (or zero). It returns "" when there is none
// or no family follows it.
func fontShorthandSize(value string) string {
	if slices.Contains(inheritCSSKeywords, value) {
		return value
	}
	fields := strings.Fields(value)
	for i, field := range fields {
		size, lineHeight, slash := strings.Cut(field, "/")
		_, unit, length := inlineCSSLength(size)
		isSize := length && (unit != "" || size == "0") || slices.Contains(strings.Fields(inlineCSSValues["font-size"]), size)
		if !isSize {
			continue
		}
		// A family must follow the size and any line height, or browsers ignore the shorthand.
		rest := fields[i+1:]
		switch {
		case slash && lineHeight == "" && len(rest) > 0:
			rest = rest[1:]
		case !slash && len(rest) > 0 && rest[0] == "/":
			rest = rest[min(2, len(rest)):]
		case !slash && len(rest) > 0 && strings.HasPrefix(rest[0], "/"):
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return ""
		}
		return size
	}
	return ""
}

// inlineCSSValues lists the keywords each checked property accepts. <length> also admits lengths and
// functions, <length+> only lengths that are not negative, and <number> numbers and percentages.
var inlineCSSValues = map[string]string{
	"display":     "none block inline inline-block flex inline-flex grid inline-grid table inline-table table-row table-cell table-row-group table-header-group table-footer-group table-column table-column-group table-caption list-item contents flow-root -webkit-box",
	"visibility":  "visible hidden collapse",
	"mso-hide":    "all none",
	"opacity":     "<number>",
	"font-size":   "<length+> xx-small x-small small medium large x-large xx-large xxx-large smaller larger",
	"height":      "<length+> auto",
	"max-height":  "<length+> none",
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
	if function := cssFunctionName(value); function != "" {
		return strings.HasPrefix(keywords, "<") && slices.Contains(cssMathFunctions, function) || property == "clip" && function == "rect"
	}
	number, unit, length := inlineCSSLength(value)
	switch strings.Fields(keywords)[0] {
	case "<length>":
		return validCSSLength(number, unit, length)
	case "<length+>":
		return validCSSLength(number, unit, length) && number >= 0
	case "<number>":
		return length && (unit == "" || unit == "%")
	default:
		return false
	}
}

// validCSSLength reports whether a parsed length is valid in standards mode, which rejects a
// unitless length other than zero.
func validCSSLength(number float64, unit string, length bool) bool {
	return length && (unit != "" || number == 0)
}

// cssFunctionName returns the name of a CSS function value, such as "url" for url(a.png), or "".
func cssFunctionName(value string) string {
	name, _, ok := strings.Cut(value, "(")
	if !ok || name == "" || !strings.HasSuffix(value, ")") || strings.ContainsFunc(name, unicode.IsSpace) {
		return ""
	}
	return name
}

var (
	cssMathFunctions  = []string{"calc", "min", "max", "clamp"}
	cssColorFunctions = []string{"rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color", "color-mix", "light-dark"}
	cssImageFunctions = []string{"url", "image-set", "-webkit-image-set", "cross-fade", "element", "paint"}
	// backgroundCSSKeywords are the keywords the background shorthand accepts besides colors.
	backgroundCSSKeywords = strings.Fields("none currentcolor repeat no-repeat repeat-x repeat-y space round scroll fixed local left right top bottom center border-box padding-box content-box text auto cover contain")
)

func cssImageFunction(name string) bool {
	return slices.Contains(cssImageFunctions, name) || strings.HasSuffix(name, "-gradient")
}

// unreadableCSSColor reports a valid color that inlineHTMLColor can't resolve: a color function
// or a hex color with alpha.
func unreadableCSSColor(value string) bool {
	if hex, ok := strings.CutPrefix(value, "#"); ok && (len(hex) == 4 || len(hex) == 8) {
		_, err := strconv.ParseUint(hex, 16, 32)
		return err == nil
	}
	return slices.Contains(cssColorFunctions, cssFunctionName(value))
}

// backgroundShorthandColor returns the background color a shorthand sets: a resolved color,
// "unknown" for one this check can't read, or "transparent" when it names none. It reports false
// unless every field, split on spaces, slashes, and commas outside parentheses, is a value the
// shorthand accepts.
func backgroundShorthandColor(value string) (string, bool) {
	if value == "initial" || slices.Contains(inheritCSSKeywords, value) {
		return "transparent", true
	}
	var fields []string
	depth, start := 0, -1
	for i, r := range value {
		if r == '(' {
			depth++
		} else if r == ')' && depth > 0 {
			depth--
		}
		if depth == 0 && (unicode.IsSpace(r) || r == '/' || r == ',') {
			if start >= 0 {
				fields = append(fields, value[start:i])
			}
			start = -1
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, value[start:])
	}
	color := "transparent"
	for _, field := range fields {
		function := cssFunctionName(field)
		number, unit, length := inlineCSSLength(field)
		switch {
		case inlineHTMLColor(field) != "":
			color = inlineHTMLColor(field)
		case unreadableCSSColor(field) || field == "currentcolor":
			color = "unknown"
		case !slices.Contains(backgroundCSSKeywords, field) && !validCSSLength(number, unit, length) &&
			!cssImageFunction(function) && !slices.Contains(cssMathFunctions, function):
			return "", false
		}
	}
	return color, len(fields) > 0
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

func hiddenHTMLElement(node *html.Node, state htmlTextState, styles inlineStyles) htmlTextState {
	for _, attr := range node.Attr {
		state.hidden = state.hidden || attr.Key == "hidden" || attr.Key == "style" && len(attr.Val) > maxInlineStyleBytes
	}
	var clipped, zeroHeight, positioned, offscreen, image bool
	for property, value := range inlineHTMLStyle(node, styles) {
		switch property {
		case "color":
			switch {
			case value == "initial":
				state.color = ""
			case value != "currentcolor" && !slices.Contains(inheritCSSKeywords, value):
				state.color = value
			}
		case "background-color":
			if value == "unknown" {
				state.background = ""
			} else if value != "transparent" {
				state.background = value
			}
		case "background-image":
			image = value == "image"
		case "display":
			state.hidden = state.hidden || value == "none"
		case "mso-hide":
			state.hidden = state.hidden || value == "all"
		case "opacity":
			// Browsers clamp negative opacity to zero.
			number, _, valid := inlineCSSLength(value)
			state.hidden = state.hidden || valid && number <= 0
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
			positioned = value == "absolute" || value == "fixed" || value == "relative"
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
	if image {
		// Text over an image has no known background color.
		state.background = ""
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
		var alpha string
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
			if i == 3 && maximum == 100 {
				number /= 100
			}
			if i < 3 {
				rgb[i] = int(math.Round(255 * number / maximum))
			} else if number == 0 {
				return "transparent"
			} else if number < 1 {
				// A translucent color never equals an opaque one.
				alpha = fmt.Sprintf("@%g", number)
			}
		}
		return fmt.Sprintf("#%02x%02x%02x%s", rgb[0], rgb[1], rgb[2], alpha)
	}
	return ""
}
