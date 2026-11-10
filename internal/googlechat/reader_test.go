package googlechat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGoogleChatJSONBoundsAndContainment(t *testing.T) {
	for _, tc := range []string{"oversize", "symlink", "trailing"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, "Groups/DM a/messages.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(name), 0700))
			switch tc {
			case "oversize":
				f, err := os.Create(name)
				require.NoError(t, err)
				require.NoError(t, f.Truncate(65<<20))
				require.NoError(t, f.Close())
			case "symlink":
				outside := filepath.Join(t.TempDir(), "messages.json")
				require.NoError(t, os.WriteFile(outside, []byte(one), 0600))
				require.NoError(t, os.Symlink(outside, name))
			case "trailing":
				require.NoError(t, os.WriteFile(name, []byte(one+` {}`), 0600))
			}
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{root}})
			require.Error(t, err)
			assert.Zero(t, summary.MessagesAdded)
		})
	}
}

func TestGoogleChatAcceptsRootLayouts(t *testing.T) {
	for _, suffix := range []string{"", "Takeout", "Takeout/Google Chat", "Takeout/Google Chat/Groups"} {
		t.Run(suffix, func(t *testing.T) {
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{filepath.Join("testdata/takeout", suffix)}})
			require.NoError(t, err)
			assert.EqualValues(t, 4, summary.MessagesAdded)
		})
	}
}

func TestGoogleChatRootLimit(t *testing.T) {
	roots := make([]string, 129)
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: roots})
	require.ErrorContains(t, err, "128")
}

func TestGoogleChatDates(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Monday, January 18, 2021 at 1:27:33\u202fPM UTC", "2021-01-18T13:27:33Z"},
		{"Monday, January 18, 2021 at 01:27:33\u00a0AM UTC", "2021-01-18T01:27:33Z"},
		{"Monday, January 18, 2021 at 1:27:33 PM PST", ""},
		{"", ""},
		{"not a date", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := parseDate(tc.input)
			if tc.want == "" {
				assert.True(t, got.IsZero())
			} else {
				assert.Equal(t, tc.want, got.Format("2006-01-02T15:04:05Z07:00"))
			}
		})
	}
}

func FuzzGoogleChatMediaPathContainment(f *testing.F) {
	for _, seed := range []string{"File-example.txt", "files/example.txt", "../secret", "/absolute", `C:\secret`, "", "files/../../secret"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if !localMediaPath(name) {
			return
		}
		base := filepath.Join(string(filepath.Separator), "fixture", "conversation")
		joined := filepath.Join(base, name)
		relative, err := filepath.Rel(base, joined)
		require.NoError(t, err)
		assert.False(t, filepath.IsAbs(relative))
		assert.NotEqual(t, "..", relative)
		assert.False(t, strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	})
}

func TestGoogleChatMissingProfilePartKeepsOwnershipUnknown(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", one)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Users/User synthetic"), 0700))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
}
