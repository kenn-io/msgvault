package feeds

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmcdole/gofeed"
	"go.kenn.io/msgvault/internal/messageimport"
)

const (
	maxExternalIDChars = 256
	maxTitleChars      = 4096
	maxSenderNameChars = 256
)

// itemKey is an item's stable external ID: its GUID (RSS guid, Atom id or
// JSON Feed id), else its link, else a hash of what it says. A key too long
// for the archive is hashed.
func itemKey(item *gofeed.Item, link string) string {
	key := strings.TrimSpace(item.GUID)
	if key == "" {
		key = link
	}
	if key == "" {
		key = "sha256:" + hashParts(item.Title, item.Published, item.Updated, item.Content, item.Description)
	}
	if utf8.RuneCountInString(key) > maxExternalIDChars {
		key = "sha256:" + hashParts(key)
	}
	return key
}

// itemMessage maps one feed item to an import message. firstSeen dates an
// item that carries no date; feeds archive each item once, so that date
// stays as first recorded.
func itemMessage(feedURL string, feed *gofeed.Feed, item *gofeed.Item, firstSeen time.Time) messageimport.ImportedMessage {
	base := siteURL(feedURL, feed)
	link := resolveURL(base, item.Link)
	sentAt := firstSeen
	switch {
	case item.PublishedParsed != nil:
		sentAt = *item.PublishedParsed
	case item.UpdatedParsed != nil:
		sentAt = *item.UpdatedParsed
	}
	body := strings.TrimSpace(item.Content)
	if body == "" {
		body = strings.TrimSpace(item.Description)
	}
	message := messageimport.ImportedMessage{
		ExternalID:  itemKey(item, link),
		MessageType: MessageType,
		Conversation: messageimport.MessageImportConversation{
			ExternalID: feedURL,
			Title:      truncate(feedTitle(feedURL, feed), maxTitleChars),
		},
		Sender:   itemSender(feedURL, feed, item, base),
		Title:    truncate(strings.TrimSpace(item.Title), maxTitleChars),
		URL:      link,
		SentAt:   sentAt.UTC().Format(time.RFC3339Nano),
		BodyHTML: body,
		Metadata: itemMetadata(item, base),
	}
	return message
}

// itemSender names who wrote an item. An author with a URL is that site's
// sender. An author with only a name is a sender of this feed, so two feeds'
// "Admin" stay two people. Without an author, the feed's site sent it.
// Author emails are ignored: feeds often publish one placeholder address for
// every author, which would merge strangers into one participant.
func itemSender(feedURL string, feed *gofeed.Feed, item *gofeed.Item, base string) messageimport.MessageImportSender {
	author := firstAuthor(item.Authors)
	if author == nil {
		author = firstAuthor(feed.Authors)
	}
	if author != nil {
		name := truncate(strings.TrimSpace(author.Name), maxSenderNameChars)
		if authorURL := resolveURL(base, author.URL); authorURL != "" {
			return messageimport.MessageImportSender{Name: name, URL: authorURL}
		}
		if name != "" {
			return messageimport.MessageImportSender{Name: name, ID: "author:" + hashParts(feedURL, name)[:32]}
		}
	}
	return messageimport.MessageImportSender{
		Name: truncate(feedTitle(feedURL, feed), maxSenderNameChars),
		URL:  base,
	}
}

func firstAuthor(authors []*gofeed.Person) *gofeed.Person {
	for _, author := range authors {
		if author != nil && (strings.TrimSpace(author.Name) != "" || strings.TrimSpace(author.URL) != "") {
			return author
		}
	}
	return nil
}

func itemMetadata(item *gofeed.Item, base string) map[string]any {
	metadata := map[string]any{}
	if len(item.Categories) > 0 {
		metadata["categories"] = item.Categories
	}
	var enclosures []map[string]any
	for _, enclosure := range item.Enclosures {
		if enclosure == nil {
			continue
		}
		if href := resolveURL(base, enclosure.URL); href != "" {
			entry := map[string]any{"url": href}
			if enclosure.Type != "" {
				entry["type"] = enclosure.Type
			}
			if enclosure.Length != "" {
				entry["length"] = enclosure.Length
			}
			enclosures = append(enclosures, entry)
		}
	}
	if len(enclosures) > 0 {
		metadata["enclosures"] = enclosures
	}
	if item.Image != nil {
		if image := resolveURL(base, item.Image.URL); image != "" {
			metadata["image"] = image
		}
	}
	if item.UpdatedParsed != nil {
		metadata["updated_at"] = item.UpdatedParsed.UTC().Format(time.RFC3339Nano)
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// feedTitle is the feed's title, or its host when it has none.
func feedTitle(feedURL string, feed *gofeed.Feed) string {
	if title := strings.TrimSpace(feed.Title); title != "" {
		return title
	}
	if parsed, err := url.Parse(feedURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return feedURL
}

// siteURL is the feed's website, the base for its relative links, or the
// feed URL itself when the feed names no usable site.
func siteURL(feedURL string, feed *gofeed.Feed) string {
	if site := resolveURL(feedURL, feed.Link); site != "" {
		return site
	}
	return feedURL
}

// resolveURL resolves ref against base and keeps only absolute http and
// https URLs that the archive accepts.
func resolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' }) {
		return ""
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if !parsed.IsAbs() {
		baseURL, err := url.Parse(base)
		if err != nil {
			return ""
		}
		parsed = baseURL.ResolveReference(parsed)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	resolved := parsed.String()
	if utf8.RuneCountInString(resolved) > maxURLChars {
		return ""
	}
	return resolved
}

func truncate(value string, maxChars int) string {
	if utf8.RuneCountInString(value) <= maxChars {
		return value
	}
	return string([]rune(value)[:maxChars])
}

func hashParts(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
