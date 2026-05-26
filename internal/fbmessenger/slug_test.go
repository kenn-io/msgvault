package fbmessenger

import (
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Test User", "test.user"},
		{"Marie-Ève Côté", "marie.eve.cote"},
		{"  Alice  ", "alice"},
		{"alice@example.com", "alice.example.com"},
		{"小明", ""},
		{"", ""},
	}
	for _, c := range cases {
		assertpkg.Equal(t, c.want, Slug(c.in), "Slug(%q)", c.in)
	}
}

func TestAddressFallbackForEmptySlug(t *testing.T) {
	assert := assertpkg.New(t)
	a := Address("小明")
	assert.True(strings.HasPrefix(a.Email, "user.") && strings.HasSuffix(a.Email, "@facebook.messenger"),
		"unexpected fallback email: %q", a.Email)
	assert.Equal(a.Email, Address("小明").Email, "fallback must be deterministic across calls")
	assert.Equal("facebook.messenger", a.Domain)
	assert.Equal("小明", a.Name, "display name must be preserved unaltered")
}

func TestAddressRegular(t *testing.T) {
	a := Address("Test User")
	assertpkg.Equal(t, "test.user@facebook.messenger", a.Email)
	assertpkg.Equal(t, "Test User", a.Name)
	assertpkg.Equal(t, "facebook.messenger", a.Domain)
}

func TestDecodeMojibake(t *testing.T) {
	assert := assertpkg.New(t)
	// "é" (U+00E9) encoded as UTF-8 is bytes 0xC3 0xA9. Interpreted as
	// Latin-1, those are runes U+00C3 U+00A9, which Facebook then emits
	// as JSON. DecodeMojibake must reverse that.
	in := "cafÃ©"
	assert.Equal("café", DecodeMojibake(in), "DecodeMojibake(%q)", in)
	// Non-Latin-1 input must round-trip unchanged.
	assert.Equal("正常", DecodeMojibake("正常"), "non-Latin-1 round-trip")
	// ASCII round-trips unchanged.
	assert.Equal("hello", DecodeMojibake("hello"), "ascii round-trip")
	// Already-valid UTF-8 with Latin-1-range code points must be preserved.
	// "café" has é = U+00E9, which is <= 0xFF, so the old code would
	// convert it to the single byte 0xE9 (invalid UTF-8). The fix detects
	// that the converted result is not valid UTF-8 and returns the original.
	assert.Equal("café", DecodeMojibake("café"), "valid UTF-8 café preserved")
	// "naïve" has ï = U+00EF, same risk.
	assert.Equal("naïve", DecodeMojibake("naïve"), "valid UTF-8 naïve preserved")
	// "über" has ü = U+00FC.
	assert.Equal("über", DecodeMojibake("über"), "valid UTF-8 über preserved")
}

func TestStripDomain(t *testing.T) {
	assertpkg.Equal(t, "test.user", StripDomain("test.user@facebook.messenger"))
	assertpkg.Equal(t, "test.user", StripDomain("test.user"))
}
