package gvoice

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/textimport"
	"golang.org/x/net/html"
)

// Filename classification patterns.
var (
	// "{Name} - Text - {Timestamp}.html".
	reText = regexp.MustCompile(`^(.+) - Text - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "{Name} - Received - {Timestamp}.html".
	reReceived = regexp.MustCompile(`^(.+) - Received - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "{Name} - Placed - {Timestamp}.html".
	rePlaced = regexp.MustCompile(`^(.+) - Placed - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "{Name} - Missed - {Timestamp}.html".
	reMissed = regexp.MustCompile(`^(.+) - Missed - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "{Name} - Voicemail - {Timestamp}.html".
	reVoicemail = regexp.MustCompile(`^(.+) - Voicemail - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "Group Conversation - {Timestamp}.html".
	reGroup = regexp.MustCompile(`^Group Conversation - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
	// "{Name} - {Timestamp}.html" (call files without explicit type — classify from HTML title).
	reNameOnly = regexp.MustCompile(`^(.+) - (\d{4}-\d{2}-\d{2}T\d{2}_\d{2}_\d{2}Z?)\.html$`)
)

// classifyFile classifies a Google Voice Takeout filename.
// Returns the contact name (empty for groups), the file type, and any error.
// Returns an error for non-HTML files or files to skip (e.g., Bills.html).
func classifyFile(filename string) (name string, ft fileType, err error) {
	if !strings.HasSuffix(filename, ".html") {
		return "", 0, fmt.Errorf("not an HTML file: %s", filename)
	}

	// Skip known non-message files
	base := filename
	if strings.EqualFold(base, "Bills.html") {
		return "", 0, errors.New("skipping Bills.html")
	}

	if m := reText.FindStringSubmatch(base); m != nil {
		return m[1], fileTypeText, nil
	}
	if m := reReceived.FindStringSubmatch(base); m != nil {
		return m[1], fileTypeReceived, nil
	}
	if m := rePlaced.FindStringSubmatch(base); m != nil {
		return m[1], fileTypePlaced, nil
	}
	if m := reMissed.FindStringSubmatch(base); m != nil {
		return m[1], fileTypeMissed, nil
	}
	if m := reVoicemail.FindStringSubmatch(base); m != nil {
		return m[1], fileTypeVoicemail, nil
	}
	if m := reGroup.FindStringSubmatch(base); m != nil {
		_ = m[1] // timestamp
		return "", fileTypeGroup, nil
	}

	// Fallback: "{Name} - {Timestamp}.html" — these are call files without
	// the explicit type keyword. Return as unknown and let the caller
	// determine the type from the HTML <title>.
	if m := reNameOnly.FindStringSubmatch(base); m != nil {
		return m[1], fileTypePlaced, nil // default to placed, caller can override from HTML
	}

	return "", 0, fmt.Errorf("unrecognized filename pattern: %s", filename)
}

// parseVCF parses a Google Voice Phones.vcf file to extract phone numbers.
// The VCF uses itemN.TEL and itemN.X-ABLabel pairs where the label may
// appear before or after the TEL line, so we collect all items first.
func parseVCF(data []byte) (ownerPhones, error) {
	var phones ownerPhones
	scanner := bufio.NewScanner(bytes.NewReader(data))

	// Collect itemN.TEL and itemN.X-ABLabel pairs
	itemTels := make(map[string]string)   // "item1" -> phone
	itemLabels := make(map[string]string) // "item1" -> label

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Match itemN.TEL:value
		if idx := strings.Index(line, ".TEL:"); idx > 0 {
			prefix := line[:idx] // e.g., "item1"
			value := line[idx+5:]
			itemTels[prefix] = value
		}
		// Match itemN.X-ABLabel:value
		if idx := strings.Index(line, ".X-ABLabel:"); idx > 0 {
			prefix := line[:idx]
			value := line[idx+11:]
			itemLabels[prefix] = value
		}

		if raw, ok := strings.CutPrefix(line, "TEL;TYPE=CELL:"); ok {
			if p, err := textimport.NormalizePhone(raw); err == nil {
				phones.Cell = p
			}
		}
	}

	// Match items: find the TEL with "Google Voice" label
	for prefix, label := range itemLabels {
		if label == "Google Voice" {
			if tel, ok := itemTels[prefix]; ok {
				if p, err := textimport.NormalizePhone(tel); err == nil {
					phones.GoogleVoice = p
				}
				break
			}
		}
	}

	if phones.GoogleVoice == "" {
		return phones, errors.New("google Voice number not found in VCF")
	}

	return phones, scanner.Err()
}

// parseTextHTML parses a Google Voice text/SMS conversation HTML file.
// Returns the individual messages, group participant phones (if any), and any error.
func parseTextHTML(r io.Reader) ([]textMessage, []string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, nil, fmt.Errorf("parse HTML: %w", err)
	}

	var messages []textMessage
	var groupParticipants []string

	// Find participants div (group conversations)
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "participants") {
			// Extract phone numbers from participant links
			walkNodes(n, func(link *html.Node) bool {
				if link.Type == html.ElementNode && link.Data == "a" && hasClass(link, "tel") {
					href := getAttr(link, "href")
					if raw, ok := strings.CutPrefix(href, "tel:"); ok {
						if p, err := textimport.NormalizePhone(raw); err == nil {
							groupParticipants = append(groupParticipants, p)
						}
					}
				}
				return false
			})
		}
		return false
	})

	// Find message divs
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "message") {
			msg := parseMessageDiv(n)
			if !msg.Timestamp.IsZero() {
				messages = append(messages, msg)
			}
			return true // don't recurse into message divs
		}
		return false
	})

	return messages, groupParticipants, nil
}

// parseMessageDiv extracts a single textMessage from a div.message node.
func parseMessageDiv(div *html.Node) textMessage {
	var msg textMessage

	walkNodes(div, func(n *html.Node) bool {
		if n.Type != html.ElementNode {
			return false
		}

		switch {
		case n.Data == "abbr" && hasClass(n, "dt"):
			// Timestamp
			title := getAttr(n, "title")
			if t, err := time.Parse("2006-01-02T15:04:05.000-07:00", title); err == nil {
				msg.Timestamp = t.UTC()
			} else if t, err := time.Parse("2006-01-02T15:04:05.000Z", title); err == nil {
				msg.Timestamp = t.UTC()
			} else if t, err := time.Parse("2006-01-02T15:04:05-07:00", title); err == nil {
				msg.Timestamp = t.UTC()
			}

		case n.Data == "a" && hasClass(n, "tel"):
			// Sender phone
			href := getAttr(n, "href")
			if raw, ok := strings.CutPrefix(href, "tel:"); ok {
				if p, err := textimport.NormalizePhone(raw); err == nil {
					msg.SenderPhone = p
				}
			}
			// Sender name from child <span class="fn"> or <abbr class="fn">
			walkNodes(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && (child.Data == "span" || child.Data == "abbr") && hasClass(child, "fn") {
					name := textContent(child)
					if name == "Me" {
						msg.IsMe = true
						msg.SenderName = "Me"
					} else {
						msg.SenderName = name
					}
					return true
				}
				return false
			})

		case n.Data == "q":
			// Message body
			msg.Body = extractQBody(n)
			return true // don't recurse further

		case n.Data == "a" && hasClass(n, "video"):
			// Video attachment
			msg.Attachments = append(msg.Attachments, attachmentRef{
				HrefInHTML: getAttr(n, "href"),
				MediaType:  "video",
			})

		case n.Data == "img":
			// Image attachment (MMS)
			src := getAttr(n, "src")
			if src != "" {
				msg.Attachments = append(msg.Attachments, attachmentRef{
					HrefInHTML: src,
					MediaType:  "image",
				})
			}
		}

		return false
	})

	return msg
}

// extractQBody extracts text content from a <q> element, converting <br> to newlines.
func extractQBody(q *html.Node) string {
	var b strings.Builder
	for c := q.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			b.WriteString(c.Data)
		case c.Type == html.ElementNode && c.Data == "br":
			// Trailing <br> in GV HTML — only add newline if there's more content after
			if c.NextSibling != nil && (c.NextSibling.Type != html.TextNode || strings.TrimSpace(c.NextSibling.Data) != "") {
				b.WriteString("\n")
			}
		case c.Type == html.ElementNode:
			// Recurse for inline elements like <a>
			b.WriteString(textContent(c))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseCallHTML parses a Google Voice call log HTML file.
func parseCallHTML(r io.Reader) (*callRecord, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	record := &callRecord{}

	// Determine call type from title
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "title" {
			title := strings.ToLower(textContent(n))
			switch {
			case strings.Contains(title, "received"):
				record.CallType = fileTypeReceived
			case strings.Contains(title, "placed"):
				record.CallType = fileTypePlaced
			case strings.Contains(title, "missed"):
				record.CallType = fileTypeMissed
			case strings.Contains(title, "voicemail"):
				record.CallType = fileTypeVoicemail
			}
			return true
		}
		return false
	})

	// Find the haudio div
	walkNodes(doc, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "haudio") {
			// Extract phone and name from contributor vcard
			walkNodes(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && child.Data == "div" && hasClass(child, "contributor") {
					walkNodes(child, func(link *html.Node) bool {
						if link.Type == html.ElementNode && link.Data == "a" && hasClass(link, "tel") {
							href := getAttr(link, "href")
							if raw, ok := strings.CutPrefix(href, "tel:"); ok {
								if p, err := textimport.NormalizePhone(raw); err == nil {
									record.Phone = p
								}
							}
							walkNodes(link, func(fn *html.Node) bool {
								if fn.Type == html.ElementNode && fn.Data == "span" && hasClass(fn, "fn") {
									record.Name = textContent(fn)
									return true
								}
								return false
							})
						}
						return false
					})
					return true
				}
				return false
			})

			// Extract timestamp
			walkNodes(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && child.Data == "abbr" && hasClass(child, "published") {
					title := getAttr(child, "title")
					if t, err := time.Parse("2006-01-02T15:04:05.000-07:00", title); err == nil {
						record.Timestamp = t.UTC()
					}
					return true
				}
				return false
			})

			// Extract duration
			walkNodes(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && child.Data == "abbr" && hasClass(child, "duration") {
					record.Duration = getAttr(child, "title")
					return true
				}
				return false
			})

			// Extract labels
			walkNodes(n, func(child *html.Node) bool {
				if child.Type == html.ElementNode && child.Data == "div" && hasClass(child, "tags") {
					walkNodes(child, func(link *html.Node) bool {
						if link.Type == html.ElementNode && link.Data == "a" {
							label := strings.ToLower(textContent(link))
							record.Labels = append(record.Labels, label)
						}
						return false
					})
					return true
				}
				return false
			})

			return true
		}
		return false
	})

	if record.Phone == "" && record.Timestamp.IsZero() {
		return nil, errors.New("failed to parse call record")
	}

	return record, nil
}

// snippet returns the first n characters of s, suitable for message preview.
func snippet(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return s
}

// computeMessageID computes a deterministic 16-char hex ID from the given parts.
func computeMessageID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:8])
}

// computeThreadID computes the conversation thread ID for a set of participant phones.
// For 1:1 texts, uses the other party's normalized phone.
// For group texts, uses "group:" + sorted(all participant phones).
// For calls, uses "calls:" + normalizedPhone.
func computeThreadID(ownerCell string, ft fileType, contactPhone string, groupParticipants []string) string {
	switch ft {
	case fileTypeGroup:
		phones := make([]string, len(groupParticipants))
		copy(phones, groupParticipants)
		sort.Strings(phones)
		return "group:" + strings.Join(phones, ",")
	case fileTypeReceived, fileTypePlaced, fileTypeMissed, fileTypeVoicemail:
		return "calls:" + contactPhone
	default:
		// 1:1 text — use the other party's phone
		return contactPhone
	}
}

// HTML parsing helpers

// walkNodes recursively walks the HTML node tree, calling fn for each node.
// If fn returns true, the children of that node are skipped.
func walkNodes(n *html.Node, fn func(*html.Node) bool) {
	if fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, fn)
	}
}

// hasClass checks if an HTML element has the given class.
func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" && slices.Contains(strings.Fields(a.Val), class) {
			return true
		}
	}
	return false
}

// getAttr returns the value of the named attribute, or empty string.
func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textContent returns the concatenated text content of a node and its children.
func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textContent(c))
	}
	return strings.TrimSpace(b.String())
}
