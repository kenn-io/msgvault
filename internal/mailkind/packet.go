package mailkind

import (
	"errors"
	"io"
	"mime"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/mailrecords"
	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/verification"
	"golang.org/x/net/html"
	"golang.org/x/net/idna"
)

// Input is local-only evidence. Only BuildPacket's fixed projection may leave
// the archive. Attachments contains MIME types, never filenames or bytes.
type Input struct {
	MessageType, Subject, Snippet, BodyText, BodyHTML string
	SenderEmail, SenderPhone, ListID                  string
	AttachmentMIMETypes                               []string
	IsFromMe                                          bool
	SentAt                                            time.Time
}

// Packet is the complete mail-kind-packet-v1 disclosure. It carries no archive
// IDs, recipients, attachment names, links extracted from HTML, or raw markup.
type Packet struct {
	SchemaVersion string         `json:"schema_version"`
	SenderDomain  string         `json:"sender_domain"`
	Subject       string         `json:"subject"`
	Excerpt       string         `json:"excerpt"`
	Attachments   map[string]int `json:"attachments_by_mime_type"`
	Tier0Types    []string       `json:"tier0_types"`
}

// BuildPacket fails closed for non-mail, detected credentials, oversized or
// incompletely segmented content, and messages with a deterministic kind.
// Quote segmentation recognizes markers; it cannot prove authorship.
func BuildPacket(in Input) (Packet, bool) {
	if (in.MessageType != "" && in.MessageType != "email") || in.IsFromMe ||
		len(in.BodyText) > msgsegment.MaxBodyBytes || len(in.BodyHTML) > msgsegment.MaxBodyBytes ||
		len(in.Subject) > 16<<10 || len(in.AttachmentMIMETypes) > 1000 {
		return Packet{}, false
	}
	if verification.Detect(verification.Input{
		MessageType: in.MessageType, Subject: in.Subject, Snippet: in.Snippet,
		BodyText: in.BodyText, BodyHTML: in.BodyHTML, SenderEmail: in.SenderEmail,
		SenderPhone: in.SenderPhone, ListID: in.ListID, IsFromMe: in.IsFromMe, SentAt: in.SentAt,
	}) != nil {
		return Packet{}, false
	}
	records := mailrecords.Extract(mailrecords.Input{HTML: in.BodyHTML, Text: in.BodyText})
	if records.Kind != "none" && records.Kind != "" {
		return Packet{}, false
	}
	body, format := in.BodyText, msgsegment.Text
	if body == "" {
		body, format = in.BodyHTML, msgsegment.HTML
	}
	segments := msgsegment.Split(body, format, msgsegment.Options{Subject: in.Subject})
	if segments.Truncated {
		return Packet{}, false
	}
	excerpt := segments.AuthoredText()
	if format == msgsegment.HTML {
		var complete bool
		excerpt, complete = htmlText(excerpt)
		if !complete {
			return Packet{}, false
		}
		// The HTML segmenter marks structural quotes. Apply text signature
		// markers after rendering the remaining ranges.
		excerpt = msgsegment.Split(excerpt, msgsegment.Text, msgsegment.Options{Subject: in.Subject}).AuthoredText()
	}
	packet := Packet{SchemaVersion: PacketSchema, SenderDomain: senderDomain(in.SenderEmail),
		Subject: boundedMasked(in.Subject, 256), Excerpt: boundedMasked(excerpt, 1500),
		Attachments: map[string]int{}, Tier0Types: []string{}}
	for _, value := range in.AttachmentMIMETypes {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil || len(mediaType) > 127 || !strings.Contains(mediaType, "/") {
			mediaType = "application/octet-stream"
		}
		packet.Attachments[strings.ToLower(mediaType)]++
	}
	for _, record := range records.Records {
		packet.Tier0Types = append(packet.Tier0Types, record.Type)
	}
	slices.Sort(packet.Tier0Types)
	packet.Tier0Types = slices.Compact(packet.Tier0Types)
	return packet, packet.Subject != "" || packet.Excerpt != ""
}

func senderDomain(value string) string {
	address, err := mail.ParseAddress(value)
	if err != nil {
		return ""
	}
	i := strings.LastIndexByte(address.Address, '@')
	if i < 0 {
		return ""
	}
	domain, err := idna.Lookup.ToASCII(address.Address[i+1:])
	if err != nil || len(domain) > 253 || strings.ContainsAny(domain, "[]:/\\ \t\r\n") {
		return ""
	}
	return strings.ToLower(domain)
}

var emailInText = regexp.MustCompile(`(?:"(?:[^"\\]|\\.)+"|[^\s@<>()\[\]"\\,;:]+)@((?:\[[^\]\s]+\])|(?:[\pL\pN](?:[\pL\pN\pM.-]*[\pL\pN\pM])?))`)

func boundedMasked(value string, limit int) string {
	value = emailInText.ReplaceAllStringFunc(strings.ToValidUTF8(value, ""), func(match string) string {
		_, domain, _ := strings.Cut(match, "@")
		if strings.HasPrefix(domain, "[") {
			return "[local]@[address]"
		}
		return "[local]@" + domain
	})
	runes := []rune(value)
	var b strings.Builder
	for i := 0; i < len(runes); {
		if unicode.IsDigit(runes[i]) {
			end := i + 1
			for end < len(runes) && unicode.IsDigit(runes[end]) {
				end++
			}
			if end-i >= 4 {
				b.WriteString("[digits]")
			} else {
				b.WriteString(string(runes[i:end]))
			}
			i = end
			continue
		}
		if !unicode.IsControl(runes[i]) || unicode.IsSpace(runes[i]) {
			b.WriteRune(runes[i])
		}
		i++
	}
	value = strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(value) > limit {
		value = string([]rune(value)[:limit])
	}
	return value
}

// htmlText renders text nodes only and never resolves URLs. Work is bounded
// independently of the body byte limit; incomplete markup is not disclosed.
func htmlText(body string) (string, bool) {
	z := html.NewTokenizer(strings.NewReader(body))
	var b strings.Builder
	var suppressed []string
	inHead := false
	for range 16384 {
		typ := z.Next()
		if typ == html.ErrorToken {
			return b.String(), errors.Is(z.Err(), io.EOF)
		}
		token := z.Token()
		switch typ {
		case html.StartTagToken:
			if token.Data == "head" {
				inHead = true
			} else if closesHeadImplicitly(token.Data) {
				inHead = false
			}
			if slices.Contains([]string{"script", "style", "template", "title"}, token.Data) {
				suppressed = append(suppressed, token.Data)
			}
		case html.EndTagToken:
			if token.Data == "head" {
				inHead = false
			}
			if len(suppressed) > 0 && suppressed[len(suppressed)-1] == token.Data {
				suppressed = suppressed[:len(suppressed)-1]
			}
		case html.TextToken:
			if len(suppressed) > 0 {
				continue
			}
			if inHead {
				if strings.TrimSpace(token.Data) == "" {
					continue
				}
				inHead = false
			}
			b.WriteString(token.Data)
		default:
		}
		if len(suppressed) == 0 && (typ == html.StartTagToken || typ == html.EndTagToken || typ == html.SelfClosingTagToken) {
			switch token.Data {
			case "br", "p", "div", "tr", "li", "hr":
				b.WriteByte('\n')
			}
		}
	}
	return "", false
}

func closesHeadImplicitly(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "body", "center", "details", "dialog", "dir", "div", "dl", "dt", "dd",
		"fieldset", "figcaption", "figure", "footer", "form", "header", "hgroup", "html", "legend", "main", "menu", "nav", "search", "section", "summary",
		"p", "pre", "listing", "plaintext", "xmp", "h1", "h2", "h3", "h4", "h5", "h6", "br", "hr", "li", "ul", "ol",
		"table", "caption", "colgroup", "tbody", "thead", "tfoot", "tr", "td", "th":
		return true
	default:
		return false
	}
}
