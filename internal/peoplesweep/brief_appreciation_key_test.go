package peoplesweep

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAppreciationContributionKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	key, err := AppreciationContributionKey("chat", "account", "message")
	require.NoError(err)
	assert.Regexp(`^[0-9a-f]{64}$`, key)
	for _, parts := range [][3]string{{"chat2", "account", "message"}, {"chat", "account2", "message"}, {"chat", "account", "message2"}} {
		other, err := AppreciationContributionKey(parts[0], parts[1], parts[2])
		require.NoError(err)
		assert.NotEqual(key, other)
	}
	for _, parts := range [][3]string{{"", "a", "b"}, {"a", "", "b"}, {"a", "b", ""}, {"a", "b", "\xff"}} {
		_, err := AppreciationContributionKey(parts[0], parts[1], parts[2])
		assert.Error(err)
	}
}

// These vectors fix the v1 JSON field order, escaping, and UTF-8 encoding.
func TestAppreciationContributionKeyV1Vectors(t *testing.T) {
	tests := []struct{ sourceType, identifier, messageID, want string }{
		{"chat", "account", "message", "9c1277d8afdfc4ed0e323abc735b283c9b0acb0c626b1b6b0e4f542b02d6e2d1"},
		{"chat", "<>&", "m", "807b5193f92660f09bcce474007b0ff622ea630c896d5f2aec08d43fd2ec6992"},
		{"chat", "\u2028\u2029", "m", "d93f28a9014d4f4b2ae93f9c630fa960dd8e1ffb1b629cfb28ada9c5b5f3b565"},
		{"聊天", "a\"b\\c\n", "雪", "9d37f96e8f1cb98c9e7b5c804cd0f40d2b71be8ac47a4effe2e24110a910a4c7"},
	}
	for _, tc := range tests {
		got, err := AppreciationContributionKey(tc.sourceType, tc.identifier, tc.messageID)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestAppreciationKeyFieldBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, err := AppreciationContributionKey("ab", "c", "m")
	require.NoError(err)
	b, err := AppreciationContributionKey("a", "bc", "m")
	require.NoError(err)
	assert.NotEqual(a, b)
	// NUL and non-ASCII coordinates remain distinct, valid identity data.
	c, err := AppreciationContributionKey("chat", "a\x00b", "世界")
	require.NoError(err)
	d, err := AppreciationContributionKey("chat", "ab", "世界")
	require.NoError(err)
	assert.NotEqual(c, d)
}

func TestAppreciationKeyIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		assert := assert.New(ht)
		value := hegel.Draw(ht, hegel.Text())
		a, err := AppreciationContributionKey("chat", "account", "x"+value)
		require.NoError(err)
		b, err := AppreciationContributionKey("chat", "account", "x"+value)
		require.NoError(err)
		assert.Equal(a, b)
		c, err := AppreciationContributionKey("chat", "account", "x"+value+"!")
		require.NoError(err)
		assert.NotEqual(a, c)
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}
