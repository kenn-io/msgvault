package personfacts

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare host", raw: "  WWW.Example.COM. ", want: "example.com"},
		{name: "https URL", raw: "https://www.Example.com:443/careers?q=go", want: "example.com"},
		{name: "URL query containing email", raw: "https://www.Example.com/search?q=user@other.example", want: "example.com"},
		{name: "URL userinfo", raw: "https://user:secret@www.Example.com/path", want: "example.com"},
		{name: "email", raw: "Person@Example.COM", want: "example.com"},
		{name: "unicode URL host", raw: "https://www.bücher.example/jobs", want: "xn--bcher-kva.example"},
		{name: "unicode email host", raw: "person@BÜCHER.example", want: "xn--bcher-kva.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeDomain(test.raw)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}

	for _, raw := range []string{"", "localhost", "example.com/path", "https://", "user@", "exa mple.com", "http://[::1"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			got, err := NormalizeDomain(raw)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}
