package emailtags

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		name     string
		change   MessageTagChange
		provider string
		fail     bool
	}{
		{"keywords case overlap", MessageTagChange{Add: []string{"Work"}, Remove: []string{"work"}}, "imap", true},
		{"Gmail distinct IDs", MessageTagChange{Add: []string{"Label_A"}, Remove: []string{"Label_a"}}, "gmail", false},
		{"empty", MessageTagChange{}, "gmail", true},
		{"blank", MessageTagChange{Add: []string{""}}, "gmail", true},
		{"limit", MessageTagChange{Add: slices.Repeat([]string{"Next"}, 101)}, "gmail", true},
		{"provider owns length", MessageTagChange{Add: []string{strings.Repeat("界", 256)}}, "gmail", false},
		{"Unicode category", MessageTagChange{Add: []string{strings.Repeat("界", 255)}}, "msmail", false},
		{"category length", MessageTagChange{Add: []string{strings.Repeat("界", 256)}}, "msmail", true},
		{"category comma", MessageTagChange{Add: []string{"Next,Later"}}, "msmail", true},
		{"category overlap", MessageTagChange{Add: []string{"Next"}, Remove: []string{"NEXT"}}, "msmail", true},
		{"keyword length", MessageTagChange{Add: []string{strings.Repeat("x", 256)}}, "imap", true},
		{"keyword atom", MessageTagChange{Add: []string{"two words"}}, "imap", true},
		{"keyword system flag", MessageTagChange{Add: []string{"\\Seen"}}, "imap", true},
		{"keyword Unicode", MessageTagChange{Add: []string{"界"}}, "imap", true},
		{"invalid UTF-8", MessageTagChange{Add: []string{string([]byte{255})}}, "gmail", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := Normalize(tc.change, tc.provider)
			if tc.fail {
				require.Error(err)
			} else {
				require.NoError(err)
			}
		})
	}
	require := require.New(t)
	got, err := Normalize(MessageTagChange{Add: []string{"Work", "work", "Next"}}, "imap")
	require.NoError(err)
	assert.Equal([]string{"Work", "Next"}, got.Add)
	add, remove := Delta([]string{"work", "other"}, MessageTagChange{Add: []string{"Work", "Next"}, Remove: []string{"absent"}}, true)
	assert.Equal([]string{"Next"}, add)
	assert.Empty(remove)
	assert.True(Verify([]string{"WORK", "Next", "other"}, MessageTagChange{Add: []string{"Work", "Next"}, Remove: []string{"absent"}}, true))
	assert.False(Verify([]string{"Work"}, MessageTagChange{Remove: []string{"work"}}, true))
}
