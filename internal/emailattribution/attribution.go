// Package emailattribution derives an archive account from outer email headers.
// It accepts confirmed candidates; it never discovers ownership or authorizes sending.
package emailattribution

import (
	"bytes"
	"net/mail"
	"slices"
	"strings"
)

const MaxHeaderBytes = 256 * 1024

// Evidence is compact, re-evaluable header evidence. Addresses not yet confirmed
// remain here so a new identity can target only messages mentioning it.
type Evidence struct {
	Original   []string `json:"original,omitempty"`
	Delivery   []string `json:"delivery,omitempty"`
	Visible    []string `json:"visible,omitempty"`
	From       []string `json:"from,omitempty"`
	Diagnostic []string `json:"diagnostic,omitempty"`
	Malformed  bool     `json:"malformed,omitempty"`
}

type Result struct {
	Address string
	Path    string
	Basis   string
}

// Parse reads only the outer header block, independently of MIME body validity.
func Parse(raw []byte) Evidence {
	end := len(raw)
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(raw, sep); i >= 0 && i+len(sep) < end {
			end = i + len(sep)
		}
	}
	if end > MaxHeaderBytes {
		return Evidence{Malformed: true}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw[:end]))
	if err != nil {
		return Evidence{Malformed: true}
	}
	var e Evidence
	for _, item := range []struct {
		names []string
		dst   *[]string
	}{
		{[]string{"X-Gm-Original-To", "X-Delivered-To", "X-Original-To"}, &e.Original},
		{[]string{"Delivered-To", "X-Resolved-To", "X-Original-Delivered-To"}, &e.Delivery},
		{[]string{"To", "Cc"}, &e.Visible},
		{[]string{"From"}, &e.From},
		{[]string{"Bcc", "X-Forwarded-To", "X-Forwarded-For"}, &e.Diagnostic},
	} {
		for _, name := range item.names {
			for _, value := range msg.Header[canonical(name)] {
				addresses, err := mail.ParseAddressList(value)
				if err != nil {
					e.Malformed = true
					continue
				}
				for _, a := range addresses {
					address := strings.ToLower(strings.TrimSpace(a.Address))
					if address != "" && strings.Contains(address, "@") {
						*item.dst = append(*item.dst, address)
					}
				}
			}
		}
	}
	return e
}

func canonical(name string) string {
	parts := strings.Split(strings.ToLower(name), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

func (e Evidence) Mentions() []string {
	var all []string
	for _, group := range [][]string{e.Original, e.Delivery, e.Visible, e.From, e.Diagnostic} {
		all = append(all, group...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// Attribute returns a unique match at the strongest usable tier. The sink is
// deferred behind visible recipients when it is only a final delivery address.
func Attribute(e Evidence, candidates []string, sink string, sent bool) Result {
	path := "inbound"
	if sent {
		path = "sent"
	}
	result := Result{Path: path}
	allowed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		address, err := mail.ParseAddress(c)
		if err == nil && address.Address == c && strings.Contains(c, "@") {
			allowed[strings.ToLower(c)] = true
		}
	}
	sink = strings.ToLower(sink)
	match := func(group []string, basis string) bool {
		matches := make(map[string]bool)
		for _, v := range group {
			if allowed[v] {
				matches[v] = true
			}
		}
		if len(matches) == 0 {
			return false
		}
		if len(matches) > 1 {
			result.Basis = "ambiguous"
			return true
		}
		for address := range matches {
			result.Address = address
		}
		result.Basis = basis
		return true
	}
	if sent {
		from := e.From
		if len(from) > 1 {
			from = slices.Clone(from)
			slices.Sort(from)
			from = slices.Compact(from)
		}
		if len(from) > 1 {
			result.Basis = "ambiguous"
			return result
		}
		if match(from, "sent-from") {
			return result
		}
		result.Basis = "unconfirmed-sender"
		if len(from) == 0 {
			result.Basis = "missing-sender"
		}
		return result
	}
	if match(e.Original, "original-recipient") {
		return result
	}
	var upstream, final []string
	for _, address := range e.Delivery {
		if sink != "" && address == sink {
			final = append(final, address)
		} else {
			upstream = append(upstream, address)
		}
	}
	if match(upstream, "delivery-chain") || match(e.Visible, "recipient-headers") || match(final, "final-inbox") {
		return result
	}
	if allowed[sink] && sink != "" {
		result.Address = sink
		result.Basis = "source-default"
		return result
	}
	result.Basis = "missing-evidence"
	if e.Malformed {
		result.Basis = "malformed-evidence"
	}
	return result
}
