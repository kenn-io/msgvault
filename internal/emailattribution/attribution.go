// Package emailattribution decides which confirmed account received or sent
// one email. It accepts confirmed candidates; it never discovers ownership or
// authorizes sending.
package emailattribution

import (
	"bufio"
	"bytes"
	"fmt"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
)

// MaxHeaderBytes bounds the outer header block read from stored MIME.
const MaxHeaderBytes = 256 * 1024

var (
	originalHeaders  = []string{"X-Gm-Original-To", "X-Delivered-To", "X-Original-To"}
	deliveredHeaders = []string{"Delivered-To", "X-Resolved-To", "X-Original-Delivered-To"}
)

// Headers holds the delivery addresses read from one message's outer headers.
// Original names the address the message was first sent to before forwarding;
// Delivered names mailboxes the message passed through on delivery.
type Headers struct {
	Original  []string
	Delivered []string
}

// ParseHeaders reads delivery addresses from an outer header block. A value
// that is not a valid address list is skipped whole and reported through the
// malformed flag; other values still count. A block that is not a header block
// at all returns an error.
func ParseHeaders(block []byte) (Headers, bool, error) {
	header, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(block))).ReadMIMEHeader()
	if err != nil {
		return Headers{}, false, fmt.Errorf("read header block: %w", err)
	}
	var h Headers
	malformed := false
	collect := func(names []string) []string {
		var out []string
		for _, name := range names {
			for _, value := range header[textproto.CanonicalMIMEHeaderKey(name)] {
				list, err := mail.ParseAddressList(value)
				if err != nil {
					malformed = true
					continue
				}
				for _, a := range list {
					address := strings.ToLower(strings.TrimSpace(a.Address))
					if strings.Contains(address, "@") {
						out = append(out, address)
					}
				}
			}
		}
		return out
	}
	h.Original = collect(originalHeaders)
	h.Delivered = collect(deliveredHeaders)
	return h, malformed, nil
}

// Evidence is everything Attribute compares against confirmed candidates.
// Visible holds confirmed To/Cc matches; Sender holds confirmed sender matches.
// NoSentFolder marks a source without Sent folders, such as an mbox import,
// whose sent copies carry no provider direction.
type Evidence struct {
	Original, Delivered, Visible, Sender []string
	NoSentFolder                         bool
}

// Result is the attributed account address, or "" when no single confirmed
// address wins. Sent reports mail the account wrote rather than received.
type Result struct {
	Address string
	Sent    bool
}

// Attribute returns the unique confirmed match at the strongest tier. A sent
// copy takes its unique confirmed sender and never the source default. Inbound
// mail tries the original recipient, then upstream delivery addresses, then
// visible recipients, then the final inbox. In a source without Sent folders,
// mail without any of that evidence and with a confirmed sender is a sent
// copy; anything else takes the source default. More than one match at a
// tier returns "" without trying lower tiers.
func Attribute(e Evidence, candidates []string, sink string, sent bool) Result {
	allowed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c = normalize(c); strings.Contains(c, "@") {
			allowed[c] = true
		}
	}
	sink = normalize(sink)
	// match reports whether the tier decided the result.
	var result Result
	match := func(group []string) bool {
		var found []string
		for _, v := range group {
			if v = normalize(v); allowed[v] && !slices.Contains(found, v) {
				found = append(found, v)
			}
		}
		switch len(found) {
		case 0:
			return false
		case 1:
			result.Address = found[0]
		}
		return true
	}
	if sent {
		result.Sent = true
		match(e.Sender)
		return result
	}
	// The final inbox's own server stamps its address into original-recipient
	// and delivery headers alike, so the sink counts only as the final inbox.
	original, finalOriginal := splitSink(e.Original, sink)
	upstream, finalDelivered := splitSink(e.Delivered, sink)
	if match(original) || match(upstream) || match(e.Visible) || match(append(finalOriginal, finalDelivered...)) {
		return result
	}
	if e.NoSentFolder && match(e.Sender) {
		result.Sent = true
		return result
	}
	if sink != "" && allowed[sink] {
		result.Address = sink
	}
	return result
}

// splitSink separates the addresses naming the sink from the rest.
func splitSink(addresses []string, sink string) (other, final []string) {
	for _, address := range addresses {
		if sink != "" && normalize(address) == sink {
			final = append(final, address)
		} else {
			other = append(other, address)
		}
	}
	return other, final
}

func normalize(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}
