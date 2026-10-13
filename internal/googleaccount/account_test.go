package googleaccount

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeGmailAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", "user@gmail.com"},
		{"User@Gmail.Com", "user@gmail.com"},
		{"first.last@gmail.com", "firstlast@gmail.com"},
		{"user@googlemail.com", "user@gmail.com"},
		{"f.i.r.s.t@googlemail.com", "first@gmail.com"},
		{"user+tag@gmail.com", "user@gmail.com"},
		{"user+@gmail.com", "user@gmail.com"},
		{"f.o.o+bar@googlemail.com", "foo@gmail.com"},
		{"user@example.com", ""},
		{"noatsign", ""},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			t.Parallel()
			got := normalizeGmailAddress(tt.email)
			assert.Equal(t, tt.want, got, "normalizeGmailAddress(%q)", tt.email)
		})
	}
}
