package feeds

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/atom"
	gofeedjson "github.com/mmcdole/gofeed/json"
	"golang.org/x/net/html/charset"
)

// gofeed's common item keeps an item's content and summary but drops whether
// each is HTML or plain text. The translators below record plain text in the
// item's Custom map under these keys, so plain text isn't stripped as HTML.
const (
	customContentIsText     = "msgvault.content_is_text"
	customDescriptionIsText = "msgvault.description_is_text"
)

// parseFeed parses an RSS, Atom or JSON feed document.
func parseFeed(body []byte) (*gofeed.Feed, error) {
	parser := gofeed.NewParser()
	parser.AtomTranslator = &atomTranslator{}
	parser.JSONTranslator = &jsonTranslator{}
	feed, err := parser.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	if feed.FeedType == "atom" {
		markAtomTextSummaries(body, feed)
	}
	dropNUL(feed)
	return feed, nil
}

// newFeedDecoder reads a feed document the way gofeed's parsers do: not
// strictly, so common mistakes still tokenize, and converting a declared
// non-UTF-8 charset. A pass of ours over the document then reads what
// gofeed read.
func newFeedDecoder(body []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = false
	d.CharsetReader = charset.NewReaderLabel
	return d
}

const (
	xmlNamespace = "http://www.w3.org/XML/1998/namespace"
	// maxBaseExpansionBytes bounds what resolving a document's URLs against
	// its xml:base may cost while gofeed parses it.
	maxBaseExpansionBytes = 8 * DefaultMaxFeedBytes
)

// baseResolvedAttributes are the attributes gofeed resolves against
// xml:base: Atom's URI attributes, and HTML's in content it reads as HTML.
var baseResolvedAttributes = map[string]bool{
	"action": true, "background": true, "cite": true, "codebase": true,
	"data": true, "href": true, "poster": true, "profile": true,
	"scheme": true, "src": true, "uri": true, "usemap": true,
}

// checkBaseExpansion refuses a document whose xml:base would make gofeed
// allocate more than maxBaseExpansionBytes. While it parses, before any
// limit of ours applies, gofeed resolves against the xml:base in scope each
// URI attribute, each URI element's text, and each URL attribute of HTML in
// an element's text. A long base and many short references can turn a small
// document into gigabytes. The estimate never falls short: a resolved URL is
// at most as long as its base and its reference together, and each '<' in
// an element's text starts at most one HTML tag, which resolves at most one
// URL per attribute name.
func checkBaseExpansion(body []byte) error {
	d := newFeedDecoder(body)
	bases := []int{0} // per open element, the length bound of its xml:base
	total := int64(0)
	// The pass ends where the decoder stops. gofeed reads with the same
	// decoder, so it stops there too.
	for token, err := d.Token(); err == nil; token, err = d.Token() {
		cost := int64(0)
		base := bases[len(bases)-1]
		switch t := token.(type) {
		case xml.StartElement:
			for _, attr := range t.Attr {
				if attr.Name.Local != "base" || attr.Name.Space != xmlNamespace || attr.Value == "" {
					continue
				}
				if _, err := url.Parse(attr.Value); err == nil {
					base += len(attr.Value)
				}
			}
			bases = append(bases, base)
			if base == 0 {
				continue
			}
			cost = int64(base)
			for _, attr := range t.Attr {
				if baseResolvedAttributes[strings.ToLower(attr.Name.Local)] {
					cost += int64(base + len(attr.Value))
				}
			}
		case xml.EndElement:
			if len(bases) > 1 {
				bases = bases[:len(bases)-1]
			}
		case xml.CharData:
			if base > 0 {
				tags := int64(bytes.Count(t, []byte("<")))
				cost = tags * int64(len(baseResolvedAttributes)) * int64(base)
			}
		}
		if total += cost; total > maxBaseExpansionBytes {
			return fmt.Errorf("%w: expanded feed links exceed %d bytes", ErrFeedTooLarge, maxBaseExpansionBytes)
		}
	}
	return nil
}

// atomSummaryTypes reads each Atom entry's summary type, which gofeed's
// atom.Entry doesn't keep.
type atomSummaryTypes struct {
	Entries []struct {
		Summary *struct {
			Type string `xml:"type,attr"`
		} `xml:"summary"`
	} `xml:"entry"`
}

// markAtomTextSummaries marks summaries whose type is text (RFC 4287
// §3.1.1: the type defaults to text). A document gofeed's decoder can't read
// leaves summaries as HTML.
func markAtomTextSummaries(body []byte, feed *gofeed.Feed) {
	var doc atomSummaryTypes
	if err := newFeedDecoder(body).Decode(&doc); err != nil || len(doc.Entries) != len(feed.Items) {
		return
	}
	for i, entry := range doc.Entries {
		if entry.Summary != nil && atomTextType(entry.Summary.Type) {
			markText(feed.Items[i], customDescriptionIsText)
		}
	}
}

// atomTranslator marks content whose type is text (RFC 4287 §4.1.3: the
// type defaults to text).
type atomTranslator struct {
	gofeed.DefaultAtomTranslator
}

func (t *atomTranslator) Translate(raw any) (*gofeed.Feed, error) {
	feed, err := t.DefaultAtomTranslator.Translate(raw)
	if err != nil {
		return nil, fmt.Errorf("translate atom feed: %w", err)
	}
	source, ok := raw.(*atom.Feed)
	if !ok || len(source.Entries) != len(feed.Items) {
		return feed, nil
	}
	for i, entry := range source.Entries {
		if entry != nil && entry.Content != nil && atomTextType(entry.Content.Type) {
			markText(feed.Items[i], customContentIsText)
		}
	}
	return feed, nil
}

func atomTextType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "", "text", "text/plain":
		return true
	default:
		return false
	}
}

// jsonTranslator marks content_text, which JSON Feed defines as plain text,
// and summary, which it defines as a plain-text sentence or two.
type jsonTranslator struct {
	gofeed.DefaultJSONTranslator
}

func (t *jsonTranslator) Translate(raw any) (*gofeed.Feed, error) {
	feed, err := t.DefaultJSONTranslator.Translate(raw)
	if err != nil {
		return nil, fmt.Errorf("translate JSON feed: %w", err)
	}
	source, ok := raw.(*gofeedjson.Feed)
	if !ok || len(source.Items) != len(feed.Items) {
		return feed, nil
	}
	for i, item := range source.Items {
		if item == nil {
			continue
		}
		if item.ContentHTML == "" && item.ContentText != "" {
			markText(feed.Items[i], customContentIsText)
		}
		markText(feed.Items[i], customDescriptionIsText)
	}
	return feed, nil
}

func markText(item *gofeed.Item, key string) {
	if item == nil {
		return
	}
	if item.Custom == nil {
		item.Custom = map[string]string{}
	}
	item.Custom[key] = "true"
}

// dropNUL removes NUL characters from every field msgvault reads. PostgreSQL
// can't store them, and a JSON feed can carry one as \u0000; one stray NUL
// would otherwise fail the item, or the whole feed when it's in an ID.
func dropNUL(feed *gofeed.Feed) {
	clean := func(value *string) {
		if strings.ContainsRune(*value, 0) {
			*value = strings.ReplaceAll(*value, "\x00", "")
		}
	}
	cleanPeople := func(people []*gofeed.Person) {
		for _, person := range people {
			if person != nil {
				clean(&person.Name)
				clean(&person.URL)
			}
		}
	}
	clean(&feed.Title)
	clean(&feed.Link)
	cleanPeople(feed.Authors)
	for _, item := range feed.Items {
		if item == nil {
			continue
		}
		for _, value := range []*string{
			&item.Title, &item.Link, &item.GUID, &item.Content, &item.Description,
			&item.Published, &item.Updated,
		} {
			clean(value)
		}
		cleanPeople(item.Authors)
		for i := range item.Categories {
			clean(&item.Categories[i])
		}
		for _, enclosure := range item.Enclosures {
			if enclosure != nil {
				clean(&enclosure.URL)
				clean(&enclosure.Type)
				clean(&enclosure.Length)
			}
		}
		if item.Image != nil {
			clean(&item.Image.URL)
		}
	}
}
