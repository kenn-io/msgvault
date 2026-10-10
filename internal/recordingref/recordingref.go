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
	Kind                                   Kind
	Origin, RouteKey, Reference, Canonical string
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func (r Ref) ReferenceSHA256() string { return digest(r.Reference) }

var tokens = regexp.MustCompile(`(?i)https?://[^\s<>"\x60]+`)
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
	return Ref{Kind: kind, Origin: origin, RouteKey: digest(string(kind) + "\x00" + identity), Reference: raw, Canonical: canonical}, true
}

func Scan(text string, origins []string) []Ref {
	return scanProse(text, origins, nil)
}

func scanProse(text string, origins []string, exact map[string]bool) []Ref {
	allowed := originSet(origins)
	seen := make(map[string]bool)
	var refs []Ref
	for _, token := range tokens.FindAllString(text, -1) {
		for len(token) > 0 && (len(token) > 8192 || !exact[token]) && strings.ContainsAny(token[len(token)-1:], ".,;:!?)]}'") {
			token = token[:len(token)-1]
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

// ScanHTML sends archived anchor targets through the same offline admission rule.
func ScanHTML(body string, origins []string) []Ref {
	allowed := originSet(origins)
	z := html.NewTokenizer(strings.NewReader(body))
	var refs []Ref
	for {
		switch z.Next() {
		case html.ErrorToken:
			return refs
		case html.TextToken, html.EndTagToken, html.CommentToken, html.DoctypeToken:
			continue
		case html.StartTagToken, html.SelfClosingTagToken:
			name, more := z.TagName()
			if !bytes.EqualFold(name, []byte("a")) {
				continue
			}
			for more {
				key, value, next := z.TagAttr()
				more = next
				if bytes.EqualFold(key, []byte("href")) {
					if r, ok := admit(string(value), allowed); ok {
						refs = append(refs, r)
					}
					break
				}
			}
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
	return Ref{Kind: TeamsRecording, Origin: u.Scheme + "://" + u.Host, RouteKey: digest(string(TeamsRecording) + "\x00" + sourceAttachmentID), Reference: storagePath, Canonical: u.String()}, true
}
