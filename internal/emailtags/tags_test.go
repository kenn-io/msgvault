package emailtags

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	assert := assert.New(t)
	for _, tc := range []struct {
		name   string
		change Change
		fold   bool
		fail   bool
	}{
		{"keywords case overlap", Change{Add: []string{"Work"}, Remove: []string{"work"}}, true, true},
		{"Gmail distinct IDs", Change{Add: []string{"Label_A"}, Remove: []string{"Label_a"}}, false, false},
		{"empty", Change{}, false, true},
		{"blank", Change{Add: []string{""}}, false, true},
		{"limit", Change{Add: make([]string, 101)}, false, true},
		{"length", Change{Add: []string{strings.Repeat("x", 256)}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := Normalize(tc.change, tc.fold)
			if tc.fail {
				require.Error(err)
			} else {
				require.NoError(err)
			}
		})
	}
	require := require.New(t)
	got, err := Normalize(Change{Add: []string{"Work", "work", "Next"}}, true)
	require.NoError(err)
	assert.Equal([]string{"Work", "Next"}, got.Add)
	add, remove := Delta([]string{"work", "other"}, Change{Add: []string{"Work", "Next"}, Remove: []string{"absent"}}, true)
	assert.Equal([]string{"Next"}, add)
	assert.Empty(remove)
	assert.True(Verify([]string{"WORK", "Next", "other"}, Change{Add: []string{"Work", "Next"}, Remove: []string{"absent"}}, true))
	assert.False(Verify([]string{"Work"}, Change{Remove: []string{"work"}}, true))
}
