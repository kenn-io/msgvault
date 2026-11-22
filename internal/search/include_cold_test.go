package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFormatIncludeCold(t *testing.T) {
	assertions := assert.New(t)

	q := ParseOR("release include:cold | planning include:cold")
	require.NoError(t, q.Err())
	assertions.True(q.IncludeCold)
	assertions.False(q.Alternatives[0].IncludeCold)
	assertions.False(q.Alternatives[1].IncludeCold)
	assertions.NotContains(q.Alternatives[0].TextTerms, "include:cold")
	assertions.Contains(Format(q), "include:cold")
	assertions.Equal(Format(q), Format(ParseOR(Format(q))))
	assertions.Error(Parse("include:stub").Err())
}
