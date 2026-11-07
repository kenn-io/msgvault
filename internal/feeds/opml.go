package feeds

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNoFeedsInOPML means an OPML document named no feed URLs.
var ErrNoFeedsInOPML = errors.New("OPML file lists no feeds")

type opmlDocument struct {
	Body struct {
		Outlines []opmlOutline `xml:"outline"`
	} `xml:"body"`
}

type opmlOutline struct {
	XMLURL   string        `xml:"xmlUrl,attr"`
	Outlines []opmlOutline `xml:"outline"`
}

// ParseOPML returns the feed URLs of an OPML subscription list, the export
// format of most feed readers, in document order and without repeats.
// Folders are flattened.
func ParseOPML(r io.Reader) ([]string, error) {
	var doc opmlDocument
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("read OPML: %w", err)
	}
	var urls []string
	seen := map[string]bool{}
	var walk func([]opmlOutline)
	walk = func(outlines []opmlOutline) {
		for _, outline := range outlines {
			if feedURL := strings.TrimSpace(outline.XMLURL); feedURL != "" && !seen[feedURL] {
				seen[feedURL] = true
				urls = append(urls, feedURL)
			}
			walk(outline.Outlines)
		}
	}
	walk(doc.Body.Outlines)
	if len(urls) == 0 {
		return nil, ErrNoFeedsInOPML
	}
	return urls, nil
}
