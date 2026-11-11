package peoplesweep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAppreciationContributionKey(t *testing.T) {
	key, err := AppreciationContributionKey("chat", "account", "message")
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{64}$`, key)
	for _, parts := range [][3]string{{"chat2", "account", "message"}, {"chat", "account2", "message"}, {"chat", "account", "message2"}} {
		other, err := AppreciationContributionKey(parts[0], parts[1], parts[2])
		require.NoError(t, err)
		assert.NotEqual(t, key, other)
	}
	for _, parts := range [][3]string{{"", "a", "b"}, {"a", "", "b"}, {"a", "b", ""}, {"a", "b", "\xff"}} {
		_, err := AppreciationContributionKey(parts[0], parts[1], parts[2])
		assert.Error(t, err)
	}
}

func TestAppreciationKeyIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		a, err := AppreciationContributionKey("chat", "account", "x"+value)
		require.NoError(ht, err)
		b, err := AppreciationContributionKey("chat", "account", "x"+value)
		require.NoError(ht, err)
		assert.Equal(ht, a, b)
		c, err := AppreciationContributionKey("chat", "account", "x"+value+"!")
		require.NoError(ht, err)
		assert.NotEqual(ht, a, c)
		assert.Equal(ht, 64, len(strings.TrimSpace(a)))
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func FuzzAppreciationContributionKey(f *testing.F) {
	f.Add("chat", "example", "message")
	f.Add("chat", "a\x00b", "世界")
	f.Fuzz(func(t *testing.T, source, identifier, message string) {
		key, err := AppreciationContributionKey(source, identifier, message)
		if err != nil {
			return
		}
		again, err := AppreciationContributionKey(source, identifier, message)
		require.NoError(t, err)
		assert.Equal(t, key, again)
		assert.Regexp(t, `^[0-9a-f]{64}$`, key)
	})
}
