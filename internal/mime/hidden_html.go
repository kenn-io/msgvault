package mime

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// HTMLHasHiddenText reports whether inline styles or hidden attributes may hide letters or digits.
// Stylesheets and browser layout are outside this check.
func HTMLHasHiddenText(rawHTML string) bool {
	doc, err := html.Parse(strings.NewReader(strings.TrimPrefix(rawHTML, "\ufeff")))
	return err == nil && htmlHasHiddenText(doc, false, "", "")
}

func htmlHasHiddenText(node *html.Node, hidden bool, color, background string) bool {
	if node.Type == html.ElementNode {
		style := inlineHTMLStyle(node)
		hidden = hidden || hiddenHTMLElement(node, style)
		if values := style["color"]; len(values) > 0 {
			color = inlineHTMLColor(values[0])
		}
		if values := style["background-color"]; len(values) > 0 {
			if value := inlineHTMLColor(values[0]); value != "transparent" {
				background = value
			}
		}
	}
	if node.Type == html.TextNode && (hidden || color == "transparent" || color != "" && color == background) &&
		strings.ContainsFunc(node.Data, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return true
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if htmlHasHiddenText(child, hidden, color, background) {
			return true
		}
	}
	return false
}

func inlineHTMLStyle(node *html.Node) map[string][]string {
	style := make(map[string][]string)
	important := make(map[string]bool)
	for _, attr := range node.Attr {
		if attr.Key != "style" {
			continue
		}
		var clean strings.Builder
		remaining := strings.ToLower(attr.Val)
		for {
			before, comment, found := strings.Cut(remaining, "/*")
			clean.WriteString(before)
			if !found {
				break
			}
			_, remaining, _ = strings.Cut(comment, "*/")
		}
		for declaration := range strings.SplitSeq(clean.String(), ";") {
			name, value, ok := strings.Cut(declaration, ":")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			value, priority, _ := strings.Cut(value, "!")
			value = strings.TrimSpace(value)
			if name == "background" {
				name = "background-color"
			}
			// Keep every hiding declaration. Colors follow precedence so children can change the backdrop.
			if name == "color" || name == "background-color" {
				hasPriority := strings.TrimSpace(priority) == "important"
				if important[name] && !hasPriority {
					continue
				}
				important[name] = hasPriority
				style[name] = nil
			}
			style[name] = append(style[name], value)
		}
	}
	return style
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
			case "opacity", "font-size":
				if zeroCSSLength(value) {
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
			case "text-indent":
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
	switch value {
	case "white":
		return "#ffffff"
	case "black":
		return "#000000"
	case "transparent":
		return value
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
