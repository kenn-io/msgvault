package googlechat

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGoogleChatJSONBoundsAndContainment(t *testing.T) {
	for _, mode := range []string{"oversize", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			path := t.TempDir()
			name := filepath.Join(path, "messages.json")
			if mode == "oversize" {
				f, err := os.Create(name)
				require.NoError(err)
				require.NoError(f.Truncate(65 << 20))
				require.NoError(f.Close())
			} else {
				outside := filepath.Join(t.TempDir(), "messages.json")
				require.NoError(os.WriteFile(outside, []byte(one), 0600))
				require.NoError(os.Symlink(outside, name))
			}
			root, err := os.OpenRoot(path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(root.Close()) })
			_, err = readJSON(t.Context(), root, "messages.json")
			require.Error(err)
		})
	}
}
func TestGoogleChatAcceptsRootLayouts(t *testing.T) {
	for _, suffix := range []string{"", "Takeout", "Takeout/Google Chat", "Takeout/Google Chat/Groups"} {
		t.Run(suffix, func(t *testing.T) {
			reader, err := openExport(t.Context(), []string{filepath.Join("testdata/takeout", suffix)}, "owner@example.com")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			assert.Len(t, reader.groups, 2)
		})
	}
}
func TestGoogleChatRootLimit(t *testing.T) {
	_, err := openExport(t.Context(), make([]string, 129), "owner@example.com")
	require.ErrorContains(t, err, "128")
}
func TestGoogleChatReaderValidation(t *testing.T) {
	for _, tc := range []struct{ name, path, data, want string }{
		{"owner conflict", "Users/User a/user_info.json", `{"user":{"email":"other@example.com"}}`, "disagrees"},
		{"no layout", "unrelated.json", one, "layout"},
		{"unknown group", "Groups/DM_synthetic/messages.json", one, "unsupported group"},
		{"archive", "takeout.zip", "", "extract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.path, tc.data)
			roots := []string{root}
			if tc.name == "archive" {
				roots = []string{filepath.Join(root, tc.path)}
			}
			_, err := openExport(t.Context(), roots, "owner@example.com")
			require.ErrorContains(t, err, tc.want)
		})
	}
	t.Run("good and unrecognized", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		write(t, a, "Groups/DM a/messages.json", one)
		write(t, b, "unrelated.json", one)
		_, err := openExport(t.Context(), []string{a, b}, "owner@example.com")
		require.ErrorContains(t, err, "layout")
	})
	t.Run("conflicting snapshots", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		write(t, a, "Groups/DM a/messages.json", one)
		write(t, b, "Groups/DM a/messages.json", `{"messages":[]}`)
		reader, err := openExport(t.Context(), []string{a, b}, "owner@example.com")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		_, _, err = reader.groupFiles(t.Context(), "DM a", "messages.json")
		require.ErrorContains(t, err, "conflict")
	})
	t.Run("empty valid layout", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, "Groups"), 0700))
		reader, err := openExport(t.Context(), []string{root}, "owner@example.com")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		assert.Empty(t, reader.groups)
	})
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

func TestGoogleChatMissingProfilePartKeepsOwnershipUnknown(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	write(t, root, "Groups/DM a/messages.json", one)
	require.NoError(os.MkdirAll(filepath.Join(root, "Users/User synthetic"), 0700))
	_, err := Import(t.Context(), st, ImportOptions{Me: "owner@example.com", Roots: []string{root}})
	require.NoError(err)
	var config sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sync_config FROM sources`).Scan(&config))
	assert.NotContains(t, config.String, "owner_profiles")
}
