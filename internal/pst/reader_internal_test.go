package pst

import (
	"path/filepath"
	"testing"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWalkFoldersRecursive_SearchFolderRecord(t *testing.T) {
	t.Run("search folder is skipped", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		folder := pstlib.Folder{
			Identifier:   1827,
			Name:         "All Messages",
			MessageCount: 2178,
		}
		var got []FolderEntry

		err := walkFoldersRecursive(&folder, "ROOT_FOLDER/Search Root", func(entry FolderEntry, _ *pstlib.Folder) error {
			got = append(got, entry)
			return nil
		})
		require.NoError(err)
		assert.Empty(got)

		_, err = folder.GetMessageIterator()
		require.ErrorIs(err, pstlib.ErrMessagesNotFound)
	})

	t.Run("normal folder with same name and count is visited", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		folder := pstlib.Folder{
			Identifier:   1826,
			Name:         "All Messages",
			MessageCount: 2178,
		}
		var got []FolderEntry

		err := walkFoldersRecursive(&folder, "ROOT_FOLDER/Search Root", func(entry FolderEntry, _ *pstlib.Folder) error {
			got = append(got, entry)
			return nil
		})
		require.NoError(err)
		require.Len(got, 1)
		assert.Equal("ROOT_FOLDER/Search Root/All Messages", got[0].Path)
		assert.Equal(int32(2178), got[0].MsgCount)
	})
}

func TestWalkFolders_MatchesLibraryTraversalWithoutSearchFolders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, err := Open(filepath.Join("testdata", "support.pst"))
	require.NoError(err, "Open")
	defer func() { _ = f.Close() }()

	var got []pstlib.Identifier
	err = f.WalkFolders(func(_ FolderEntry, folder *pstlib.Folder) error {
		got = append(got, folder.Identifier)
		return nil
	})
	require.NoError(err, "WalkFolders")

	var all, want []pstlib.Identifier
	err = f.pstFile.WalkFolders(func(folder *pstlib.Folder) error {
		all = append(all, folder.Identifier)
		if folder.Identifier.GetType() != pstlib.IdentifierTypeSearchFolder {
			want = append(want, folder.Identifier)
		}
		return nil
	})
	require.NoError(err, "go-pst WalkFolders")
	require.Less(len(want), len(all))
	assert.Equal(want, got)
}
