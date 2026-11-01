// Package senderauth interprets caller-supplied authentication headers.
// Trust pins do not prove provenance: callers must ensure headers came from
// their trusted receiver. ARC assertions are retained as untrusted observations.
package senderauth

import (
	"net"
	"regexp"
	"strings"
)

type Status string

const (
	Unknown   Status = "unknown"
	Pass      Status = "pass"
	Fail      Status = "fail"
	Softfail  Status = "softfail"
	Neutral   Status = "neutral"
	None      Status = "none"
	Temperror Status = "temperror"
	Permerror Status = "permerror"
)

type Basis string

const (
	ProviderHeader  Basis = "provider_header"
	Untrusted       Basis = "authserv_id_untrusted"
	NoHeader        Basis = "no_header"
	HeaderTruncated Basis = "header_truncated"
	Unparseable     Basis = "unparseable"
	FromMismatch    Basis = "from_mismatch"
)

// Header is an unfolded or folded header value, in message order.
type Header struct{ Name, Value string }

// TrustSet contains exact authserv-ids or wildcard subdomains (*.example.test).
// An empty set trusts nothing.
type TrustSet []string

type Result struct {
	SPF, DKIM, DMARC Status
	Verified         bool
	VerifiedDomain   string
	AuthservID       string
	Basis            Basis
	// ObservedAuthservIDs and ARC contain untrusted message text.
	ObservedAuthservIDs []string
	ARC                 []string
}

var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

func domain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 253 || !domainPattern.MatchString(s) {
		return ""
	}
	for label := range strings.SplitSeq(s, ".") {
		if len(label) > 63 {
			return ""
		}
	}
	return s
}
func (t TrustSet) matches(id string) bool {
	for _, p := range t {
		p = strings.ToLower(strings.TrimSpace(p))
		if strings.HasPrefix(p, "*.") {
			suffix := domain(p[2:])
			if suffix != "" && strings.HasSuffix(id, "."+suffix) {
				return true
			}
		} else if d := domain(p); d != "" && id == d {
			return true
		}
	}
	return false
}
func status(s string) Status {
	switch Status(strings.ToLower(s)) {
	case Pass, Fail, Softfail, Neutral, None, Temperror, Permerror:
		return Status(strings.ToLower(s))
	default:
		return Unknown
	}
}

// clean removes comments and unfolds continuation lines while rejecting broken
// syntax. Quoted strings are preserved so separators inside them remain inert.
func clean(s string) (string, bool) {
	var b strings.Builder
	depth := 0
	quoted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' {
			if i+2 >= len(s) || s[i+1] != '\n' || (s[i+2] != ' ' && s[i+2] != '\t') {
				return "", false
			}
			i++
			if depth == 0 {
				b.WriteByte(' ')
			}
			continue
		}
		if c == '\n' || (c < 32 && c != '\t') || c == 127 {
			return "", false
		}
		if c == '\\' && quoted {
			if i+1 >= len(s) || s[i+1] < 32 || s[i+1] == 127 {
				return "", false
			}
			b.WriteByte(c)
			i++
			b.WriteByte(s[i])
			continue
		}
		if c == '"' && depth == 0 {
			quoted = !quoted
			b.WriteByte(c)
			continue
		}
		if quoted {
			b.WriteByte(c)
			continue
		}
		if c == '\\' && depth > 0 {
			i++
			if i >= len(s) || s[i] == '\r' || s[i] == '\n' {
				return "", false
			}
			continue
		}
		if c == '(' {
			if depth == 0 {
				b.WriteByte(' ')
			}
			depth++
			continue
		}
		if c == ')' {
			if depth == 0 {
				return "", false
			}
			depth--
			continue
		}
		if depth == 0 {
			b.WriteByte(c)
		}
	}
	return b.String(), depth == 0 && !quoted
}

// splitOutsideQuotes separates fields without interpreting quoted contents.
func splitOutsideQuotes(s string, separator func(byte) bool) []string {
	var parts []string
	start := 0
	quoted := false
	for i := 0; i < len(s); i++ {
		if quoted && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			quoted = !quoted
		} else if !quoted && separator(s[i]) {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// authservID reads only the leading identity; malformed content after the
// first separator cannot make an untrusted boundary part of the trusted block.
func authservID(value string) string {
	i, depth := 0, 0
	for ; i < len(value); i++ {
		switch value[i] {
		case '\\':
			if depth > 0 {
				i++
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return ""
			}
		case ';':
			if depth == 0 {
				goto found
			}
		}
	}
	return ""
found:
	prefix, ok := clean(value[:i])
	if !ok {
		return ""
	}
	fields := strings.Fields(prefix)
	if len(fields) < 1 || len(fields) > 2 {
		return ""
	}
	return domain(propertyValue(fields[0]))
}

type parsed struct {
	id     string
	values map[string]Status
	from   string
}

// assignments scans names and values separately so CFWS around operators is
// accepted without treating quoted text or mailbox punctuation as assignments.
func assignments(s string) ([]string, bool) {
	var fields []string
	i := 0
	space := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	name := func() string {
		start := i
		for i < len(s) && ((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') || (s[i] >= '0' && s[i] <= '9') || s[i] == '_' || s[i] == '-') {
			i++
		}
		return s[start:i]
	}
	for {
		space()
		if i == len(s) {
			return fields, true
		}
		key := name()
		if key == "" {
			return nil, false
		}
		space()
		if len(fields) == 0 && i < len(s) && s[i] == '/' {
			i++
			space()
			start := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if i == start {
				return nil, false
			}
			key += "/" + s[start:i]
			space()
		}
		if i < len(s) && s[i] == '.' {
			i++
			space()
			prop := name()
			if prop == "" {
				return nil, false
			}
			key += "." + prop
			space()
		}
		if i == len(s) || s[i] != '=' {
			return nil, false
		}
		i++
		space()
		start := i
		if i < len(s) && s[i] == '"' {
			if len(fields) == 0 {
				return nil, false
			}
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(s) {
				return nil, false
			}
			i++
			quotedEnd := i
			space()
			if i < len(s) && s[i] == '@' {
				i++
				space()
				domainStart := i
				for i < len(s) && s[i] != ' ' && s[i] != '\t' {
					i++
				}
				if domain(s[domainStart:i]) == "" {
					return nil, false
				}
			} else {
				i = quotedEnd
			}
		} else {
			for i < len(s) && s[i] != ' ' && s[i] != '\t' {
				if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-/=?^_`{|}~.@:", rune(s[i])) {
					return nil, false
				}
				i++
			}
		}
		if start == i || (i < len(s) && s[i] != ' ' && s[i] != '\t') {
			return nil, false
		}
		value := s[start:i]
		if !strings.HasPrefix(value, "\"") && strings.Contains(value, ":") && (len(fields) == 0 || net.ParseIP(value) == nil) {
			return nil, false
		}
		fields = append(fields, key+"="+value)
	}
}

// propertyValue decodes RFC quoted-pairs without interpreting Go escapes.
func propertyValue(value string) string {
	if !strings.HasPrefix(value, "\"") || !strings.HasSuffix(value, "\"") {
		return value
	}
	var decoded strings.Builder
	for i := 1; i < len(value)-1; i++ {
		if value[i] == '\\' {
			i++
		}
		decoded.WriteByte(value[i])
	}
	return decoded.String()
}

func parse(value string) (parsed, bool) {
	var p parsed
	s, ok := clean(value)
	if !ok {
		return p, false
	}
	parts := splitOutsideQuotes(s, func(c byte) bool { return c == ';' })
	if len(parts) < 2 {
		return p, false
	}
	idFields := strings.Fields(parts[0])
	if len(idFields) < 1 || len(idFields) > 2 {
		return p, false
	}
	p.id = domain(propertyValue(idFields[0]))
	if p.id == "" {
		return p, false
	}
	if len(idFields) == 2 && idFields[1] != "1" {
		return p, false
	}
	p.values = make(map[string]Status)
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "none") {
			if len(parts) != 2 {
				return p, false
			}
			continue
		}
		fields, valid := assignments(part)
		if !valid || len(fields) == 0 {
			return p, false
		}
		kv := strings.SplitN(fields[0], "=", 2)
		method, version, versioned := strings.Cut(strings.ToLower(kv[0]), "/")
		if versioned && strings.TrimLeft(version, "0") != "1" {
			continue
		}
		if strings.Contains(method, ".") {
			return p, false
		}
		if method == "spf" || method == "dkim" || method == "dmarc" {
			if _, exists := p.values[method]; !exists {
				p.values[method] = status(kv[1])
				if method == "dmarc" {
					for _, f := range fields[1:] {
						prop := strings.SplitN(f, "=", 2)
						if strings.EqualFold(prop[0], "header.from") {
							if p.from != "" {
								return p, false
							}
							value := propertyValue(prop[1])
							if value != strings.TrimSpace(value) {
								return p, false
							}
							p.from = domain(value)
							if p.from == "" {
								return p, false
							}
						}
					}
				}
			}
		}
	}
	return p, true
}

// Parse merges the contiguous trusted Authentication-Results headers, stopping
// at the first untrusted header. fromDomain is the actual, independently parsed
// From domain. Only exact domain equality verifies; relaxed DMARC alignment may
// therefore produce false negatives. truncated must be true for an incomplete
// outer header block. ARC headers are observations and never verify a message.
func Parse(headers []Header, trusted TrustSet, fromDomain string, truncated bool) Result {
	r := Result{SPF: Unknown, DKIM: Unknown, DMARC: Unknown, Basis: NoHeader}
	for _, h := range headers {
		if strings.EqualFold(h.Name, "ARC-Authentication-Results") {
			r.ARC = append(r.ARC, h.Value)
		}
	}
	if truncated {
		r.Basis = HeaderTruncated
		return r
	}
	seen := map[string]bool{}
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "Authentication-Results") {
			continue
		}
		id := authservID(h.Value)
		if id == "" {
			if r.AuthservID == "" {
				r.Basis = Unparseable
			}
			break
		}
		if !trusted.matches(id) {
			r.ObservedAuthservIDs = append(r.ObservedAuthservIDs, id)
			if r.AuthservID == "" {
				r.Basis = Untrusted
			}
			break
		}
		p, ok := parse(h.Value)
		if !ok {
			r = Result{SPF: Unknown, DKIM: Unknown, DMARC: Unknown, Basis: Unparseable, ARC: r.ARC}
			return r
		}
		if !trusted.matches(p.id) {
			r.ObservedAuthservIDs = append(r.ObservedAuthservIDs, p.id)
			if r.AuthservID == "" {
				r.Basis = Untrusted
			}
			break
		}
		if r.AuthservID == "" {
			r.AuthservID = p.id
			r.Basis = ProviderHeader
		}
		for _, method := range []string{"spf", "dkim", "dmarc"} {
			if v, exists := p.values[method]; exists && !seen[method] {
				seen[method] = true
				switch method {
				case "spf":
					r.SPF = v
				case "dkim":
					r.DKIM = v
				case "dmarc":
					r.DMARC = v
					if v == Pass {
						from := domain(fromDomain)
						if from != "" && p.from != "" && from == p.from {
							r.Verified = true
							r.VerifiedDomain = from
						} else {
							r.Basis = FromMismatch
						}
					}
				}
			}
		}
	}
	return r
}
