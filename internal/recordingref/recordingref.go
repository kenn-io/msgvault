// Package recordingref admits recording references without network access.
package recordingref

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/idna"
)

type Kind string

const (
	Loom           Kind = "loom"
	CapCloud       Kind = "cap_cloud"
	CapSelfHosted  Kind = "cap_self_hosted"
	TeamsRecording Kind = "teams_recording"
)

// Ref URLs contain access material and must stay in memory.
type Ref struct {
	RouteKey, Reference, Canonical string
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func (r Ref) ReferenceSHA256() string { return digest(r.Reference) }

var tokens = regexp.MustCompile(`(?i)https?://[^\s<>"\x60]+`)
var markdownLinks = regexp.MustCompile(`(?i)\[(?:[^\[\]\n]|\[[^\[\]\n]*\])*\]\((https?://(?:[^()\s<>]|\([^()\s<>]*\))*)(?:[ \t]+(?:"[^"\n]*"|'[^'\n]*'|\([^()\n]*\)))?[ \t]*\)`)
var recordingPath = regexp.MustCompile(`^/(share|embed|s|dev)/([^/]+)$`)

func parse(raw string) (*url.URL, error) {
	if len(raw) > 8192 {
		return nil, errors.New("recording URL exceeds limit")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("invalid recording URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("invalid recording scheme")
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) == nil {
		host, err = idna.Lookup.ToASCII(host)
	}
	if err != nil || host == "" {
		return nil, errors.New("invalid recording host")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid recording port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid recording port")
		}
		port = strconv.Itoa(n)
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

func CanonicalOrigin(raw string) (string, error) {
	u, err := parse(raw)
	if err != nil {
		return "", err
	}
	if u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", errors.New("recording origin must contain only scheme and host")
	}
	return u.Scheme + "://" + u.Host, nil
}

func originSet(origins []string) map[string]bool {
	allowed := make(map[string]bool, len(origins))
	for _, raw := range origins {
		if origin, err := CanonicalOrigin(raw); err == nil {
			allowed[origin] = true
		}
	}
	return allowed
}

func admit(raw string, allowed map[string]bool) (Ref, bool) {
	u, err := parse(raw)
	if err != nil {
		return Ref{}, false
	}
	path := recordingPath.FindStringSubmatch(u.EscapedPath())
	if path == nil || u.EscapedPath() != u.Path {
		return Ref{}, false
	}
	origin := u.Scheme + "://" + u.Host
	var kind Kind
	switch {
	case u.Scheme == "https" && (u.Host == "loom.com" || u.Host == "www.loom.com") && (path[1] == "share" || path[1] == "embed"):
		kind = Loom
	case u.Scheme == "https" && (u.Host == "cap.so" || u.Host == "www.cap.so") && path[1] != "share":
		kind = CapCloud
	case allowed[origin] && path[1] != "share":
		kind = CapSelfHosted
	default:
		return Ref{}, false
	}
	canonical := origin + u.EscapedPath()
	identity := canonical
	if kind == Loom {
		identity = "loom:" + path[1] + "/" + path[2]
	}
	if kind == CapCloud {
		identity = "cap:" + path[2]
	}
	if kind == CapSelfHosted {
		canonical = ""
	}
	return Ref{RouteKey: digest(string(kind) + "\x00" + identity), Reference: raw, Canonical: canonical}, true
}

func scanProse(text string, allowed map[string]bool, exact map[string]bool) []Ref {
	seen := make(map[string]bool)
	var refs []Ref
	text = markdownLinks.ReplaceAllStringFunc(text, func(link string) string {
		target := markdownLinks.FindStringSubmatch(link)[1]
		if r, ok := admit(target, allowed); ok && !exact[target] && !seen[r.RouteKey] {
			refs = append(refs, r)
			seen[r.RouteKey] = true
		}
		return " "
	})
	for _, match := range tokens.FindAllStringIndex(text, -1) {
		token := text[match[0]:match[1]]
		if !exact[token] {
			before := text[:match[0]]
			var closing []string
			for {
				matched := false
				for _, marker := range []string{"***", "___", "**", "__", "~~", "||", "*", "_"} {
					if strings.HasSuffix(before, marker) {
						closing = append(closing, marker)
						before = strings.TrimSuffix(before, marker)
						matched = true
						break
					}
				}
				if !matched {
					break
				}
			}
			suffix := strings.Join(closing, "")
			for len(token) > 0 && (len(token) > 8192 || !exact[token]) && (suffix == "" || !strings.HasSuffix(token, suffix)) {
				last, size := utf8.DecodeLastRuneInString(token)
				if !strings.ContainsRune(".,;:!?)]}'”’»›…", last) {
					break
				}
				token = token[:len(token)-size]
			}
			if !exact[token] && suffix != "" && strings.HasSuffix(token, suffix) {
				token = strings.TrimSuffix(token, suffix)
				for len(token) > 0 && (len(token) > 8192 || !exact[token]) {
					last, size := utf8.DecodeLastRuneInString(token)
					if !strings.ContainsRune(".,;:!?)]}'”’»›…", last) {
						break
					}
					token = token[:len(token)-size]
				}
			}
		}
		if len(token) <= 8192 && exact[token] {
			continue
		}
		if r, ok := admit(token, allowed); ok && !seen[r.RouteKey] {
			refs = append(refs, r)
			seen[r.RouteKey] = true
		}
	}
	return refs
}

func scanHTML(body string, allowed map[string]bool) ([]Ref, string) {
	z := html.NewTokenizer(strings.NewReader(body))
	var refs []Ref
	var prose strings.Builder
	inAnchor := false
	for {
		token := z.Next()
		switch token {
		case html.ErrorToken:
			return refs, prose.String()
		case html.TextToken, html.CommentToken, html.DoctypeToken:
		case html.EndTagToken:
			name, _ := z.TagName()
			if bytes.EqualFold(name, []byte("a")) {
				inAnchor = false
				prose.WriteByte(' ')
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, more := z.TagName()
			if !bytes.EqualFold(name, []byte("a")) {
				break
			}
			inAnchor = false
			for more {
				key, value, next := z.TagAttr()
				more = next
				if bytes.EqualFold(key, []byte("href")) {
					inAnchor = len(value) > 0 && token != html.SelfClosingTagToken
					if r, ok := admit(strings.Trim(string(value), " \t\r\n\f"), allowed); ok {
						refs = append(refs, r)
					}
					break
				}
			}
			prose.WriteByte(' ')
			continue
		}
		if !inAnchor {
			prose.Write(z.Raw())
		}
	}
}

func TeamsPointer(sourceAttachmentID, storagePath string) (Ref, bool) {
	if !strings.HasPrefix(sourceAttachmentID, "teams:recording:") {
		return Ref{}, false
	}
	u, err := parse(storagePath)
	if err != nil {
		return Ref{}, false
	}
	return Ref{RouteKey: digest(string(TeamsRecording) + "\x00" + sourceAttachmentID), Reference: storagePath, Canonical: u.String()}, true
}
