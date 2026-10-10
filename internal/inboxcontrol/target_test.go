package inboxcontrol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTargetRequiresExactSourceAndItem(t *testing.T) {
	good := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "provider-message"}
	require.NoError(t, good.Validate())
	cases := []struct {
		name   string
		change func(*Target)
	}{
		{"missing source", func(v *Target) { v.SourceID = 0 }},
		{"negative source", func(v *Target) { v.SourceID = -1 }},
		{"missing source type", func(v *Target) { v.SourceType = "" }},
		{"unknown provider", func(v *Target) { v.SourceType = "unsupported" }},
		{"missing source identifier", func(v *Target) { v.SourceIdentifier = "" }},
		{"missing account", func(v *Target) { v.AccountID = "" }},
		{"missing scope", func(v *Target) { v.Scope = "" }},
		{"unknown scope", func(v *Target) { v.Scope = "query" }},
		{"missing item", func(v *Target) { v.ItemID = 0 }},
		{"negative item", func(v *Target) { v.ItemID = -2 }},
		{"missing provider identity", func(v *Target) { v.ProviderID = "" }},
		{"query in place of identity", func(v *Target) { v.ProviderID = ""; v.ItemID = 0 }},
		{"mail chat scope", func(v *Target) { v.Scope = ScopeChat }},
		{"mail with mailbox", func(v *Target) { v.Mailbox = "INBOX" }},
		{"mail with UID", func(v *Target) { v.UID = 3 }},
		{"mail with UIDVALIDITY", func(v *Target) { v.UIDValidity = 4 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := good
			tc.change(&target)
			assert.ErrorIs(t, target.Validate(), ErrInvalid)
		})
	}
}

func TestTargetRequiresExactIMAPMembership(t *testing.T) {
	good := Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "archive-native-id", Mailbox: "INBOX", UIDValidity: 42, UID: 3}
	require.NoError(t, good.Validate())
	cases := []struct {
		name   string
		change func(*Target)
	}{
		{"missing mailbox", func(v *Target) { v.Mailbox = "" }},
		{"missing validity", func(v *Target) { v.UIDValidity = 0 }},
		{"missing UID", func(v *Target) { v.UID = 0 }},
		{"chat scope", func(v *Target) { v.Scope = ScopeChat }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := good
			tc.change(&target)
			assert.ErrorIs(t, target.Validate(), ErrInvalid)
		})
	}
	// The membership is exact even when two copies share the same Message-ID.
	other := good
	other.UID = 4
	require.NoError(t, other.Validate())
	assert.NotEqual(t, good, other)
}

func TestTargetSeparatesBeeperChatIdentity(t *testing.T) {
	good := Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "beeper-installation", AccountID: "synthetic-account", Scope: ScopeChat, ItemID: 2, ProviderID: "!synthetic:example.com"}
	require.NoError(t, good.Validate())
	for _, change := range []func(*Target){
		func(v *Target) { v.Scope = ScopeMessage },
		func(v *Target) { v.Mailbox = "INBOX" },
		func(v *Target) { v.UID = 3 },
		func(v *Target) { v.UIDValidity = 4 },
	} {
		target := good
		change(&target)
		assert.ErrorIs(t, target.Validate(), ErrInvalid)
	}
}

func TestTargetRejectsUnsafeIdentityStrings(t *testing.T) {
	good := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "provider-message"}
	for _, bad := range []string{" ", "\x00", "message\n", "\xff", strings.Repeat("x", 4097)} {
		for _, field := range []string{"source", "account", "provider"} {
			t.Run(field, func(t *testing.T) {
				target := good
				switch field {
				case "source":
					target.SourceIdentifier = bad
				case "account":
					target.AccountID = bad
				case "provider":
					target.ProviderID = bad
				}
				assert.ErrorIs(t, target.Validate(), ErrInvalid)
			})
		}
	}
	// Diagnostics must not echo untrusted identifiers or account data.
	target := good
	target.AccountID = "sensitive\nvalue"
	err := target.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), target.AccountID)
}
