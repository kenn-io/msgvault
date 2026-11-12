package beeperidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserID(t *testing.T) {
	for _, tc := range []struct {
		name, account, key, want string
		ok                       bool
	}{
		{"scoped", "account:a", Fallback("account:a", "user:雪"), "user:雪", true},
		{"other account", "account:b", Fallback("account:a", "user"), "", false},
		{"legacy", "account", Fallback("", "user"), "", false},
		{"noncanonical length", "a", "beeper:01:a:1:b", "", false},
		{"truncated", "a", "beeper:1:a:4:b", "", false},
		{"trailing", "a", Fallback("a", "b") + ":x", "", false},
		{"empty account", "", Fallback("", "user"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UserID(tc.account, tc.key)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.ok, ok)
		})
	}
}
