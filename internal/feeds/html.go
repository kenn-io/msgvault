package feeds

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// urlAttributes are the HTML attributes whose value is one URL.
var urlAttributes = map[string]bool{
	"href":   true,
	"src":    true,
	"poster": true,
	"cite":   true,
}

// absoluteURLs makes the relative URLs in an item's HTML absolute against
// base, the feed document's address, so the archived copy still points where
// the feed meant once it no longer sits next to the feed. gofeed has already
// applied any xml:base. Every byte outside a rewritten tag is kept as it was.
// A fragment-only link (#notes) stays as it is.
//
// Each resolved URL is as long as base, so many short references against a
// long base grow the HTML many times over. When the rewritten HTML would be
// longer than maxBytes, the item keeps its HTML as published.
func absoluteURLs(source, base string, maxBytes int) string {
	baseURL, err := url.Parse(base)
	if err != nil || !baseURL.IsAbs() {
		return source
	}
	var out strings.Builder
	tokens := html.NewTokenizer(strings.NewReader(source))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			break
		}
		raw := string(tokens.Raw())
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			out.WriteString(raw)
			continue
		}
		token := tokens.Token()
		room := maxBytes - out.Len()
		changed := false
		for i, attr := range token.Attr {
			var resolved string
			switch {
			case urlAttributes[attr.Key]:
				resolved = absoluteURL(baseURL, attr.Val)
			case attr.Key == "srcset":
				resolved = absoluteSrcset(baseURL, attr.Val, room)
			}
			if resolved != "" && resolved != attr.Val {
				// A tag's URL attributes each count against the room.
				if room -= len(resolved); room < 0 {
					return source
				}
				token.Attr[i].Val = resolved
				changed = true
			}
		}
		if changed {
			out.WriteString(token.String())
		} else {
			out.WriteString(raw)
		}
		if out.Len() > maxBytes {
			return source
		}
	}
	return out.String()
}

// absoluteURL resolves a relative reference against base. It returns ""
// for a value it leaves alone: empty, fragment-only, already absolute, or
// unparsable.
func absoluteURL(base *url.URL, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") {
		return ""
	}
	ref, err := url.Parse(value)
	if err != nil || ref.IsAbs() {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// absoluteSrcset resolves each candidate URL in a srcset list. A list with a
// data: URL is left alone, since its commas can't be told from the list's. It
// stops once the list it builds is longer than room, and returns that longer
// list, so the caller sees it doesn't fit.
func absoluteSrcset(base *url.URL, value string, room int) string {
	if strings.Contains(value, "data:") {
		return ""
	}
	candidates := strings.Split(value, ",")
	changed := false
	size := 0
	for i, candidate := range candidates {
		fields := strings.Fields(candidate)
		if len(fields) == 0 {
			continue
		}
		if resolved := absoluteURL(base, fields[0]); resolved != "" {
			fields[0] = resolved
			changed = true
		}
		candidates[i] = strings.Join(fields, " ")
		if size += len(candidates[i]) + len(", "); size > room {
			return strings.Join(candidates[:i+1], ", ")
		}
	}
	if !changed {
		return ""
	}
	return strings.Join(candidates, ", ")
}
