package discord

import (
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
)

func TestNativeDiscordOverwriteDenyPrecedence(t *testing.T) {
	readBits := nativeDiscordViewChannel | nativeDiscordReadHistory
	mask := func(bits uint64) string { return strconv.FormatUint(bits, 10) }

	tests := []struct {
		name       string
		base       uint64
		roles      []string
		overwrites []PermissionOverwrite
		want       bool
	}{
		{
			name: "allow follows deny in everyone overwrite",
			base: readBits,
			overwrites: []PermissionOverwrite{{
				ID: "200", Type: 0, Allow: mask(nativeDiscordViewChannel), Deny: mask(nativeDiscordViewChannel),
			}},
			want: true,
		},
		{
			name:  "allow wins across conflicting role overwrites",
			roles: []string{"201", "202"},
			overwrites: []PermissionOverwrite{
				{ID: "201", Type: 0, Allow: mask(readBits), Deny: "0"},
				{ID: "202", Type: 0, Allow: "0", Deny: mask(nativeDiscordViewChannel)},
			},
			want: true,
		},
		{
			name:  "allow follows deny in member overwrite",
			base:  readBits,
			roles: []string{"201"},
			overwrites: []PermissionOverwrite{{
				ID: "101", Type: 1, Allow: mask(nativeDiscordReadHistory), Deny: mask(nativeDiscordReadHistory),
			}},
			want: true,
		},
		{
			name:  "role deny removes a base permission without an allow",
			base:  readBits,
			roles: []string{"201"},
			overwrites: []PermissionOverwrite{{
				ID: "201", Type: 0, Allow: "0", Deny: mask(nativeDiscordViewChannel),
			}},
			want: false,
		},
		{
			name:  "member allow overrides role deny",
			base:  nativeDiscordReadHistory,
			roles: []string{"201"},
			overwrites: []PermissionOverwrite{
				{ID: "201", Type: 0, Allow: "0", Deny: mask(nativeDiscordViewChannel)},
				{ID: "101", Type: 1, Allow: mask(nativeDiscordViewChannel), Deny: "0"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := Assert.New(t)
			authority := nativeDiscordAuthority{
				permission: store.NativeSourcePermission{NativeIdentity: "200", Known: true, MessageRead: true},
				member:     NativeGuildMember{User: User{ID: "101"}, Roles: tt.roles},
				base:       tt.base,
			}

			assert.Equal(tt.want, authority.canRead(Channel{PermissionOverwrites: tt.overwrites}))
		})
	}
}
