package tumblr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var headerRE = regexp.MustCompile(`^(\S+)\s+(\S+)\s+@\s*(.*)$`)
var dateRE = regexp.MustCompile(`^(.*?)\s*\(([^()]*)\)$`)

func parseConversation(ctx context.Context, data []byte) (*Conversation, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("tumblr: conversation HTML is not valid UTF-8")
	}
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("tumblr: parse conversation HTML: %w", err)
	}
	c := &Conversation{}
	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Type == html.ElementNode {
			if n.Data == "h2" {
				text := strings.TrimSpace(nodeText(n))
				if id, ok := strings.CutPrefix(text, "Conversation ID:"); ok {
					c.ID = strings.TrimSpace(id)
				} else if c.Status == "" && text != "Participants:" {
					c.Status = text
				}
			}
			if n.Data == "div" && hasClass(n, "container") {
				if hasClass(n, "participant") {
					p := Participant{}
					for child := n.FirstChild; child != nil; child = child.NextSibling {
						text := strings.TrimSpace(nodeText(child))
						switch {
						case strings.HasPrefix(text, "Blog Name:"):
							p.Name = strings.TrimSpace(strings.TrimPrefix(text, "Blog Name:"))
						case strings.HasPrefix(text, "Blog ID:"):
							p.ID = strings.TrimSpace(strings.TrimPrefix(text, "Blog ID:"))
						case strings.HasPrefix(text, "Status:"):
							p.Status = strings.TrimSpace(strings.TrimPrefix(text, "Status:"))
						}
					}
					c.Participants = append(c.Participants, p)
				} else {
					info := findInfo(n)
					if info == nil {
						c.UnknownBlocks++
					} else if m, ok := parseMessage(n, info); ok {
						c.Messages = append(c.Messages, m)
					} else {
						c.Malformed++
					}
				}
				return nil
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, err
	}
	if c.ID == "" {
		return nil, errors.New("tumblr: missing Conversation ID")
	}
	return c, nil
}
func hasClass(n *html.Node, name string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), name)
}
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
func findInfo(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "span" && hasClass(n, "message-info") {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findInfo(c); found != nil {
			return found
		}
	}
	return nil
}
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "img", "audio", "video", "source":
				return
			case "br":
				b.WriteByte('\n')
				return
			}
			if hasClass(n, "message-info") {
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && (n.Data == "p" || n.Data == "div") {
			b.WriteByte('\n')
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
func parseMessage(n, info *html.Node) (Message, bool) {
	// Headers are evidence, so read the span's text without the body exclusion.
	var b strings.Builder
	for c := info.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeText(c))
	}
	m := Message{Header: strings.TrimSpace(b.String())}
	parts := headerRE.FindStringSubmatch(m.Header)
	if parts == nil {
		return m, false
	}
	m.Sender, m.Type, m.DateText = parts[1], parts[2], parts[3]
	if d := dateRE.FindStringSubmatch(m.DateText); d != nil {
		m.DateText, m.Epoch = strings.TrimSpace(d[1]), strings.TrimSpace(d[2])
	}
	date, err := time.Parse("2006-01-02 15:04:05 GMT", m.DateText)
	if err == nil && date.Year() > 0 {
		m.SentAt.Time, m.SentAt.Valid = date, true
	}
	epoch, e := strconv.ParseInt(m.Epoch, 10, 64)
	validEpoch := e == nil && epoch >= -62135596800 && epoch <= 253402300799
	if validEpoch && !m.SentAt.Valid {
		m.SentAt.Time, m.SentAt.Valid = time.Unix(epoch, 0).UTC(), true
	}
	m.DateConflict = validEpoch && m.SentAt.Valid && m.SentAt.Time.Unix() != epoch
	m.SentAt.Valid = m.SentAt.Valid && !m.DateConflict
	m.Text = nodeText(n)
	var raw bytes.Buffer
	_ = html.Render(&raw, n)
	m.HTML = raw.String()
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "script" || n.Data == "style" || hasClass(n, "message-info") {
				return
			}
			switch n.Data {
			case "img", "audio", "video", "source":
				if src := attr(n, "src"); src != "" {
					m.Media = append(m.Media, src)
				}
			case "a":
				href := attr(n, "href")
				if m.Type == "POSTREF" {
					m.Targets = append(m.Targets, href)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return m, true
}
