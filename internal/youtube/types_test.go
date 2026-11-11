package youtube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSourceIdentifier(t *testing.T) {
	for _, id := range []string{"UCfixture", "https://evil.example/channel/UCfixture", "https://www.youtube.com/channel/UCfixture?token=x", "https://www.youtube.com/channel/"} {
		_, ok := ParseSourceIdentifier(id)
		require.False(t, ok, id)
	}
	id, ok := ParseSourceIdentifier("https://www.youtube.com/channel/UCfixture")
	require.True(t, ok)
	require.Equal(t, "UCfixture", id)
}
