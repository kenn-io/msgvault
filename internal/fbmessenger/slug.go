package fbmessenger

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wesm/msgvault/internal/mime"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Domain is the synthetic domain used for all Facebook Messenger participants.
const Domain = "facebook.messenger"

// Slug normalizes a Facebook display name into a deterministic ASCII slug
// usable as the local-part of a synthetic email address. Diacritics are
// stripped via NFKD + combining-mark removal; runs of non-alphanumerics
// collapse to a single dot; leading and trailing dots are trimmed.
// Returns an empty string when no ASCII-foldable letters or digits remain.
func Slug(name string) string {
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	folded, _, err := transform.String(t, name)
	if err != nil {
		folded = name
	}
	var b strings.Builder
	prevDot := true
	for _, r := range folded {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
			prevDot = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDot = false
		default:
			if !prevDot {
				b.WriteByte('.')
				prevDot = true
			}
		}
	}
	return strings.Trim(b.String(), ".")
}

// Address returns a deterministic synthetic mime.Address for the given
// Facebook display name. The name is preserved verbatim; the email
// local-part is Slug(name), falling back to "user.<8-hex-sha1>" when the
// slug is empty so callers never see an address without a local-part.
func Address(name string) mime.Address {
	local := Slug(name)
	if local == "" {
		sum := sha1.Sum([]byte(name))
		local = "user." + hex.EncodeToString(sum[:4])
	}
	return mime.Address{
		Name:   name,
		Email:  local + "@" + Domain,
		Domain: Domain,
	}
}

// DecodeMojibake reverses Facebook DYI JSON's well-known Latin-1-over-UTF-8
// encoding. Facebook's JSON exporter writes UTF-8 bytes as if they were
// Latin-1 code points, so "café" (UTF-8: 0x63 0x61 0x66 0xC3 0xA9) becomes
// the JSON string "caf\u00c3\u00a9". This function re-interprets those
// Latin-1 code points as raw bytes and checks whether the result is valid
// UTF-8. If not (or if the input contains runes above U+00FF), the original
// string is returned unchanged.
func DecodeMojibake(s string) string {
	buf := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return s
		}
		buf = append(buf, byte(r))
	}
	decoded := string(buf)
	if !utf8.ValidString(decoded) {
		return s
	}
	return decoded
}

// StripDomain returns the local part of a synthetic "<slug>@facebook.messenger"
// email. If the input lacks the expected domain suffix, it is returned as-is.
func StripDomain(email string) string {
	suffix := "@" + Domain
	if strings.HasSuffix(email, suffix) {
		return strings.TrimSuffix(email, suffix)
	}
	return email
}
