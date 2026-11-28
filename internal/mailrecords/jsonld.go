package mailrecords

import (
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/jsonexact"
	"golang.org/x/net/html"
)

// Extract parses schema.org JSON-LD without network access or side effects.
// Oversized or excessively nested HTML yields an empty result. A malformed
// JSON-LD block is ignored; other well-formed blocks can still contribute.
func Extract(input Input) Result {
	result := emptyResult()
	if len(input.HTML) > MaxHTMLBytes || !strings.Contains(strings.ToLower(input.HTML), "application/ld+json") {
		return result
	}
	blocks, text, ok := scriptBlocks(input.HTML)
	if !ok {
		return result
	}
	origin := input.Origin
	if origin != "sender" && origin != "relayed" {
		origin = "unknown"
	}
	// Keep all scans bounded even if a caller supplies an enormous text body.
	if len(input.Text) > MaxHTMLBytes {
		input.Text = input.Text[:MaxHTMLBytes]
	}
	visible := newGuardSource(input.Text + "\n" + text)
	source := newGuardSource(input.HTML + "\n" + input.Text)
	nodes := 0
	for _, block := range blocks {
		var value any
		if json.Unmarshal([]byte(block), &value, jsonexact.PreserveNumbers) != nil {
			continue
		}
		walk(value, contextAbsent, 0, &nodes, func(object map[string]any, context jsonldContext) {
			if len(result.Records) >= MaxRecords {
				return
			}
			name := recordType(object["@type"], context == contextSchema)
			if name == "" {
				return
			}
			record := extractRecord(name, object, context, source, visible, origin)
			result.Records = append(result.Records, record)
			result.Dropped += record.Dropped
			if result.Kind == "none" {
				result.Kind = kinds[name]
				result.KindSource = "json_ld"
				result.KindTrust = "claimed_by_sender"
			}
		})
		if nodes >= maxNodes || len(result.Records) >= MaxRecords {
			break
		}
	}
	return result
}

func scriptBlocks(source string) ([]string, string, bool) {
	z := html.NewTokenizer(strings.NewReader(source))
	z.SetMaxBuf(MaxHTMLBytes)
	// Bound work before building the HTML tree. The tree accounts for implied
	// end tags, so successive <p> or <li> elements do not count as nesting.
	for tokens := 0; ; tokens++ {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				return nil, "", false
			}
			break
		}
		if tokens >= maxNodes {
			return nil, "", false
		}
	}
	document, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return nil, "", false
	}
	var blocks []string
	var text strings.Builder
	var visit func(*html.Node, int, bool) bool
	visit = func(node *html.Node, depth int, hidden bool) bool {
		if depth > maxDepth {
			return false
		}
		if node.Type == html.ElementNode {
			hidden = hidden || node.Data == "script" || node.Data == "style"
			if node.Data == "script" {
				for _, attr := range node.Attr {
					if attr.Key != "type" {
						continue
					}
					mediaType, _, err := mime.ParseMediaType(attr.Val)
					if err == nil && strings.EqualFold(mediaType, "application/ld+json") {
						for child := node.FirstChild; child != nil; child = child.NextSibling {
							if child.Type == html.TextNode {
								blocks = append(blocks, child.Data)
							}
						}
					}
					break
				}
			}
		}
		if node.Type == html.TextNode && !hidden {
			text.WriteString(node.Data)
			text.WriteByte(' ')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if !visit(child, depth+1, hidden) {
				return false
			}
		}
		return true
	}
	ok := visit(document, 0, false)
	return blocks, text.String(), ok
}

type jsonldContext uint8

const (
	contextAbsent jsonldContext = iota
	contextSchema
	contextUnsupported
)

func schemaContext(value any) bool {
	switch v := value.(type) {
	case string:
		return v == "https://schema.org" || v == "https://schema.org/" || v == "http://schema.org" || v == "http://schema.org/"
	case []any:
		// Unknown remote contexts and custom maps can redefine names.
		return len(v) > 0 && !slices.ContainsFunc(v, func(x any) bool { return !schemaContext(x) })
	default:
		return false
	}
}

func objectContext(object map[string]any, inherited jsonldContext) jsonldContext {
	if local, exists := object["@context"]; exists {
		if schemaContext(local) {
			return contextSchema
		}
		return contextUnsupported
	}
	return inherited
}

func walk(value any, context jsonldContext, depth int, nodes *int, visit func(map[string]any, jsonldContext)) {
	if depth > maxDepth || *nodes >= maxNodes {
		return
	}
	*nodes++
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			walk(child, context, depth+1, nodes, visit)
		}
	case map[string]any:
		context = objectContext(v, context)
		visit(v, context)
		keys := make([]string, 0, len(v))
		for key := range v {
			if key != "@context" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			walk(v[key], context, depth+1, nodes, visit)
		}
	}
}

var kinds = map[string]string{
	"Order": "order", "ParcelDelivery": "shipment", "Invoice": "invoice",
	"FlightReservation": "flight", "LodgingReservation": "lodging", "EventReservation": "event_ticket",
}

func recordType(value any, context bool) string {
	switch v := value.(type) {
	case string:
		name := v
		if suffix, ok := strings.CutPrefix(v, "https://schema.org/"); ok {
			name = suffix
		} else if suffix, ok := strings.CutPrefix(v, "http://schema.org/"); ok {
			name = suffix
		} else if !context {
			return ""
		}
		if _, ok := kinds[name]; ok {
			return name
		}
	case []any:
		for _, item := range v {
			if name := recordType(item, context); name != "" {
				return name
			}
		}
	}
	return ""
}
