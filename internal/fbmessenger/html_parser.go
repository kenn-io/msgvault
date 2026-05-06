package fbmessenger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// maxBodyLinesBeforeTimestamp limits the look-ahead window when scanning
// for a timestamp line after a sender name in HTML message blocks.
const maxBodyLinesBeforeTimestamp = 64

// htmlTimeLayouts are the candidate layouts the HTML parser tries when
// interpreting a timestamp string. All parsed times are treated as UTC;
// Facebook HTML exports do not expose a timezone (plan D6).
var htmlTimeLayouts = []string{
	"Jan 2, 2006, 3:04 PM",
	"Jan 2, 2006 at 3:04 PM",
	"2 Jan 2006, 15:04",
	"Jan 2, 2006, 3:04:05 PM",
}

// parseHTMLTimestamp parses a stamp string with any of the known layouts,
// returning the time in UTC. Returns the zero value and false on failure.
func parseHTMLTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range htmlTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ParseHTMLThread parses a DYI HTML thread directory and returns a Thread.
func ParseHTMLThread(rootDir, threadDir string) (*Thread, error) {
	entries, err := os.ReadDir(threadDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: read html thread dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "message_") && strings.HasSuffix(name, ".html") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("fbmessenger: no message_*.html files in %s", threadDir)
	}
	sort.Strings(files)

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("fbmessenger: abs root: %w", err)
	}

	var thread Thread
	thread.Format = "html"
	thread.DirName = filepath.Base(threadDir)

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(threadDir, name))
		if err != nil {
			return nil, fmt.Errorf("fbmessenger: read %s: %w", name, err)
		}
		thread.RawBytes = append(thread.RawBytes, data...)
		doc, err := html.Parse(strings.NewReader(string(data)))
		if err != nil {
			return nil, fmt.Errorf("fbmessenger: parse %s: %w", name, err)
		}
		if title := extractHTMLTitle(doc); title != "" && thread.Title == "" {
			thread.Title = title
		}
		lines, images := collectHTMLLines(doc)
		absThreadDir, _ := filepath.Abs(threadDir)
		participants, msgs := parseHTMLLines(lines, images, absRoot, absThreadDir)
		if len(thread.Participants) == 0 {
			thread.Participants = participants
		}
		base := len(thread.Messages)
		for i := range msgs {
			msgs[i].Index = base + i
		}
		thread.Messages = append(thread.Messages, msgs...)
	}

	if len(thread.Participants) <= 2 {
		thread.ConvType = "direct_chat"
	} else {
		thread.ConvType = "group_chat"
	}

	sort.SliceStable(thread.Messages, func(i, j int) bool {
		if thread.Messages[i].SentAt.IsZero() || thread.Messages[j].SentAt.IsZero() {
			return thread.Messages[i].Index < thread.Messages[j].Index
		}
		return thread.Messages[i].SentAt.Before(thread.Messages[j].SentAt)
	})
	// Re-number indices after sort so Index remains monotonic.
	for i := range thread.Messages {
		thread.Messages[i].Index = i
	}
	return &thread, nil
}

// extractHTMLTitle finds the document <title> text.
func extractHTMLTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "title" && n.FirstChild != nil {
		return strings.TrimSpace(n.FirstChild.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := extractHTMLTitle(c); t != "" {
			return t
		}
	}
	return ""
}

// htmlImageRef records an <img> src and the index in the lines array
// where it appears, so parseHTMLLines can associate images with the
// correct message block.
type htmlImageRef struct {
	Src     string
	LineIdx int // index into the lines slice at point of encounter
}

// collectHTMLLines walks the body of the document and returns a flat list
// of logical text lines plus positioned image references.
//
// A "line" is the concatenated text content of a leaf block-level element
// (div/p/span when it contains no block-level descendants). Whitespace
// runs inside a line are collapsed.
//
// Images are recorded with the line index at the time they are encountered
// in document order, so callers can associate each image with the message
// block whose line range contains it.
func collectHTMLLines(doc *html.Node) ([]string, []htmlImageRef) {
	var body *html.Node
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if body != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if body == nil {
		body = doc
	}

	var lines []string
	var images []htmlImageRef

	// collectImgs records all <img> elements under n at the current line index.
	var collectImgs func(n *html.Node)
	collectImgs = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, a := range n.Attr {
				if a.Key == "src" {
					images = append(images, htmlImageRef{Src: a.Val, LineIdx: len(lines)})
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collectImgs(c)
		}
	}

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		// Pick up images that live outside leaf blocks (e.g. between divs).
		if n.Type == html.ElementNode && n.Data == "img" {
			for _, a := range n.Attr {
				if a.Key == "src" {
					images = append(images, htmlImageRef{Src: a.Val, LineIdx: len(lines)})
					break
				}
			}
			return
		}
		if n.Type == html.ElementNode && isLeafBlock(n) {
			// Collect images inside the leaf block at the current line position.
			collectImgs(n)
			text := collapseWhitespace(textContent(n))
			if text != "" {
				lines = append(lines, text)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	return lines, images
}

// isLeafBlock returns true for block-level elements that contain no
// block-level descendants. The DOM walker treats such elements as the
// boundary of one logical text line.
func isLeafBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "div", "p", "li", "td", "h1", "h2", "h3", "h4":
		// fall through
	default:
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			switch c.Data {
			case "div", "p", "li", "td", "h1", "h2", "h3", "h4":
				return false
			}
			if hasBlockDescendant(c) {
				return false
			}
		}
	}
	return true
}

func hasBlockDescendant(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			switch c.Data {
			case "div", "p", "li", "td", "h1", "h2", "h3", "h4":
				return true
			}
			if hasBlockDescendant(c) {
				return true
			}
		}
	}
	return false
}

// textContent returns the concatenation of text nodes under n.
func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textContent(c))
	}
	return b.String()
}

var wsRun = regexp.MustCompile(`[ \t\f\v]+`)

func collapseWhitespace(s string) string {
	s = html.UnescapeString(s)
	// Collapse horizontal whitespace runs but preserve newlines so body
	// text keeps paragraph breaks.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(wsRun.ReplaceAllString(line, " "))
	}
	// Rejoin and trim leading/trailing blank lines.
	joined := strings.Join(lines, "\n")
	joined = strings.Trim(joined, "\n")
	return joined
}

// parseHTMLLines scans the flat text-line output of collectHTMLLines,
// extracts the participants header, and walks the remaining lines
// collecting (sender, body, timestamp) triples. Images are assigned to
// the message block whose line range contains the image's DOM position.
func parseHTMLLines(lines []string, images []htmlImageRef, absRoot, htmlDir string) ([]Participant, []Message) {
	var participants []Participant
	participantNames := make(map[string]bool)
	// remainingStart is the offset into lines where message blocks begin
	// (after the Participants: header). Image LineIdx values are relative
	// to the original lines slice, so we track this offset.
	remainingStart := 0
	remaining := lines
	for i, ln := range lines {
		if strings.HasPrefix(ln, "Participants:") {
			rest := strings.TrimSpace(strings.TrimPrefix(ln, "Participants:"))
			for _, part := range strings.Split(rest, ",") {
				name := strings.TrimSpace(part)
				if name == "" {
					continue
				}
				if !participantNames[name] {
					participantNames[name] = true
					participants = append(participants, Participant{Name: name})
				}
			}
			remaining = lines[i+1:]
			remainingStart = i + 1
			break
		}
	}

	// imagesInRange returns images whose LineIdx falls in [loLine, hiLine).
	// LineIdx values are in the original lines coordinate space.
	imagesInRange := func(loLine, hiLine int) []htmlImageRef {
		var out []htmlImageRef
		for _, img := range images {
			if img.LineIdx >= loLine && img.LineIdx < hiLine {
				out = append(out, img)
			}
		}
		return out
	}

	var messages []Message
	// Scan for message blocks. A message is a window that starts with a
	// participant name line, ends with a timestamp line, and has zero or
	// more body lines in between.
	i := 0
	for i < len(remaining) {
		sender := remaining[i]
		if !participantNames[sender] {
			i++
			continue
		}
		// Find the next timestamp line.
		end := -1
		nextSender := -1
		for j := i + 1; j < len(remaining) && j < i+1+maxBodyLinesBeforeTimestamp; j++ {
			if _, ok := parseHTMLTimestamp(remaining[j]); ok {
				end = j
				break
			}
			// If we hit another sender name before a timestamp, this
			// block lacks a timestamp; bail out gracefully and resume
			// scanning at that candidate rather than advancing one line
			// at a time through the failed window.
			if participantNames[remaining[j]] {
				nextSender = j
				break
			}
		}
		if end == -1 {
			if nextSender > i {
				i = nextSender
			} else {
				i++
			}
			continue
		}
		bodyLines := remaining[i+1 : end]
		body := strings.Join(bodyLines, "\n")
		ts, _ := parseHTMLTimestamp(remaining[end])

		msg := Message{
			SenderName: sender,
			SentAt:     ts,
			Body:       body,
		}

		// Attach images whose DOM position falls within this message
		// block's line range [sender line .. timestamp line].
		blockImages := imagesInRange(remainingStart+i, remainingStart+end+1)
		for _, img := range blockImages {
			msg.Attachments = append(msg.Attachments, makeHTMLAttachment(absRoot, htmlDir, img.Src))
		}
		if len(blockImages) > 0 && body == "" {
			msg.Body = "[photo]"
		}

		messages = append(messages, msg)
		i = end + 1
	}
	return participants, messages
}

func makeHTMLAttachment(absRoot, htmlDir, src string) Attachment {
	// HTML attachment src is relative to the HTML file directory.
	// We resolve against htmlDir, but require the result to stay inside
	// absRoot so a malicious export cannot read arbitrary files.
	abs := ""
	if !filepath.IsAbs(src) {
		cleaned := filepath.Clean(src)
		if !strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) && cleaned != ".." {
			candidate := filepath.Join(htmlDir, cleaned)
			absCandidate, err := filepath.Abs(candidate)
			if err == nil && (absCandidate == absRoot || strings.HasPrefix(absCandidate, absRoot+string(filepath.Separator))) {
				abs = absCandidate
			}
		}
	}
	return Attachment{
		URI:      src,
		AbsPath:  abs,
		Kind:     "photo",
		Filename: filepath.Base(src),
		MimeType: guessMime(src),
	}
}
