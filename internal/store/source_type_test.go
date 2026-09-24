package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestEffectiveSourceType(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "legacy Gmail", raw: "", want: "gmail"},
		{name: "explicit Gmail", raw: "gmail", want: "gmail"},
		{name: "IMAP", raw: "imap", want: "imap"},
		{name: "named provider", raw: "mbox", want: "mbox"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, store.EffectiveSourceType(tt.raw))
		})
	}
}
