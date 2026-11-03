// Package senderauth reports authentication results from a pinned receiver.
package senderauth

import (
	"bufio"
	"bytes"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/emersion/go-msgauth/authres"
)

// Verdict describes the pinned receiver's results. Verified covers the From domain only.
type Verdict struct {
	Verified   bool   `json:"verified"`
	Domain     string `json:"domain,omitempty"`
	AuthservID string `json:"authserv_id,omitempty"`
	SPF        string `json:"spf,omitempty"`
	DKIM       string `json:"dkim,omitempty"`
	DMARC      string `json:"dmarc,omitempty"`
}

// Evaluate reads only the topmost Authentication-Results header.
func Evaluate(block []byte, trusted []string, displayedFrom string) *Verdict {
	verdict := &Verdict{}
	headers, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(block))).ReadMIMEHeader()
	if err != nil {
		return verdict
	}
	cleaned, ok := cleanResults(headers.Get("Authentication-Results"))
	if !ok || cleaned == "" {
		return verdict
	}
	id, results, err := authres.Parse(cleaned)
	if err != nil {
		return verdict
	}
	pinned := false
	for _, pin := range trusted {
		if strings.EqualFold(id, pin) {
			pinned = true
			break
		}
	}
	if !pinned {
		return verdict
	}
	verdict.AuthservID = id
	seenSPF, seenDKIM, seenDMARC := false, false, false
	for _, result := range results {
		switch r := result.(type) {
		case *authres.SPFResult:
			if !seenSPF {
				seenSPF = true
				verdict.SPF = string(r.Value)
			}
		case *authres.DKIMResult:
			if !seenDKIM {
				seenDKIM = true
				verdict.DKIM = string(r.Value)
			}
		case *authres.DMARCResult:
			if !seenDMARC {
				seenDMARC = true
				verdict.DMARC = string(r.Value)
				verdict.Domain = strings.ToLower(r.From)
			}
		}
	}
	from, err := mail.ParseAddressList(headers.Get("From"))
	if err == nil && len(from) == 1 && len(headers.Values("From")) == 1 {
		domain := addressDomain(from[0].Address)
		verdict.Verified = verdict.DMARC == "pass" && domain != "" && strings.EqualFold(verdict.Domain, domain) && strings.EqualFold(domain, addressDomain(displayedFrom))
	}
	return verdict
}

func addressDomain(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		return address[i+1:]
	}
	return ""
}

// authres splits on semicolons without recognizing comments or quotes.
// Remove nested comments and refuse quoted semicolons before handing it a header.
func cleanResults(value string) (string, bool) {
	var out strings.Builder
	depth := 0
	quoted, escaped := false, false
	for _, c := range value {
		if escaped {
			if depth == 0 {
				if c == ';' {
					return "", false
				}
				out.WriteRune(c)
			}
			escaped = false
			continue
		}
		if c == '\\' && (depth > 0 || quoted) {
			escaped = true
			if depth == 0 {
				out.WriteRune(c)
			}
			continue
		}
		if depth > 0 {
			if c == '(' {
				depth++
			}
			if c == ')' {
				depth--
			}
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
		if !quoted && c == '(' {
			depth++
			out.WriteByte(' ')
			continue
		}
		if quoted && c == ';' {
			return "", false
		}
		out.WriteRune(c)
	}
	return out.String(), depth == 0 && !quoted && !escaped
}
