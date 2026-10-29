package beeperidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderKeyRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cases := []struct{ service, scopeKind, scopeValue, native string }{
		{"telegram", "service", "", "482111"},
		{"telegram", "account", "acct-7", "482111"},
		{"matrix", "server", "beeper.local", "@alice:beeper.local"},
		{"slack", "workspace", "T0123456789", "U0123456789"},
		{"telegram", "account", "a:b", "482111"}, // a delimiter inside a component stays one component
		{"", "", "", ""},
	}
	for _, c := range cases {
		key := ProviderKey(c.service, c.scopeKind, c.scopeValue, c.native)
		service, scopeKind, scopeValue, native, ok := DecodeProviderKey(key)
		require.True(ok, "decode %q", key)
		assert.Equal(c.service, service, "service for %q", key)
		assert.Equal(c.scopeKind, scopeKind, "scope kind for %q", key)
		assert.Equal(c.scopeValue, scopeValue, "scope value for %q", key)
		assert.Equal(c.native, native, "native id for %q", key)
	}
}

func TestDecodeProviderKeyRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"provider:",
		"beeper:7:acct-7:6:482111",
		"provider:8:telegram",
		"provider:x:telegram:7:service:0::6:482111",
		"provider:99:telegram:7:service:0::6:482111",
		"provider:8:telegram:7:service:0::6:482111:extra",
		"provider:8:telegram7:service:0::6:482111",
	}
	for _, key := range bad {
		_, _, _, _, ok := DecodeProviderKey(key)
		assert.False(t, ok, "%q must not decode", key)
	}
}

// TestTelegramIdentityCanonicalForms pins the one accepted form of a
// Telegram identity: digits-only native IDs, the service-scoped key they
// encode to, and that shape's recognition. The bridge, the Telegram export
// importer, and the archive migration all share these functions so one
// person cannot fork into two identities.
func TestTelegramIdentityCanonicalForms(t *testing.T) {
	assert := assert.New(t)

	assert.True(NumericTelegramID("482111"))
	assert.True(NumericTelegramID("7000000000000555555"), "19-digit ids are digits")
	for _, bad := range []string{"", "+482111", "-1", "48 111", "48-111", "482111x", "０"} {
		assert.False(NumericTelegramID(bad), "%q is not a bare decimal id", bad)
	}

	assert.Equal("provider:8:telegram:7:service:0::6:482111", TelegramStableProviderKey("482111"))
	assert.Empty(TelegramStableProviderKey("+482111"), "a signed id has no shared key")
	assert.Empty(TelegramStableProviderKey(""), "an empty id has no shared key")

	assert.True(TelegramSharedProviderKey(TelegramStableProviderKey("482111")))
	assert.False(TelegramSharedProviderKey(""), "empty is not the shared form")
	assert.False(TelegramSharedProviderKey(
		ProviderKey("telegram", "account", "acct-7", "482111")), "account scoping is not the shared form")
	assert.False(TelegramSharedProviderKey(
		ProviderKey("discord", "service", "", "482111")), "another service is not the shared form")
	// A non-canonical encoding of the same components (leading-zero length)
	// decodes but does not equal the canonical key, so it is not recognized.
	assert.False(TelegramSharedProviderKey("provider:08:telegram:7:service:0::6:482111"))
}
